package graph

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/store"
)

// hangTimeout only catches a hang; no passing test waits it out.
const hangTimeout = time.Minute

// twoEngines opens two engines on two handles of one store file: two processes
// sharing the store. flock and LockFileEx exclude separate opens in one process.
func twoEngines(t *testing.T) (*Engine, *Engine) {
	t.Helper()
	return twoEnginesOn(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
}

// twoEnginesOn is twoEngines on clk, which both engines share as two processes
// on one machine share its clock.
func twoEnginesOn(t *testing.T, clk *clock.Fake) (*Engine, *Engine) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	open := func() *Engine {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		e, err := New(st, ledger.New(st), clk)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(e.Close)
		return e
	}
	return open(), open()
}

// waitFor polls cond until it holds, failing with msg after hangTimeout.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitErr waits for a result on ch, failing on a hang or a non-nil error.
func awaitErr(t *testing.T, what string, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(hangTimeout):
		t.Fatalf("%s hung", what)
	}
}

func TestReconcileIfStaleOneIndexerPerWorktree(t *testing.T) {
	e1, e2 := twoEngines(t)
	ctx := context.Background()
	src1 := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}
	done1 := make(chan error, 1)
	go func() { done1 <- e1.ReconcileIfStale(ctx, "/wt", src1, "tok") }()
	waitFor(t, "first reconcile never listed", func() bool { return src1.lists.Load() == 1 })

	src2 := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}
	close(src2.gate)
	prog := &refreshProgress{}
	done2 := make(chan error, 1)
	go func() { done2 <- e2.ReconcileIfStale(ctx, "/wt", src2, "tok", WithProgress(prog)) }()
	waitFor(t, "second reconcile never waited", func() bool {
		if src2.lists.Load() != 0 {
			t.Fatal("second process indexed concurrently")
		}
		return prog.detail() == waitingForIndex
	})

	close(src1.gate)
	awaitErr(t, "first reconcile", done1)
	awaitErr(t, "second reconcile", done2)
	if n := src2.lists.Load(); n != 0 {
		t.Errorf("second process listed %d times, want 0: the first one indexed this state", n)
	}
	runs, err := e2.led.Get(ctx, "lifetime", "graph_index_runs")
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Errorf("graph_index_runs = %d, want 1", runs)
	}
}

func TestReconcileIfStaleOtherWorktreeNotBlocked(t *testing.T) {
	e1, e2 := twoEngines(t)
	ctx := context.Background()
	src1 := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}
	done1 := make(chan error, 1)
	go func() { done1 <- e1.ReconcileIfStale(ctx, "/wtA", src1, "tok") }()
	defer func() {
		close(src1.gate)
		awaitErr(t, "reconcile of wtA", done1)
	}()
	waitFor(t, "reconcile of wtA never listed", func() bool { return src1.lists.Load() == 1 })

	done2 := make(chan error, 1)
	go func() { done2 <- e2.ReconcileIfStale(ctx, "/wtB", crossFileCallSource(), "tok") }()
	awaitErr(t, "reconcile of wtB while wtA indexes", done2)
	var n int
	if err := e2.st.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE worktree='/wtB'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wtB files = %d, want 2", n)
	}
}

// TestReconcileIfIdleLeavesABusyWorktree: while another process indexes the
// worktree, stuck reading its git state, and the store's write lock is held,
// ReconcileIfIdle returns ErrIndexing without reading the token or writing; a
// write would fail with SQLITE_BUSY.
func TestReconcileIfIdleLeavesABusyWorktree(t *testing.T) {
	e1, e2 := twoEngines(t)
	ctx := context.Background()
	inToken, gate := make(chan struct{}), make(chan struct{})
	token1 := func(string) string {
		close(inToken)
		<-gate
		return "tok"
	}
	done1 := make(chan error, 1)
	go func() { done1 <- e1.ReconcileIfIdle(ctx, "/wt", crossFileCallSource(), token1) }()
	select {
	case <-inToken:
	case <-time.After(hangTimeout):
		t.Fatal("first reconcile never read its token")
	}
	release := holdWriteLock(t, e2)

	src2 := &gatedSource{inner: crossFileCallSource(), gate: make(chan struct{})}
	close(src2.gate)
	token2 := func(string) string {
		t.Error("token read while another process indexes the worktree")
		return "tok"
	}
	done2 := make(chan error, 1)
	go func() { done2 <- e2.ReconcileIfIdle(ctx, "/wt", src2, token2) }()
	select {
	case err := <-done2:
		if !errors.Is(err, ErrIndexing) {
			t.Fatalf("idle reconcile = %v, want ErrIndexing", err)
		}
	case <-time.After(hangTimeout):
		t.Fatal("idle reconcile hung")
	}
	if n := src2.lists.Load(); n != 0 {
		t.Errorf("idle reconcile listed %d times, want 0", n)
	}

	release()
	close(gate)
	awaitErr(t, "first reconcile", done1)
}
