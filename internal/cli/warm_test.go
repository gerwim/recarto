package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/agents"
	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/store"
)

// filesIn counts the graph files a's store holds for wt.
func filesIn(t *testing.T, a *app.App, wt string) int {
	t.Helper()
	var n int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWarmPrunesDeadWorktrees: deleting a git worktree must eventually delete
// its graph copy from the shared store; the work session start hands off is
// the opportunistic moment.
func TestWarmPrunesDeadWorktrees(t *testing.T) {
	liveDir := t.TempDir()
	a := newPruneApp(t, liveDir)
	a.Git.Worktree = t.TempDir()
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

	if err := Warm(ctx, a, nil); err != nil {
		t.Fatal(err)
	}

	if got := filesIn(t, a, "/definitely/gone-wt"); got != 0 {
		t.Errorf("dead worktree still has %d files after warm", got)
	}
	if got := filesIn(t, a, liveDir); got != 1 {
		t.Errorf("live worktree files = %d, want 1 (prune must be scoped)", got)
	}
}

// TestWarmPrunesUnusedWorktrees: warm drops the graph of a worktree not used
// for N days and keeps a recent one and its own.
func TestWarmPrunesUnusedWorktrees(t *testing.T) {
	wtOld, wtRecent, own := t.TempDir(), t.TempDir(), t.TempDir()
	a := newPruneApp(t, wtOld, wtRecent, own)
	a.Git.Worktree = own
	fc := a.Clock.(*clock.Fake)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(own, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := graph.FSSource{Root: own}
	for _, wt := range []string{wtOld, wtRecent, own} {
		if err := a.Graph.Reconcile(ctx, wt, src); err != nil {
			t.Fatal(err)
		}
	}
	fc.Advance(6 * 24 * time.Hour)
	if err := a.Graph.MarkUsed(ctx, wtRecent); err != nil {
		t.Fatal(err)
	}
	fc.Advance(4 * 24 * time.Hour)

	if err := Warm(ctx, a, nil); err != nil {
		t.Fatal(err)
	}

	if got := filesIn(t, a, wtOld); got != 0 {
		t.Errorf("unused worktree still has %d files after warm", got)
	}
	if got := filesIn(t, a, wtRecent); got != 1 {
		t.Errorf("recent worktree files = %d, want 1", got)
	}
	if got := filesIn(t, a, own); got == 0 {
		t.Error("warm pruned its own worktree")
	}
}

// initGitRepo creates a real git repo in dir with files (path → content)
// committed. Duplicated per test package (no shared testutil package yet).
func initGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestWarmIndexesServedReposAndCachesTheirMap(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	initGitRepo(t, primary, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	initGitRepo(t, extra, map[string]string{"lib/b.go": "package lib\n\nfunc B() {}\n"})
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO"} {
		t.Setenv(k, "")
	}
	t.Setenv("RECARTO_REPOS", extra)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	a, err := app.Open(app.Config{CWD: primary, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()

	if err := Warm(ctx, a, nil); err != nil {
		t.Fatal(err)
	}

	repos := a.AllRepos()
	if len(repos) != 2 {
		t.Fatalf("served repos = %d, want 2", len(repos))
	}
	for i, mod := range []string{"api", "lib"} {
		rc := repos[i]
		wt := rc.Git.Worktree
		// Nothing reads the map before the nodes go: a read of a finished index caches the map itself.
		if err := rc.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx, `DELETE FROM nodes`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		arch, err := rc.Graph.Architecture(ctx, wt, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(arch.Modules) != 1 || arch.Modules[0].Name != mod || arch.Modules[0].Symbols == 0 {
			t.Errorf("%s: map = %+v, want module %s with its symbols from the cache warm wrote", wt, arch.Modules, mod)
		}
	}
}

func TestWarmBadUnusedDaysStillPrunesDead(t *testing.T) {
	wtOld, own := t.TempDir(), t.TempDir()
	a := newPruneApp(t, wtOld, own)
	a.Git.Worktree = own
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(own, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := graph.FSSource{Root: own}
	for _, wt := range []string{own, wtOld, "/definitely/gone-wt"} {
		if err := a.Graph.Reconcile(ctx, wt, src); err != nil {
			t.Fatal(err)
		}
	}
	a.Clock.(*clock.Fake).Advance(10 * 24 * time.Hour)
	t.Setenv("RECARTO_UNUSED_WORKTREE_DAYS", "7d")

	if err := Warm(ctx, a, nil); err == nil || !strings.Contains(err.Error(), "RECARTO_UNUSED_WORKTREE_DAYS") {
		t.Fatalf("Warm error = %v, want one naming RECARTO_UNUSED_WORKTREE_DAYS", err)
	}
	if got := filesIn(t, a, "/definitely/gone-wt"); got != 0 {
		t.Errorf("dead worktree files = %d, want 0", got)
	}
	if got := filesIn(t, a, wtOld); got != 1 {
		t.Errorf("unused worktree files = %d, want 1", got)
	}
}

func TestWarmFlags(t *testing.T) {
	ctx := context.Background()
	own, other := t.TempDir(), t.TempDir()
	a := newPruneApp(t, own, other)
	a.Git.Worktree = own
	if err := os.WriteFile(filepath.Join(own, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDeadWorktree(t, a, other)

	if err := runWarm(ctx, a, io.Discard, []string{"--skip=" + own}); err != nil {
		t.Fatal(err)
	}
	if got := filesIn(t, a, own); got != 0 {
		t.Errorf("--skip: the skipped worktree got %d files", got)
	}
	if got := deadFiles(t, a); got != 0 {
		t.Errorf("--skip: the sweep must still run, dead worktree has %d files", got)
	}

	seedDeadWorktree(t, a, other)
	if err := runWarm(ctx, a, io.Discard, []string{"--only=" + own}); err != nil {
		t.Fatal(err)
	}
	if got := filesIn(t, a, own); got != 1 {
		t.Errorf("--only: indexed files = %d, want 1", got)
	}
	if got := deadFiles(t, a); got == 0 {
		t.Error("--only must not sweep")
	}

	for _, args := range [][]string{
		{"--only=/not/served"},
		{"--only=" + own, "--skip=" + own},
		{"--only=" + own, "--if-code=" + own},
		{"--only=" + own, "--session=s"},
	} {
		if err := runWarm(ctx, a, io.Discard, args); err == nil {
			t.Errorf("warm %v: want an error", args)
		}
	}
}

// scratchServing opens an App in an empty folder outside git serving one
// committed Go repo.
func scratchServing(t *testing.T) (a *app.App, folder, repo string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder, repo = filepath.Join(base, "_scratch"), filepath.Join(base, "lib")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo, map[string]string{"lib/b.go": "package lib\n\nfunc B() {}\n"})
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO"} {
		t.Setenv(k, "")
	}
	t.Setenv("RECARTO_REPOS", repo)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	a, err = app.Open(app.Config{CWD: folder, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, folder, repo
}

func TestWarmIfCode(t *testing.T) {
	a, folder, _ := scratchServing(t)
	ctx := context.Background()
	if err := runWarm(ctx, a, io.Discard, []string{"--if-code=" + folder}); err != nil {
		t.Fatal(err)
	}
	if _, st, err := a.Graph.CodeMap(ctx, folder, true); err != nil || st.Built || filesIn(t, a, folder) != 0 {
		t.Fatalf("a folder without code was indexed: %+v %v", st, err)
	}
	if err := os.WriteFile(filepath.Join(folder, "x.go"), []byte("package x\n\nfunc X() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWarm(ctx, a, io.Discard, []string{"--if-code=" + folder}); err != nil {
		t.Fatal(err)
	}
	if n := filesIn(t, a, folder); n != 1 {
		t.Fatalf("a folder with code: %d files indexed, want 1", n)
	}
}

func TestWarmSessionWritesMissingRosterRows(t *testing.T) {
	a, _, _ := scratchServing(t)
	ctx := context.Background()
	if _, err := agents.Handle(ctx, a, agents.Event{Kind: agents.KindSessionStart, SessionID: "s", Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	lib := a.Repos[0]
	if _, err := lib.Store.DB.Exec(`DELETE FROM sessions WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if err := runWarm(ctx, a, io.Discard, []string{"--session=s"}); err != nil {
		t.Fatal(err)
	}
	var started int64
	if err := lib.Store.DB.QueryRow(`SELECT started_at FROM sessions WHERE id='s' AND ended_at IS NULL`).Scan(&started); err != nil || started != 1_700_000_000_000 {
		t.Fatalf("the repo's row: started %d, %v", started, err)
	}
}
