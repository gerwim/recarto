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
	"github.com/gerwim/recarto/internal/memory"
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

func newPruneApp(t *testing.T, live ...string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: worktreeListGit{fakeGit: fakeGit{branch: "main"}, live: live},
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
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "")
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
	if !strings.Contains(out.String(), "fts optimized: ") {
		t.Errorf("output %q should report the FTS optimize", out.String())
	}
}

// TestCompactDryRunDoesNotPrune: --dry-run must not delete anything.
func TestCompactDryRunDoesNotPrune(t *testing.T) {
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "")
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

// unusedSetup is a store with wtOld and wtLegacy last used at T0, wtRecent used
// at T0+6d, and the current worktree /repo, all live, at T0+10d.
type unusedSetup struct {
	a                         *app.App
	t0                        time.Time
	wtOld, wtRecent, wtLegacy string
}

func seedUnusedWorktrees(t *testing.T) unusedSetup {
	t.Helper()
	ctx := context.Background()
	s := unusedSetup{wtOld: t.TempDir(), wtRecent: t.TempDir(), wtLegacy: t.TempDir()}
	s.a = newPruneApp(t, s.wtOld, s.wtRecent, s.wtLegacy, "/repo")
	fc := s.a.Clock.(*clock.Fake)
	s.t0 = fc.Now()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := graph.FSSource{Root: src}
	if err := s.a.Graph.Reconcile(ctx, "/repo", fs); err != nil {
		t.Fatal(err)
	}
	for _, wt := range []string{s.wtOld, s.wtRecent} {
		if err := s.a.Graph.ReconcileIfStale(ctx, wt, fs, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.a.Graph.Reconcile(ctx, s.wtLegacy, fs); err != nil {
		t.Fatal(err)
	}
	fc.Advance(6 * 24 * time.Hour)
	if err := s.a.Graph.MarkUsed(ctx, s.wtRecent); err != nil {
		t.Fatal(err)
	}
	fc.Advance(4 * 24 * time.Hour)
	return s
}

func worktreeFiles(t *testing.T, a *app.App, wt string) int {
	t.Helper()
	var n int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCompactPrunesUnusedWorktrees: compact drops the graph of worktrees not
// used for N days, keeps recent ones and its own, and keeps the memories,
// bugs and todos recorded from the pruned worktree.
func TestCompactPrunesUnusedWorktrees(t *testing.T) {
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "")
	s := seedUnusedWorktrees(t)
	ctx := context.Background()
	refs := []memory.Ref{{Type: memory.RefFile, Value: "a.go"}}
	for _, m := range []memory.NewMemory{
		{Kind: memory.KindLearning, Title: "learning from old worktree", Body: "the parser caches tokens per file", Refs: refs, Worktree: s.wtOld},
		{Kind: memory.KindBug, Title: "bug from old worktree", Body: "nil map write in the indexer", Refs: refs, Worktree: s.wtOld},
		{Kind: memory.KindTodo, Title: "todo from old worktree", Body: "split the resolver", Refs: refs, Worktree: s.wtOld},
	} {
		if _, _, _, err := s.a.Engine.Record(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := Compact(ctx, s.a, &out, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if got := worktreeFiles(t, s.a, s.wtOld); got != 0 {
		t.Errorf("unused worktree files = %d, want 0", got)
	}
	if got := worktreeFiles(t, s.a, s.wtLegacy); got != 0 {
		t.Errorf("unmarked old worktree files = %d, want 0", got)
	}
	for _, wt := range []string{s.wtRecent, "/repo"} {
		if got := worktreeFiles(t, s.a, wt); got != 1 {
			t.Errorf("%s files = %d, want 1", wt, got)
		}
	}
	if !strings.Contains(out.String(), "worktrees pruned: 2") || !strings.Contains(out.String(), "--vacuum") {
		t.Errorf("output %q should report 2 pruned worktrees and the --vacuum hint", out.String())
	}

	rs, err := s.a.Engine.Recall(ctx, memory.RecallQuery{Worktree: s.wtOld, Files: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[memory.Kind]bool{}
	for _, r := range rs.Results {
		kinds[r.Record.Kind] = true
	}
	if !kinds[memory.KindLearning] || !kinds[memory.KindBug] {
		t.Errorf("recall after prune returned kinds %v, want the learning and the bug", kinds)
	}
	todos, err := s.a.Engine.TodosForCode(ctx, "", []string{"a.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(todos) != 1 {
		t.Errorf("todos for a.go after prune = %d, want 1", len(todos))
	}
}

// TestCompactDryRunListsUnusedWorktrees: --dry-run names each unused worktree
// with its last use and changes nothing.
func TestCompactDryRunListsUnusedWorktrees(t *testing.T) {
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "")
	s := seedUnusedWorktrees(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := Compact(ctx, s.a, &out, []string{"--force", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	want := s.wtOld + "  last used " + s.t0.Format("2006-01-02 15:04")
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q should contain %q", out.String(), want)
	}
	for _, wt := range []string{s.wtOld, s.wtRecent, s.wtLegacy, "/repo"} {
		if got := worktreeFiles(t, s.a, wt); got != 1 {
			t.Errorf("%s files after dry run = %d, want 1", wt, got)
		}
	}
	var tok string
	if err := s.a.Store.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, "graph_token:"+s.wtOld).Scan(&tok); err != nil {
		t.Errorf("dry run dropped the token of %s: %v", s.wtOld, err)
	}
}

// TestCompactUnusedOffPrunesOnlyDead: with N = 0 only worktrees whose
// directory is gone are pruned.
func TestCompactUnusedOffPrunesOnlyDead(t *testing.T) {
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "0")
	s := seedUnusedWorktrees(t)
	ctx := context.Background()
	src := graph.FSSource{Root: s.wtOld}
	if err := os.WriteFile(filepath.Join(s.wtOld, "b.go"), []byte("package p\nfunc B(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.a.Graph.Reconcile(ctx, "/definitely/gone-wt", src); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Compact(ctx, s.a, &out, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if deadFiles(t, s.a) != 0 {
		t.Error("dead worktree rows survived compact")
	}
	if got := worktreeFiles(t, s.a, s.wtOld); got != 1 {
		t.Errorf("old worktree files = %d, want 1 with unused pruning off", got)
	}
}

// TestCompactBadUnusedDaysStillPrunesDead: an invalid setting still drops
// deleted worktrees, keeps unused ones as 0 does, runs the memory compaction,
// and then fails compact with a message naming the variable.
func TestCompactBadUnusedDaysStillPrunesDead(t *testing.T) {
	for _, v := range []string{"abc", "-1", "106752"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "")
			s := seedUnusedWorktrees(t)
			if err := s.a.Graph.Reconcile(context.Background(), "/definitely/gone-wt", graph.FSSource{Root: s.wtOld}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", v)
			var out bytes.Buffer
			err := Compact(context.Background(), s.a, &out, []string{"--force"})
			if err == nil || !strings.Contains(err.Error(), "RECARTO_UNUSED_WORKTREE_DAYS") {
				t.Fatalf("Compact error = %v, want one naming RECARTO_UNUSED_WORKTREE_DAYS", err)
			}
			if got := deadFiles(t, s.a); got != 0 {
				t.Errorf("dead worktree files = %d, want 0", got)
			}
			if !strings.Contains(out.String(), "epoch:") {
				t.Errorf("output %q should report the memory compaction", out.String())
			}
			for _, wt := range []string{s.wtOld, s.wtRecent, s.wtLegacy, "/repo"} {
				if got := worktreeFiles(t, s.a, wt); got != 1 {
					t.Errorf("%s files = %d, want 1", wt, got)
				}
			}
		})
	}
}
