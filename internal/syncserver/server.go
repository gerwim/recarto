// Package syncserver is the server half of Recarto's HTTP memory sync: a tiny
// pure-Go net/http service, shipped in the same recarto binary (`recarto
// server`), that lets machines working the same git repo converge their
// memories. It holds one SQLite store per repo key (the same schema a client
// uses — the server is, structurally, a headless recarto store) and applies
// incoming records through memory.SyncApply, the identical merge core the
// client uses to apply what it pulls. See internal/remotesync for the client
// half and internal/syncwire for the shared wire format.
package syncserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// maxPushBytes bounds a push request body. Sync payloads are text-only memory
// rows (never the code graph), so a repo with thousands of memories is well
// under this; it exists to bound a hostile or malfunctioning client, not to
// constrain legitimate use.
const maxPushBytes = 32 << 20 // 32MB

// keyPattern restricts repo keys to a safe filesystem-path segment. Keys are
// either sha256(RootCommit)[:16] (hex) or an operator-chosen RECARTO_SYNC_REPO
// override; either way this is the only guard between an HTTP path segment
// and a directory join, so it must reject traversal ("..", "/") outright.
var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// repoStore bundles one repo key's opened store with its engine.
type repoStore struct {
	st  *store.Store
	eng *memory.Engine
}

// defaultRateLimit / defaultRateBurst are the per-IP token bucket
// parameters: steady-state sync traffic is a handful of small requests per
// machine, so this is generous headroom for legitimate use while still
// bounding a hostile or malfunctioning client.
const (
	defaultRateLimit = 20.0 // tokens/sec
	defaultRateBurst = 60.0
)

// Server holds the static auth key, the data directory root, and a lazily
// populated, mutex-guarded cache of per-repo-key stores.
type Server struct {
	key     string
	dataDir string
	clk     clock.Clock
	limiter *rateLimiter

	mu     sync.Mutex
	stores map[string]*repoStore
}

// New returns a Server. key must be non-empty (callers must refuse to start
// otherwise — see cli.ServerCmd). dataDir is created on first use.
func New(key, dataDir string) *Server {
	return &Server{
		key: key, dataDir: dataDir, clk: clock.Real{}, stores: map[string]*repoStore{},
		limiter: newRateLimiter(defaultRateLimit, defaultRateBurst),
	}
}

// Handler builds the routed, authenticated HTTP handler. Middleware order
// (outermost first): rate limit -> auth -> gzip response -> the handler
// itself. Rate limiting runs before auth so it also throttles repeated wrong-
// key guesses; gzip response wrapping runs innermost so it wraps exactly the
// bytes the handler writes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/repos/{key}/changes", s.rateLimit(s.requireKey(withGzipResponse(s.handleGetChanges))))
	mux.HandleFunc("POST /v1/repos/{key}/changes", s.rateLimit(s.requireKey(withGzipResponse(s.handlePostChanges))))
	mux.HandleFunc("POST /v1/import", s.rateLimit(s.requireKey(s.handleImport)))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// Close closes every opened per-repo store.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, rs := range s.stores {
		if err := rs.st.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var errBadRepoKey = errors.New("syncserver: invalid repo key")

// storeFor lazily opens (or returns the cached) store for repoKey.
func (s *Server) storeFor(repoKey string) (*repoStore, error) {
	if !keyPattern.MatchString(repoKey) {
		return nil, errBadRepoKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs, ok := s.stores[repoKey]; ok {
		return rs, nil
	}
	path := filepath.Join(s.dataDir, repoKey, "store.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("repo %s: %w", repoKey, err)
	}
	// The sync server is long-lived and holds each repo store open for its whole
	// lifetime, so it takes the proactive corruption check on first open — the
	// same reasoning as `recarto serve`.
	st, err := store.Open(path, store.WithQuickCheck())
	if err != nil {
		return nil, fmt.Errorf("repo %s: open store: %w", repoKey, err)
	}
	if err := EnsureSyncSchema(st.DB); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("repo %s: ensure sync schema: %w", repoKey, err)
	}
	// sync_index is server-only bookkeeping that store.Repair deliberately does
	// not salvage, so a store that was rebuilt after corruption comes back with
	// its memories but no index — and would then serve empty pulls forever.
	// Reindexing anything missing here makes that self-healing.
	reindexed, err := reconcileSyncIndex(context.Background(), st)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("repo %s: reconcile sync index: %w", repoKey, err)
	}
	if reindexed > 0 {
		log.Printf("sync: reindexed %d record(s) into %s's sync index", reindexed, repoKey)
	}
	rs := &repoStore{st: st, eng: memory.New(st, ledger.New(st), s.clk)}
	s.stores[repoKey] = rs
	log.Printf("sync: opened store %s", repoKey)
	return rs, nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// bumpSyncIndexTx records that repoKey's canonical row id changed, assigning
// it the next monotonic seq. It must run on the SAME store.Conn (the same
// open WriteTx) as the memory.SyncApply call that produced the change —
// memory.Engine.SyncApplyTx's afterApply hook is what wires that up — so the
// two commit together or neither does. A prior version ran this in its own,
// separate WriteTx after SyncApply's had already committed; a crash or error
// between the two left the memory applied but never indexed for future
// pulls, with no way to recover it (a client re-push of the identical record
// is a genuine SyncNoop, since the memory side already matches).
func bumpSyncIndexTx(ctx context.Context, c store.Conn, id string, deleted bool, updatedAt int64) (int64, error) {
	if _, err := c.ExecContext(ctx,
		`INSERT INTO counters(scope, metric, value) VALUES ('sync', 'seq', 1)
		 ON CONFLICT(scope, metric) DO UPDATE SET value = value + 1`); err != nil {
		return 0, err
	}
	var seq int64
	if err := c.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE scope='sync' AND metric='seq'`).Scan(&seq); err != nil {
		return 0, err
	}
	deletedInt := 0
	if deleted {
		deletedInt = 1
	}
	_, err := c.ExecContext(ctx,
		`INSERT INTO sync_index(memory_id, seq, deleted, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(memory_id) DO UPDATE SET
		   seq = excluded.seq, deleted = excluded.deleted, updated_at = excluded.updated_at`,
		id, seq, deletedInt, updatedAt)
	return seq, err
}
