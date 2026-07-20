package graph

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// gatedSource blocks List until its gate closes, simulating a long first index
// or refresh, and counts List calls to observe singleflight behavior.
type gatedSource struct {
	inner Source
	gate  chan struct{}
	lists atomic.Int64
}

func (g *gatedSource) List() ([]FileEntry, error) {
	g.lists.Add(1)
	<-g.gate
	return g.inner.List()
}
func (g *gatedSource) Read(p string) ([]byte, error) { return g.inner.Read(p) }

// TestRefreshProgressReceivesResolveSubPhase proves the live resolve sub-phase
// labels flow into refreshProgress (the adapter behind the MCP freshness hint), not
// just the base label — so a client polling an in-flight refresh sees which resolve
// stage is running. Driving Resolve through a real refreshProgress, detail() must
// end on the last sub-phase and stay under the "resolving references" prefix.
func TestRefreshProgressReceivesResolveSubPhase(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.IndexFile(ctx, "/wt", "a.go", []byte("package p\nfunc A(){ B() }"), 1, 24); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "b.go", []byte("package p\nfunc B(){}"), 1, 19); err != nil {
		t.Fatal(err)
	}
	prog := &refreshProgress{}
	if err := e.Resolve(ctx, "/wt", prog); err != nil {
		t.Fatal(err)
	}
	got := prog.detail()
	if !strings.HasPrefix(got, "resolving references") {
		t.Fatalf("refreshProgress detail = %q, want a 'resolving references' sub-phase", got)
	}
	if got == "resolving references" {
		t.Fatalf("refreshProgress detail stuck on the base label %q; sub-phases did not reach it", got)
	}
}

// errSource fails every List, simulating a persistently broken refresh.
type errSource struct{ lists atomic.Int64 }

func (e *errSource) List() ([]FileEntry, error) {
	e.lists.Add(1)
	return nil, errors.New("boom: worktree unreadable")
}
func (e *errSource) Read(string) ([]byte, error) { return nil, errors.New("boom") }

// awaitFresh joins the in-flight background refresh (or confirms freshness) and
// fails the test if the graph does not become Fresh within a generous bound.
func awaitFresh(t *testing.T, e *Engine, wt string, src Source, token string) {
	t.Helper()
	f := e.EnsureFresh(context.Background(), wt, src, token, 10*time.Second)
	if f.State != Fresh {
		t.Fatalf("EnsureFresh state = %v (detail %q), want Fresh", f.State, f.Detail)
	}
}

func TestEnsureFreshTokenMatchIsCheapNoReconcile(t *testing.T) {
	e := newTestEngine(t)
	src := &countingSource{inner: crossFileCallSource()}
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	f := e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 0)
	if f.State != Fresh {
		t.Fatalf("state = %v, want Fresh", f.State)
	}
	if src.lists != 1 {
		t.Fatalf("List calls = %d, want 1 (fresh token must not reconcile)", src.lists)
	}
}

func TestEnsureFreshFirstIndexRunsInBackground(t *testing.T) {
	e := newTestEngine(t)
	src := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}

	f := e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 20*time.Millisecond)
	if f.State != FirstIndex {
		t.Fatalf("state = %v, want FirstIndex (no data yet, refresh still running)", f.State)
	}

	close(src.gate)
	awaitFresh(t, e, "/wt", src, "tok-1")
	if got := resolvedCallEdges(t, e, "/wt"); got == 0 {
		t.Fatal("background first index did not resolve edges")
	}
}

func TestEnsureFreshSingleflightJoinsInFlightRefresh(t *testing.T) {
	e := newTestEngine(t)
	src := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}

	f1 := e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 10*time.Millisecond)
	f2 := e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 10*time.Millisecond)
	if f1.State != FirstIndex || f2.State != FirstIndex {
		t.Fatalf("states = %v, %v, want FirstIndex, FirstIndex", f1.State, f2.State)
	}
	if n := src.lists.Load(); n != 1 {
		t.Fatalf("List calls = %d, want 1 (second caller must join, not start a second reconcile)", n)
	}

	close(src.gate)
	awaitFresh(t, e, "/wt", src, "tok-1")
	if n := src.lists.Load(); n != 1 {
		t.Fatalf("List calls after completion = %d, want 1", n)
	}
}

func TestEnsureFreshServesStaleWhileRefreshing(t *testing.T) {
	e := newTestEngine(t)
	mem := crossFileCallSource()
	if err := e.ReconcileIfStale(context.Background(), "/wt", mem, "tok-1"); err != nil {
		t.Fatal(err)
	}

	mem.files["c.go"] = "package p\n\nfunc C() {}\n"
	src := &gatedSource{inner: mem, gate: make(chan struct{})}
	f := e.EnsureFresh(context.Background(), "/wt", src, "tok-2", 20*time.Millisecond)
	if f.State != Refreshing {
		t.Fatalf("state = %v, want Refreshing (stale data exists and is servable)", f.State)
	}

	close(src.gate)
	awaitFresh(t, e, "/wt", src, "tok-2")
	if got := countRows(t, e, "files", "/wt"); got != 3 {
		t.Fatalf("files = %d, want 3 after background refresh", got)
	}
}

// TestEnsureFreshDetachedFromCallerContext is the core production property: the
// caller's cancellation (an MCP client timeout) must NOT abort the background
// reconcile.
func TestEnsureFreshDetachedFromCallerContext(t *testing.T) {
	e := newTestEngine(t)
	src := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	f := e.EnsureFresh(ctx, "/wt", src, "tok-1", 10*time.Millisecond)
	if f.State != FirstIndex {
		t.Fatalf("state = %v, want FirstIndex", f.State)
	}
	cancel() // the client gives up; the index must keep going

	close(src.gate)
	awaitFresh(t, e, "/wt", src, "tok-1")
	if got := resolvedCallEdges(t, e, "/wt"); got == 0 {
		t.Fatal("reconcile died with the caller's context: background refresh must be detached")
	}
}

// BenchmarkGraphFresh guards the per-query freshness fast path: every MCP graph
// tool call pays this check when the token matches, so it must stay at
// microsecond point-lookup cost (token + pending marker + index presence).
func BenchmarkGraphFresh(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "store.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	e, err := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	if err := e.ReconcileIfStale(context.Background(), "/wt", crossFileCallSource(), "tok-1"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if !e.graphFresh(context.Background(), "/wt", "tok-1") {
			b.Fatal("graph should be fresh")
		}
	}
}

// TestEnsureFreshEmptyTokenReportsFreshOnCompletion: a worktree outside git has
// no freshness token (WorktreeToken returns ""), so there is nothing to compare
// after the refresh lands — a successful refresh IS fresh, and must not be
// misreported as "worktree changed".
func TestEnsureFreshEmptyTokenReportsFreshOnCompletion(t *testing.T) {
	e := newTestEngine(t)
	src := crossFileCallSource()
	f := e.EnsureFresh(context.Background(), "/wt", src, "", 10*time.Second)
	if f.State != Fresh {
		t.Fatalf("state = %v (detail %q), want Fresh after a completed refresh with no token", f.State, f.Detail)
	}
}

// TestEnsureFreshFailureCooldown: a failing refresh must not be restarted by
// every query — that would re-burn a full reconcile attempt per tool call.
func TestEnsureFreshFailureCooldown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	e, err := New(st, ledger.New(st), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)

	src := &errSource{}
	f := e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 5*time.Second)
	if f.State != FirstIndex {
		t.Fatalf("state = %v, want FirstIndex", f.State)
	}
	if !strings.Contains(f.Detail, "boom") {
		t.Fatalf("detail %q should carry the refresh failure", f.Detail)
	}
	if n := src.lists.Load(); n != 1 {
		t.Fatalf("List calls = %d, want 1", n)
	}

	// Immediately after the failure: no restart.
	_ = e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 5*time.Second)
	if n := src.lists.Load(); n != 1 {
		t.Fatalf("List calls = %d, want 1 (failure cooldown must prevent a restart)", n)
	}

	// After the cooldown: retry allowed.
	clk.Advance(refreshFailureCooldown + time.Second)
	_ = e.EnsureFresh(context.Background(), "/wt", src, "tok-1", 5*time.Second)
	if n := src.lists.Load(); n != 2 {
		t.Fatalf("List calls = %d, want 2 (cooldown elapsed, retry expected)", n)
	}
}
