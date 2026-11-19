package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/cache"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/version"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
type fakeGit struct{ branch, top string }

func (f fakeGit) Run(args ...string) (string, error) {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir") && has("--path-format=absolute"):
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		if f.top != "" {
			return f.top, nil
		}
		return "/repo", nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T, branch string) *App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := Open(Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: branch},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestOpenResolvesIdentityAndStore(t *testing.T) {
	a := newTestApp(t, "main")

	if a.Git.Branch != "main" {
		t.Errorf("branch = %q, want main", a.Git.Branch)
	}
	if a.Git.RepoID == "" || a.Git.RepoName != "repo" {
		t.Errorf("git identity not resolved: %+v", a.Git)
	}
	if a.Engine == nil || a.Ledger == nil || a.Store == nil {
		t.Fatal("app components not wired")
	}

	// Sanity: the engine writes through the opened store.
	if _, _, _, err := a.Engine.Record(context.Background(), newLearning()); err != nil {
		t.Fatalf("engine record failed: %v", err)
	}

	// Open writes repo metadata.
	var name string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key='repo_name'`).Scan(&name); err != nil {
		t.Fatalf("repo_name meta not written: %v", err)
	}
	if name != "repo" {
		t.Errorf("repo_name = %q, want repo", name)
	}
}

func newLearning() memory.NewMemory {
	return memory.NewMemory{Kind: memory.KindLearning, Branch: "main", Body: "use wal mode for cross process writers"}
}

func TestOpenBuildsGraphEngine(t *testing.T) {
	a := newTestApp(t, "main")
	if a.Graph == nil {
		t.Fatal("App.Graph is nil")
	}
}

func TestOpenContextInterruptsTheQuickCheck(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
	t.Setenv("RECARTO_WORKSPACE", "")
	cfg := Config{
		CWD:        "/repo",
		Runner:     fakeGit{branch: "main"},
		Clock:      clock.NewFake(time.UnixMilli(1_700_000_000_000)),
		QuickCheck: true,
	}
	_, gc, _, err := resolvePrimary(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := cache.StorePath(gc.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, err := OpenContext(ctx, cfg)
	if err == nil {
		t.Fatalf("OpenContext with a cancelled ctx succeeded (close: %v)", a.Close())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenContext with a cancelled ctx = %v, want context.Canceled", err)
	}
	// store.quickCheckStamp and store.quickCheckLock name these files; this
	// package cannot reach them.
	if _, err := os.Stat(path + ".quickcheck"); !os.IsNotExist(err) {
		t.Errorf("stamp after an interrupted check: %v, want none", err)
	}
	want := fmt.Sprintf("pid=%d at=", os.Getpid())
	if b, err := os.ReadFile(path + ".quickcheck.lock"); err != nil || !strings.HasPrefix(string(b), want) {
		t.Errorf("lock file after an interrupted check = %q (%v), want a trace starting %q", b, err, want)
	}
}

// openFromMainThenWorktree opens the store first from the main checkout (/repo), the
// first open writing the identity, and returns it with an Open from a second
// worktree (/wt/b) of the same repo. before runs between the two opens.
func openFromMainThenWorktree(t *testing.T, before func(a *App)) (*App, *App) {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_WORKSPACE", "")
	t.Setenv("RECARTO_REPOS", "")
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	a, err := Open(Config{CWD: "/repo", Runner: fakeGit{branch: "main"}, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	before(a)
	b, err := Open(Config{CWD: "/wt/b", Runner: fakeGit{branch: "b", top: "/wt/b"}, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if b.Git.RepoID != a.Git.RepoID || b.Git.RepoName != "b" {
		t.Fatalf("worktree open: id %q name %q, want id %q name b", b.Git.RepoID, b.Git.RepoName, a.Git.RepoID)
	}
	return a, b
}

// TestOpenFromAnotherWorktreeTakesNoWriteLock: worktrees of one repo share a
// store but each has its own directory name, so an open from another worktree
// must not count the name as drift — it opens while another process holds the
// write lock, which an open that wrote could not.
func TestOpenFromAnotherWorktreeTakesNoWriteLock(t *testing.T) {
	ctx := context.Background()
	openFromMainThenWorktree(t, func(a *App) {
		blocker, err := sql.Open("sqlite", "file:"+a.Store.Path())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = blocker.Close() })
		bc, err := blocker.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bc.Close() })
		if _, err := bc.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = bc.ExecContext(ctx, "ROLLBACK") })
	})
}

// TestOpenWritesIdentityOnVersionChange: a real identity change is still
// written, whichever worktree opens the store.
func TestOpenWritesIdentityOnVersionChange(t *testing.T) {
	ctx := context.Background()
	_, b := openFromMainThenWorktree(t, func(a *App) {
		if _, err := a.Store.DB.ExecContext(ctx,
			`UPDATE meta SET value='0.0.0-old' WHERE key='recarto_version'`); err != nil {
			t.Fatal(err)
		}
	})
	var v string
	if err := b.Store.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='recarto_version'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != version.Version {
		t.Errorf("recarto_version = %q, want %q", v, version.Version)
	}
}
