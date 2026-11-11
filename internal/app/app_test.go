package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/memory"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
type fakeGit struct{ branch string }

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
