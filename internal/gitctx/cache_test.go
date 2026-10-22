package gitctx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countingGit is real git that counts its spawns.
type countingGit struct {
	Real
	n *int
}

func (c countingGit) Run(args ...string) (string, error) {
	*c.n++
	return c.Real.Run(args...)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitRepo(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

// resolve runs ResolveCached for dir and returns the context and git spawn count.
func resolve(t *testing.T, cache, dir string) (Context, int) {
	t.Helper()
	n := 0
	gc, err := ResolveCached(cache, countingGit{Real{Dir: dir}, &n}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return gc, n
}

func TestResolveCachedReusesUntilHeadMoves(t *testing.T) {
	cache, repo := t.TempDir(), t.TempDir()
	commitRepo(t, repo)

	first, n := resolve(t, cache, repo)
	if n == 0 || first.Branch != "main" || first.RootCommit == "" {
		t.Fatalf("first resolve: %+v (%d git calls)", first, n)
	}
	if again, n := resolve(t, cache, repo); n != 0 || again != first {
		t.Fatalf("unchanged checkout: %d git calls, %+v, want 0 and %+v", n, again, first)
	}

	git(t, repo, "checkout", "-q", "-b", "feature")
	if moved, n := resolve(t, cache, repo); n == 0 || moved.Branch != "feature" {
		t.Fatalf("after checkout: %+v (%d git calls), want a fresh resolve on feature", moved, n)
	}
	git(t, repo, "checkout", "-q", "--detach")
	if det, _ := resolve(t, cache, repo); !strings.HasPrefix(det.Branch, "detached@") {
		t.Fatalf("after detach: branch %q", det.Branch)
	}
	if _, n := resolve(t, cache, repo); n != 0 {
		t.Fatalf("detached checkout not reused: %d git calls", n)
	}
}

func TestResolveCachedLinkedWorktree(t *testing.T) {
	cache, base := t.TempDir(), t.TempDir()
	main := filepath.Join(base, "main")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	commitRepo(t, main)
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "side", wt)

	gc, _ := resolve(t, cache, wt)
	primary, _ := resolve(t, cache, main)
	if gc.Branch != "side" || gc.RepoID != primary.RepoID || gc.Worktree == primary.Worktree {
		t.Fatalf("worktree %+v vs main %+v", gc, primary)
	}
	if again, n := resolve(t, cache, wt); n != 0 || again != gc {
		t.Fatalf("linked worktree not reused: %d git calls", n)
	}
	git(t, wt, "checkout", "-q", "-b", "side2")
	if moved, _ := resolve(t, cache, wt); moved.Branch != "side2" {
		t.Fatalf("worktree checkout missed: %q", moved.Branch)
	}
}

func TestResolveCachedSkipsUnbornRepo(t *testing.T) {
	cache, repo := t.TempDir(), t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	resolve(t, cache, repo)
	if gc, n := resolve(t, cache, repo); n == 0 || gc.RootCommit != "" {
		t.Fatalf("unborn repo must keep resolving: %+v (%d git calls)", gc, n)
	}
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	if gc, _ := resolve(t, cache, repo); gc.RootCommit == "" {
		t.Fatal("first commit's root commit was not picked up")
	}
}

func TestResolveCachedNonGitUntilRepoAppears(t *testing.T) {
	cache, dir := t.TempDir(), t.TempDir()
	if gc, _ := resolve(t, cache, dir); !gc.NonGit {
		t.Skipf("temp dir is inside a git repo (%+v)", gc)
	}
	if gc, n := resolve(t, cache, dir); n != 0 || !gc.NonGit {
		t.Fatalf("non-git dir not reused: %+v (%d git calls)", gc, n)
	}
	commitRepo(t, dir)
	if gc, n := resolve(t, cache, dir); n == 0 || gc.NonGit {
		t.Fatalf("new repo missed: %+v (%d git calls)", gc, n)
	}
}

func TestResolveCachedNestedRepoInvalidates(t *testing.T) {
	cache, repo := t.TempDir(), t.TempDir()
	commitRepo(t, repo)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outer, _ := resolve(t, cache, sub)
	if _, n := resolve(t, cache, sub); n != 0 {
		t.Fatalf("subdirectory not reused: %d git calls", n)
	}
	commitRepo(t, sub)
	if inner, _ := resolve(t, cache, sub); inner.RepoID == outer.RepoID || !sameDir(inner.Worktree, sub) {
		t.Fatalf("nested repo missed: %+v", inner)
	}
}

func TestResolveCachedBypassedByGitDir(t *testing.T) {
	cache, repo := t.TempDir(), t.TempDir()
	commitRepo(t, repo)
	t.Setenv("GIT_DIR", filepath.Join(repo, ".git"))
	resolve(t, cache, repo)
	if _, n := resolve(t, cache, repo); n == 0 {
		t.Fatal("GIT_DIR must bypass the cache")
	}
}
