package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gerwim/recarto/internal/store"
)

// FreshState classifies what a query can expect from the stored graph right now.
type FreshState int

const (
	// Fresh: the graph matches the worktree token; results are current.
	Fresh FreshState = iota
	// Refreshing: a background refresh is in flight (or recently failed);
	// existing, slightly stale data is available to serve.
	Refreshing
	// FirstIndex: the worktree has no indexed data yet and its first index is
	// in flight; queries come up empty until it lands.
	FirstIndex
)

func (s FreshState) String() string {
	switch s {
	case Fresh:
		return "fresh"
	case Refreshing:
		return "refreshing"
	case FirstIndex:
		return "first-index"
	}
	return fmt.Sprintf("FreshState(%d)", int(s))
}

// Freshness is EnsureFresh's report: the state plus a human-readable detail
// (progress, or the failure of the last attempt) for surfacing to the caller.
type Freshness struct {
	State  FreshState
	Detail string
}

// refreshFailureCooldown is how long after a failed background refresh a new
// attempt may start. Without it every query against a persistently failing
// worktree would relaunch a full reconcile attempt.
const refreshFailureCooldown = 30 * time.Second

// refreshRun is one background reconcile, shared by every caller that observes
// it (singleflight per worktree). err and finishedAt are written before done
// closes and only read after it, or under the engine's freshMu.
type refreshRun struct {
	done       chan struct{}
	err        error
	finishedAt time.Time // zero while running
	prog       *refreshProgress
}

// refreshProgress adapts the Progress callbacks into an atomically readable
// status line for callers polling an in-flight refresh.
type refreshProgress struct {
	total, indexed atomic.Int64
	phase          atomic.Value // string
}

// Start also clears the phase: a "waiting for another process" label must not
// outlive the wait, since detail prefers the phase.
func (p *refreshProgress) Start(total int) {
	p.total.Store(int64(total))
	p.phase.Store("")
}
func (p *refreshProgress) Inc()           { p.indexed.Add(1) }
func (p *refreshProgress) Phase(l string) { p.phase.Store(l) }
func (p *refreshProgress) Done()          {}

func (p *refreshProgress) detail() string {
	if l, _ := p.phase.Load().(string); l != "" {
		return l
	}
	if t := p.total.Load(); t > 0 {
		return fmt.Sprintf("%d/%d files indexed", p.indexed.Load(), t)
	}
	return "scanning worktree"
}

// EnsureFresh is the non-blocking, bounded form of query-time freshness. A
// matching token returns Fresh immediately (three point lookups, microseconds).
// Otherwise it joins — or starts, at most one per worktree per process; across processes
// ReconcileIfStale's lock makes a second one wait — a
// background reconcile DETACHED from ctx, so a caller's timeout or cancellation
// can never abort indexing mid-flight, waits up to wait for it, and otherwise
// reports honestly what the caller can serve in the meantime: Refreshing when
// stale data exists, FirstIndex when nothing is indexed yet. After a failed
// attempt no new one starts for refreshFailureCooldown; the failure rides along
// in Detail.
func (e *Engine) EnsureFresh(ctx context.Context, worktree string, src Source, token string, wait time.Duration) Freshness {
	return e.ensureFresh(ctx, worktree, src, token, func() time.Duration { return wait })
}

// EnsureFreshBy is EnsureFresh waiting until deadline instead of for a duration:
// the bound is applied where the wait starts, so callers sharing one deadline
// never wait past it.
func (e *Engine) EnsureFreshBy(ctx context.Context, worktree string, src Source, token string, deadline time.Time) Freshness {
	return e.ensureFresh(ctx, worktree, src, token, func() time.Duration {
		return max(time.Until(deadline), 0)
	})
}

func (e *Engine) ensureFresh(ctx context.Context, worktree string, src Source, token string, wait func() time.Duration) Freshness {
	// A lost mark only lets the sweep drop this graph sooner, and the next query
	// re-indexes it; a query must never fail over it. NoteUsed writes only an
	// hour-old mark, so most queries take no write lock.
	_ = e.NoteUsed(ctx, worktree)
	if e.graphFresh(ctx, worktree, token) {
		return Freshness{State: Fresh}
	}
	r := e.startOrJoinRefresh(ctx, worktree, src, token)

	// A completed failed run must win even when a shared deadline has expired.
	select {
	case <-r.done:
		return e.refreshResult(ctx, worktree, token, r)
	default:
	}
	d := wait()
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.done:
		return e.refreshResult(ctx, worktree, token, r)
	case <-timer.C:
	case <-ctx.Done():
	}
	return e.staleStatus(ctx, worktree, r.prog.detail())
}

func (e *Engine) refreshResult(ctx context.Context, worktree, token string, r *refreshRun) Freshness {
	if r.err != nil {
		return e.staleStatus(ctx, worktree, "graph refresh failed: "+r.err.Error())
	}
	// An empty token (worktree outside git) never matches anything, so a
	// completed refresh is freshness.
	if token == "" || e.graphFresh(ctx, worktree, token) {
		return Freshness{State: Fresh}
	}
	return e.staleStatus(ctx, worktree, "worktree changed during refresh")
}

// graphFresh reports whether worktree's stored graph already matches token and
// carries no interrupted-recovery debt (pending resolve marker, missing
// secondary indexes). Shared by EnsureFresh and ReconcileIfStale.
func (e *Engine) graphFresh(ctx context.Context, worktree, token string) bool {
	if token == "" {
		return false
	}
	s, err := readIndexState(ctx, e.st.DB, worktree)
	return err == nil && s.finished() && s.token == token
}

// indexState is what the store records about a worktree's index.
type indexState struct {
	token     string // stamped by the last finished reconcile; "" when none
	pending   bool   // a reconcile left committed writes unresolved
	indexesOK bool   // every graph secondary index exists
	built     bool   // outside git: a reconcile finished once (builtKey)
}

// finished reports whether the stored graph is the output of a finished reconcile.
func (s indexState) finished() bool { return s.token != "" && !s.pending && s.indexesOK }

// readIndexState reads worktree's index state in one round trip on q; graphFresh
// runs it on every graph tool call (~65µs on the bench box; see BenchmarkGraphFresh).
func readIndexState(ctx context.Context, q store.Conn, worktree string) (indexState, error) {
	args := make([]any, 0, len(graphSecondaryIndexes)+3)
	args = append(args, graphTokenKey+worktree, resolvePendingKey(worktree), builtKey(worktree))
	ph := make([]string, len(graphSecondaryIndexes))
	for i, ix := range graphSecondaryIndexes {
		args = append(args, ix.name)
		ph[i] = "?"
	}
	var s indexState
	var indexes int
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT value FROM meta WHERE key=?1), ''),
		        EXISTS(SELECT 1 FROM meta WHERE key=?2),
		        EXISTS(SELECT 1 FROM meta WHERE key=?3),
		        (SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN (`+
			strings.Join(ph, ",")+`))`, args...).
		Scan(&s.token, &s.pending, &s.built, &indexes)
	s.indexesOK = indexes == len(graphSecondaryIndexes)
	return s, err
}

// codeMapDone reports whether s, read on q, is a finished index the code map
// may be built from. Outside git no token is ever stamped, so there a graph
// with no recovery debt counts when it has files (an index of an earlier
// version left no mark) or the built mark. A finished index of an earlier state
// of the worktree counts too: it is stale, not partial.
func codeMapDone(ctx context.Context, q store.Conn, worktree string, nonGit bool, s indexState) (bool, error) {
	if !nonGit {
		return s.finished(), nil
	}
	if s.pending || !s.indexesOK {
		return false, nil
	}
	if s.built {
		return true, nil
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM files WHERE worktree=? LIMIT 1`, worktree).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// startOrJoinRefresh returns the worktree's in-flight background refresh,
// starting one when none exists. A finished failed run is returned as-is (its
// done channel is already closed) until the cooldown elapses, so callers see
// the failure without relaunching the work.
func (e *Engine) startOrJoinRefresh(ctx context.Context, worktree string, src Source, token string) *refreshRun {
	e.freshMu.Lock()
	defer e.freshMu.Unlock()
	if e.refreshes == nil {
		e.refreshes = map[string]*refreshRun{}
	}
	if r, ok := e.refreshes[worktree]; ok {
		if r.finishedAt.IsZero() || e.clk.Now().Sub(r.finishedAt) < refreshFailureCooldown {
			return r
		}
	}
	r := &refreshRun{done: make(chan struct{}), prog: &refreshProgress{}}
	e.refreshes[worktree] = r
	// Detached from the caller: an MCP client timing out and cancelling its
	// request must not kill the index run it triggered.
	bg := context.WithoutCancel(ctx)
	go func() {
		err := e.ReconcileIfStale(bg, worktree, src, token, WithProgress(r.prog))
		e.freshMu.Lock()
		r.err = err
		r.finishedAt = e.clk.Now()
		if err == nil {
			delete(e.refreshes, worktree) // success: nothing to remember
		}
		e.freshMu.Unlock()
		close(r.done)
	}()
	return r
}

// staleStatus reports what a caller can serve while the graph is not fresh:
// existing data (Refreshing) or nothing yet (FirstIndex).
func (e *Engine) staleStatus(ctx context.Context, worktree, detail string) Freshness {
	var one int
	err := e.st.DB.QueryRowContext(ctx,
		`SELECT 1 FROM files WHERE worktree=? LIMIT 1`, worktree).Scan(&one)
	if err == nil {
		return Freshness{State: Refreshing, Detail: detail}
	}
	return Freshness{State: FirstIndex, Detail: detail}
}

// PresentFiles returns up to n of the files worktree's graph stores that are
// still on disk, and whether the graph stores any file. It reads n rows and
// stats each, so a caller can check a few known code files first instead of
// listing the whole worktree.
func (e *Engine) PresentFiles(ctx context.Context, worktree string, n int) (present []string, listed bool, err error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT path FROM files WHERE worktree=? LIMIT ?`, worktree, n)
	if err != nil {
		return nil, false, err
	}
	// rows.Err reports a failed read; Close only frees the cursor.
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, false, err
		}
		listed = true
		if info, err := os.Stat(filepath.Join(worktree, filepath.FromSlash(p))); err == nil && !info.IsDir() {
			present = append(present, p)
		}
	}
	return present, listed, rows.Err()
}
