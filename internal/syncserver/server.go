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

// Server holds the static auth key, the data directory root, and a lazily
// populated, mutex-guarded cache of per-repo-key stores.
type Server struct {
	key     string
	dataDir string
	clk     clock.Clock

	mu     sync.Mutex
	stores map[string]*repoStore
}

// New returns a Server. key must be non-empty (callers must refuse to start
// otherwise — see cli.ServerCmd). dataDir is created on first use.
func New(key, dataDir string) *Server {
	return &Server{key: key, dataDir: dataDir, clk: clock.Real{}, stores: map[string]*repoStore{}}
}

// Handler builds the routed, authenticated HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/repos/{key}/changes", s.requireKey(s.handleGetChanges))
	mux.HandleFunc("POST /v1/repos/{key}/changes", s.requireKey(s.handlePostChanges))
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
	st, err := store.Open(path)
	if err != nil {
		return nil, fmt.Errorf("repo %s: open store: %w", repoKey, err)
	}
	if err := EnsureSyncSchema(st.DB); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("repo %s: ensure sync schema: %w", repoKey, err)
	}
	rs := &repoStore{st: st, eng: memory.New(st, ledger.New(st), s.clk)}
	s.stores[repoKey] = rs
	return rs, nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// bumpSyncIndex records that repoKey's canonical row id changed, assigning it
// the next monotonic seq. It runs in its own transaction, separate from the
// SyncApply transaction that produced the change: store.WriteTx's write mutex
// is not reentrant (the same constraint memory.Record's ledger bump documents),
// so "apply the memory change" and "index it for future pulls" are
// necessarily two commits, not one atomic unit. A crash between them leaves
// the memory applied but not yet indexed — self-healing, since the next push
// of the same (now-idempotent) record is a genuine SyncNoop only for OTHER
// peers who already saw the seq; the pusher's own outbox entry is only
// cleared after the HTTP response succeeds, so a client retries until both
// steps have happened at least once.
func bumpSyncIndex(ctx context.Context, st *store.Store, id string, deleted bool, updatedAt int64) (int64, error) {
	var seq int64
	err := st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES ('sync', 'seq', 1)
			 ON CONFLICT(scope, metric) DO UPDATE SET value = value + 1`); err != nil {
			return err
		}
		if err := c.QueryRowContext(ctx,
			`SELECT value FROM counters WHERE scope='sync' AND metric='seq'`).Scan(&seq); err != nil {
			return err
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
		return err
	})
	return seq, err
}
