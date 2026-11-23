package gitctx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner maps the joined args to a result.
type fakeRunner struct {
	out map[string]string
	err map[string]error
}

func (f fakeRunner) Run(args ...string) (string, error) {
	k := strings.Join(args, " ")
	if e, ok := f.err[k]; ok {
		return "", e
	}
	if v, ok := f.out[k]; ok {
		return v, nil
	}
	return "", errors.New("unexpected git call: " + k)
}

func TestResolveNormalRepo(t *testing.T) {
	r := fakeRunner{out: map[string]string{
		"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
		"symbolic-ref --short -q HEAD":                      "feature/login",
		"rev-parse --show-toplevel":                         "/home/u/proj",
		"rev-list --max-parents=0 HEAD":                     "abc123def456",
	}}
	ctx, err := Resolve(r, "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.NonGit {
		t.Fatal("expected git repo")
	}
	if ctx.Branch != "feature/login" {
		t.Fatalf("branch = %q", ctx.Branch)
	}
	if ctx.Worktree != "/home/u/proj" {
		t.Fatalf("worktree = %q", ctx.Worktree)
	}
	if ctx.RepoName != "proj" {
		t.Fatalf("repo name = %q", ctx.RepoName)
	}
	if len(ctx.RepoID) != 16 {
		t.Fatalf("repo id = %q (len %d), want 16 hex chars", ctx.RepoID, len(ctx.RepoID))
	}
	if ctx.RootCommit != "abc123def456" {
		t.Fatalf("root commit = %q", ctx.RootCommit)
	}
}

func TestResolveSharedAcrossWorktrees(t *testing.T) {
	// Two worktrees of the same repo share the same --git-common-dir,
	// so they must resolve to the same RepoID.
	base := map[string]string{
		"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
		"rev-list --max-parents=0 HEAD":                     "abc123",
	}
	wt1 := fakeRunner{out: mergeMap(base, map[string]string{
		"symbolic-ref --short -q HEAD": "main",
		"rev-parse --show-toplevel":    "/home/u/proj",
	})}
	wt2 := fakeRunner{out: mergeMap(base, map[string]string{
		"symbolic-ref --short -q HEAD": "feature/x",
		"rev-parse --show-toplevel":    "/home/u/proj-wt-feature",
	})}
	c1, _ := Resolve(wt1, "/home/u/proj")
	c2, _ := Resolve(wt2, "/home/u/proj-wt-feature")
	if c1.RepoID != c2.RepoID {
		t.Fatalf("worktrees must share repo id: %q != %q", c1.RepoID, c2.RepoID)
	}
	if c1.Branch == c2.Branch {
		t.Fatal("branches should differ between worktrees")
	}
}

func TestResolveDetachedHead(t *testing.T) {
	r := fakeRunner{
		out: map[string]string{
			"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
			"rev-parse --show-toplevel":                         "/home/u/proj",
			"rev-list --max-parents=0 HEAD":                     "abc123",
			"rev-parse --short HEAD":                            "deadbee",
		},
		err: map[string]error{
			"symbolic-ref --short -q HEAD": errors.New("exit status 1"),
		},
	}
	ctx, err := Resolve(r, "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Branch != "detached@deadbee" {
		t.Fatalf("branch = %q, want detached@deadbee", ctx.Branch)
	}
}

func TestResolveNonGit(t *testing.T) {
	r := fakeRunner{err: map[string]error{
		"rev-parse --path-format=absolute --git-common-dir": errors.New("not a git repository"),
	}}
	ctx, err := Resolve(r, "/tmp/scratch")
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.NonGit {
		t.Fatal("expected NonGit=true")
	}
	if ctx.Branch != "(none)" {
		t.Fatalf("branch = %q, want (none)", ctx.Branch)
	}
	if len(ctx.RepoID) != 16 {
		t.Fatalf("repo id len = %d, want 16", len(ctx.RepoID))
	}
	if ctx.RepoID != NonGitID("/tmp/scratch/") {
		t.Fatalf("repo id %s, want NonGitID's %s", ctx.RepoID, NonGitID("/tmp/scratch/"))
	}
}

func mergeMap(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// fakeWorktreeRunner answers `git worktree list --porcelain` with a canned
// porcelain block.
type fakeWorktreeRunner struct{ out string }

func (f fakeWorktreeRunner) Run(args ...string) (string, error) {
	if args[0] == "worktree" {
		return f.out, nil
	}
	return "", nil
}

// TestLiveWorktrees parses `git worktree list --porcelain`: one block per
// worktree, the path on the "worktree " line; other lines (HEAD, branch,
// detached, bare) are ignored.
func TestLiveWorktrees(t *testing.T) {
	out := "worktree /home/u/repo\nHEAD abc123\nbranch refs/heads/master\n\n" +
		"worktree /home/u/repo-wt2\nHEAD def456\ndetached\n\n" +
		"worktree /home/u/repo wt3\nbare\n"
	got, err := LiveWorktrees(fakeWorktreeRunner{out: out})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/home/u/repo", "/home/u/repo-wt2", "/home/u/repo wt3"}
	if len(got) != len(want) {
		t.Fatalf("LiveWorktrees = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LiveWorktrees[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// commitFile makes a repo at dir whose first commit adds a file holding content,
// so repos with different content have different root commits.
func commitFile(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "f")
	git(t, dir, "commit", "-q", "-m", "init")
	return RootCommit(Real{Dir: dir})
}

func TestHasCommit(t *testing.T) {
	base := t.TempDir()
	r, q := filepath.Join(base, "r"), filepath.Join(base, "q")
	rootR, rootQ := commitFile(t, r, "r"), commitFile(t, q, "q")
	if rootR == "" || rootR == rootQ {
		t.Fatalf("fixture roots %q, %q", rootR, rootQ)
	}
	git(t, r, "checkout", "-q", "--orphan", "other")
	git(t, r, "commit", "-q", "--allow-empty", "-m", "other root")
	rootOther := RootCommit(Real{Dir: r})
	if rootOther == rootR {
		t.Fatal("fixture: the orphan branch kept the root")
	}
	for _, tc := range []struct {
		sha  string
		want bool
	}{{rootR, true}, {rootOther, true}, {rootQ, false}} {
		if got := HasCommit(Real{Dir: r}, tc.sha); got != tc.want {
			t.Errorf("HasCommit(%s) = %v, want %v", tc.sha, got, tc.want)
		}
	}
}

func TestRootCommit(t *testing.T) {
	base := t.TempDir()
	r := filepath.Join(base, "r")
	root := commitFile(t, r, "r")
	git(t, r, "commit", "-q", "--allow-empty", "-m", "second")
	if got := RootCommit(Real{Dir: r}); got != root || len(got) != 40 {
		t.Fatalf("RootCommit = %q, want %q", got, root)
	}
	unborn := filepath.Join(base, "unborn")
	if err := os.MkdirAll(unborn, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, unborn, "init", "-q")
	if got := RootCommit(Real{Dir: unborn}); got != "" {
		t.Fatalf("unborn RootCommit = %q, want empty", got)
	}
}
