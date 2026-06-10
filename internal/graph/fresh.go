package graph

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
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

func (p *refreshProgress) Start(total int) { p.total.Store(int64(total)) }
func (p *refreshProgress) Inc()            { p.indexed.Add(1) }
func (p *refreshProgress) Phase(l string)  { p.phase.Store(l) }
func (p *refreshProgress) Done()           {}

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
// Otherwise it joins — or starts, at most one per worktree per process — a
// background reconcile DETACHED from ctx, so a caller's timeout or cancellation
// can never abort indexing mid-flight, waits up to wait for it, and otherwise
// reports honestly what the caller can serve in the meantime: Refreshing when
// stale data exists, FirstIndex when nothing is indexed yet. After a failed
// attempt no new one starts for refreshFailureCooldown; the failure rides along
// in Detail.
func (e *Engine) EnsureFresh(ctx context.Context, worktree string, src Source, token string, wait time.Duration) Freshness {
	if e.graphFresh(ctx, worktree, token) {
		return Freshness{State: Fresh}
	}
	r := e.startOrJoinRefresh(ctx, worktree, src, token)

	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.done:
		if r.err != nil {
			return e.staleStatus(ctx, worktree, "graph refresh failed: "+r.err.Error())
		}
		// The refresh completed for the token it started with; the worktree may
		// have moved again meanwhile, so report Fresh only if it still holds.
		// An empty token (worktree outside git) never matches anything, so there
		// a completed refresh IS freshness.
		if token == "" || e.graphFresh(ctx, worktree, token) {
			return Freshness{State: Fresh}
		}
		return e.staleStatus(ctx, worktree, "worktree changed during refresh")
	case <-timer.C:
	case <-ctx.Done():
	}
	return e.staleStatus(ctx, worktree, r.prog.detail())
}

// graphFresh reports whether worktree's stored graph already matches token and
// carries no interrupted-recovery debt (pending resolve marker, missing
// secondary indexes). Shared by EnsureFresh and ReconcileIfStale. This runs on
// every graph tool call, so all three checks share one round trip (~65µs on
// the bench box); see BenchmarkGraphFresh.
func (e *Engine) graphFresh(ctx context.Context, worktree, token string) bool {
	if token == "" {
		return false
	}
	args := make([]any, 0, len(graphSecondaryIndexes)+2)
	args = append(args, graphTokenKey+worktree, resolvePendingKey(worktree))
	ph := make([]string, len(graphSecondaryIndexes))
	for i, ix := range graphSecondaryIndexes {
		args = append(args, ix.name)
		ph[i] = "?"
	}
	var stored string
	var pending bool
	var indexes int
	err := e.st.DB.QueryRowContext(ctx,
		`SELECT (SELECT COALESCE(value, '') FROM meta WHERE key=?1),
		        EXISTS(SELECT 1 FROM meta WHERE key=?2),
		        (SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN (`+
			strings.Join(ph, ",")+`))`, args...).
		Scan(&stored, &pending, &indexes)
	return err == nil && stored == token && !pending && indexes == len(graphSecondaryIndexes)
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
