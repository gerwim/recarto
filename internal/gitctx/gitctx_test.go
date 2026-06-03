package gitctx

import (
	"errors"
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
		"symbolic-ref --short -q HEAD":                       "feature/login",
		"rev-parse --show-toplevel":                          "/home/u/proj",
		"rev-list --max-parents=0 HEAD":                      "abc123def456",
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
		"rev-list --max-parents=0 HEAD":                      "abc123",
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
			"rev-parse --show-toplevel":                          "/home/u/proj",
			"rev-list --max-parents=0 HEAD":                      "abc123",
			"rev-parse --short HEAD":                             "deadbee",
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
