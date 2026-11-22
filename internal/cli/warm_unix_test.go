//go:build !windows

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/gitwrap"
	"github.com/gerwim/recarto/internal/graph"
)

// TestWarmLimitHelper is the `recarto warm` process TestWarmStopsAtItsLimit
// runs. Its limit fires when SIGUSR1 arrives, unless Run stopped it first.
func TestWarmLimitHelper(t *testing.T) {
	if os.Getenv("RECARTO_WARM_LIMIT_HELPER") != "1" {
		t.Skip("run by TestWarmStopsAtItsLimit")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	armWarmLimit = func(_ time.Duration, f func()) func() bool {
		// 0 armed, 1 stopped, 2 fired: whichever of stop and the signal claims
		// it first wins, as with time.Timer.
		var state atomic.Int32
		go func() {
			<-sig
			if state.CompareAndSwap(0, 2) {
				f()
			}
		}()
		return func() bool { return state.CompareAndSwap(0, 1) }
	}
	_ = Run([]string{"warm"}, io.Discard)
	t.Fatal("warm returned instead of being stopped")
}

// TestWarmStopsAtItsLimit: a warm run stuck in git ends at its limit, and the
// next run completes the index.
func TestWarmStopsAtItsLimit(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo, map[string]string{"a.go": "package p\n\nfunc A() {}\n"})
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO", "RECARTO_REPOS"} {
		t.Setenv(k, "")
	}
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	// The fake git fires the limit once the run is inside the index, then hangs.
	fakeDir := gitwrap.Script(t, `for a in "$@"; do if [ "$a" = ls-files ]; then kill -USR1 $PPID; exec sleep 3600; fi; done`)

	cmd := exec.Command(os.Args[0], "-test.run=^TestWarmLimitHelper$")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "RECARTO_WARM_LIMIT_HELPER=1", "PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reaps the orphaned sleep along with the helper.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(stderr.String(), "stopped after") {
			t.Fatalf("helper = %v, stderr %q; want a non-zero exit saying it stopped", err, stderr.String())
		}
	case <-time.After(time.Minute):
		t.Fatal("warm was not stopped at its limit")
	}

	a, err := app.Open(app.Config{CWD: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()
	if err := Warm(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	_, st, err := a.Graph.CodeMap(ctx, a.Git.Worktree, false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Error("index unfinished after the next warm")
	}
	if got := filesIn(t, a, a.Git.Worktree); got != 1 {
		t.Errorf("indexed files = %d, want 1", got)
	}
}

// heldSource lists only once gate closes, so its reconcile holds the
// worktree's index lock until then.
type heldSource struct {
	graph.Source
	listing chan struct{}
	gate    chan struct{}
}

func (h heldSource) List() ([]graph.FileEntry, error) {
	close(h.listing)
	<-h.gate
	return h.Source.List()
}

// TestWarmLeavesAWorktreeAnotherProcessIndexes: while another process indexes
// the primary worktree and holds the store's write lock, warm neither waits
// nor writes there, and still indexes the served repos and runs its sweep.
func TestWarmLeavesAWorktreeAnotherProcessIndexes(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	initGitRepo(t, primary, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	initGitRepo(t, extra, map[string]string{"lib/b.go": "package lib\n\nfunc B() {}\n"})
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO"} {
		t.Setenv(k, "")
	}
	t.Setenv("RECARTO_REPOS", extra)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	open := func() *app.App {
		a, err := app.Open(app.Config{CWD: primary, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	a, a2 := open(), open()
	ctx := context.Background()
	wt := a.Git.Worktree

	// A warm that read the busy worktree's git state before trying its index
	// lock would run this git status; the shim records that instead of hanging.
	eager := filepath.Join(t.TempDir(), "eager-status")
	hook := `if [ "$1" = -C ] && [ "$2" = '` + wt + `' ] && [ "$3" = status ]; then : > '` + eager + `'; exit 1; fi`
	t.Setenv("PATH", gitwrap.Script(t, hook)+string(os.PathListSeparator)+os.Getenv("PATH"))

	held := heldSource{Source: graph.NewSource(wt), listing: make(chan struct{}), gate: make(chan struct{})}
	indexed := make(chan error, 1)
	go func() { indexed <- a2.Graph.ReconcileIfStale(ctx, wt, held, "tok") }()
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(held.gate) }) }
	t.Cleanup(openGate)
	awaitSignal(t, "the other process never began indexing", held.listing)

	blocker, err := sql.Open("sqlite", "file:"+a.Store.Path()+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	bconn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Runs before the pool's Close: no handle stays open for the temp dir's removal.
	t.Cleanup(func() { _ = bconn.Close() })
	if _, err := bconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("blocker BEGIN IMMEDIATE: %v", err)
	}
	awaitNil(t, "warm with the worktree busy", func() error { return Warm(ctx, a, nil) })
	repos := a.AllRepos()
	if len(repos) != 2 {
		t.Fatalf("served repos = %d, want 2", len(repos))
	}
	var served int
	if err := repos[1].Store.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE worktree=?`, repos[1].Git.Worktree).Scan(&served); err != nil {
		t.Fatal(err)
	}
	if served != 1 {
		t.Errorf("served repo files = %d, want 1", served)
	}
	if got := filesIn(t, a, wt); got != 0 {
		t.Errorf("busy worktree files = %d, want 0: warm must leave it", got)
	}

	if _, err := bconn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, "/definitely/gone-wt", graph.FSSource{Root: primary}); err != nil {
		t.Fatal(err)
	}
	awaitNil(t, "warm after the store lock is free", func() error { return Warm(ctx, a, nil) })
	if got := filesIn(t, a, "/definitely/gone-wt"); got != 0 {
		t.Errorf("dead worktree files = %d, want 0: warm's sweep must run", got)
	}
	if got := filesIn(t, a, wt); got != 0 {
		t.Errorf("busy worktree files = %d, want 0: warm must leave it", got)
	}

	if _, err := os.Stat(eager); err == nil {
		t.Error("warm ran git status for the busy worktree before trying its index lock")
	}

	openGate()
	awaitNil(t, "the other process's index", func() error { return <-indexed })
}

// awaitSignal waits for ch to close, failing only on a hang.
func awaitSignal(t *testing.T, msg string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Minute):
		t.Fatal(msg)
	}
}

// awaitNil runs f in a goroutine and fails on an error or a hang.
func awaitNil(t *testing.T, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(time.Minute):
		t.Fatalf("%s hung", what)
	}
}
