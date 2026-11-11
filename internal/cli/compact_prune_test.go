package cli

import (
	"bytes"
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

func newPruneApp(t *testing.T, liveDir string) *app.App {
	t.Helper()
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
	return a
}

func seedDeadWorktree(t *testing.T, a *app.App, liveDir string) {
	t.Helper()
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
}

func deadFiles(t *testing.T, a *app.App) int {
	t.Helper()
	var n int
	if err := a.Store.DB.QueryRow(
		`SELECT COUNT(*) FROM files WHERE worktree='/definitely/gone-wt'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCompactPrunesDeadWorktreesAndVacuums: `recarto compact` is the
// maintenance entrypoint — it must sweep dead worktrees' graph copies, report
// the count, and (with --vacuum) return the freed pages to the OS.
func TestCompactPrunesDeadWorktreesAndVacuums(t *testing.T) {
	liveDir := t.TempDir()
	a := newPruneApp(t, liveDir)
	seedDeadWorktree(t, a, liveDir)

	var out bytes.Buffer
	if err := Compact(context.Background(), a, &out, []string{"--force", "--vacuum"}); err != nil {
		t.Fatal(err)
	}
	if deadFiles(t, a) != 0 {
		t.Error("dead worktree rows survived compact")
	}
	if !strings.Contains(out.String(), "worktrees pruned: 1") {
		t.Errorf("output %q should report the pruned worktree", out.String())
	}
	if !strings.Contains(out.String(), "vacuum") {
		t.Errorf("output %q should report the vacuum", out.String())
	}
}

// TestCompactDryRunDoesNotPrune: --dry-run must not delete anything.
func TestCompactDryRunDoesNotPrune(t *testing.T) {
	liveDir := t.TempDir()
	a := newPruneApp(t, liveDir)
	seedDeadWorktree(t, a, liveDir)

	var out bytes.Buffer
	if err := Compact(context.Background(), a, &out, []string{"--force", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if deadFiles(t, a) == 0 {
		t.Error("--dry-run pruned the dead worktree")
	}
	if !strings.Contains(out.String(), "would prune") {
		t.Errorf("output %q should report what a real run would prune", out.String())
	}
}
