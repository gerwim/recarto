package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/graph"
)

// worktreeListGit extends fakeGit with a `git worktree list --porcelain`
// answer naming the given live paths.
type worktreeListGit struct {
	fakeGit
	live []string
}

func (g worktreeListGit) Run(args ...string) (string, error) {
	if args[0] == "worktree" {
		var b strings.Builder
		for _, p := range g.live {
			b.WriteString("worktree " + p + "\nHEAD abc\n\n")
		}
		return b.String(), nil
	}
	return g.fakeGit.Run(args...)
}

// TestDeadWorktrees: a stored worktree is dead only when git no longer lists
// it AND its path is gone from disk — both, so a porcelain/path-form mismatch
// (symlinks) can never condemn a live worktree.
func TestDeadWorktrees(t *testing.T) {
	liveDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := Open(Config{
		CWD:    "/repo",
		Runner: worktreeListGit{fakeGit: fakeGit{branch: "main"}, live: []string{liveDir}},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(liveDir, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Index the live worktree, a deleted one (rows labeled with a path that no
	// longer exists), and an unlisted-but-present one (e.g. porcelain printed a
	// different path form for it).
	if err := a.Graph.Reconcile(ctx, liveDir, graph.FSSource{Root: liveDir}); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, "/definitely/gone-wt", graph.FSSource{Root: liveDir}); err != nil {
		t.Fatal(err)
	}
	present := t.TempDir()
	if err := a.Graph.Reconcile(ctx, present, graph.FSSource{Root: liveDir}); err != nil {
		t.Fatal(err)
	}

	dead, err := a.DeadWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0] != "/definitely/gone-wt" {
		t.Fatalf("DeadWorktrees = %v, want exactly [/definitely/gone-wt]", dead)
	}
}

// TestDeadWorktreesUnavailableListIsNoop: with no authoritative worktree list
// (git errors), nothing may be condemned.
func TestDeadWorktreesUnavailableListIsNoop(t *testing.T) {
	a := newTestApp(t, "main") // fakeGit errors on `worktree list`
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, "/definitely/gone-wt", graph.FSSource{Root: src}); err != nil {
		t.Fatal(err)
	}
	dead, err := a.DeadWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 0 {
		t.Fatalf("DeadWorktrees = %v, want none when the live list is unavailable", dead)
	}
}
