package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
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

// TestSessionStartPrunesDeadWorktrees: deleting a git worktree must eventually
// delete its graph copy from the shared store — session start is the
// opportunistic moment (best-effort, inside the warm-up budget).
func TestSessionStartPrunesDeadWorktrees(t *testing.T) {
	liveDir := t.TempDir()
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
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
	if err := a.Graph.Reconcile(ctx, liveDir, graph.FSSource{Root: liveDir}); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, "/definitely/gone-wt", graph.FSSource{Root: liveDir}); err != nil {
		t.Fatal(err)
	}

	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}

	count := func(wt string) int {
		var n int
		if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count("/definitely/gone-wt"); got != 0 {
		t.Errorf("dead worktree still has %d files after session start", got)
	}
	if got := count(liveDir); got != 1 {
		t.Errorf("live worktree files = %d, want 1 (prune must be scoped)", got)
	}
}
