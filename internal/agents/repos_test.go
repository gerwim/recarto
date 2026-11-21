package agents

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/memory"
)

// initGitRepo creates a real, minimal git repo with one committed file.
// Duplicated per test package (no shared testutil package yet).
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "c.go"), []byte("package b\n\nfunc C() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
}

// The verifier shape: a session in a non-git scratch dir reads a SIBLING
// repo's file — the anchored memory recorded by a session cd'd into that repo
// (anchor path relative to the REPO root, not the scratch cwd) must fire.
func TestPostReadSurfacesSiblingRepoGuidance(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base := t.TempDir()
	repo1, scratch := filepath.Join(base, "repo1"), filepath.Join(base, "_scratch")
	for _, d := range []string{repo1, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo1)
	ctx := context.Background()

	// A cd'd session records the anchored decision (repo-relative anchor).
	t.Setenv("RECARTO_REPOS", "")
	cdApp, err := app.Open(app.Config{CWD: repo1, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	branch := cdApp.Git.Branch
	if _, _, _, err := cdApp.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDecision, Title: "c.go stays tiny", Body: "keep c.go dependency-free",
		Branch: branch, Worktree: cdApp.Git.Worktree, Source: "mcp",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a/b/c.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = cdApp.Close()

	// The repo-less session serves repo1 and reads the file by ABSOLUTE path
	// (which is `repo1/a/b/c.go` relative to its own cwd — one folder deeper).
	t.Setenv("RECARTO_REPOS", repo1)
	a, err := app.Open(app.Config{CWD: scratch, Clock: clock.NewFake(time.UnixMilli(1_700_000_100_000))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	res, err := Handle(ctx, a, Event{Kind: KindPostRead, SessionID: "s1",
		FilePath: filepath.Join(repo1, "a", "b", "c.go"), ToolResult: "package b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "c.go stays tiny") {
		t.Fatalf("sibling repo's anchored decision did not fire:\n%q", res.Context)
	}
}

// Session start names the served repos so the agent knows to target them.
func TestSessionStartNamesServedRepos(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base := t.TempDir()
	repo1, scratch := filepath.Join(base, "repo1"), filepath.Join(base, "_scratch")
	for _, d := range []string{repo1, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo1)
	t.Setenv("RECARTO_REPOS", repo1)
	a, err := app.Open(app.Config{CWD: scratch, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Also serving repos: repo1") {
		t.Fatalf("session-start block missing the served-repos note:\n%s", res.Context)
	}
}

// servedPair serves two repos that both define Serve with a bug anchored to
// it: the primary p (fake git) and the RECARTO_REPOS extra e (a real repo),
// both under base, which is also HOME. outside is a directory neither holds.
func servedPair(t *testing.T) (a *app.App, p, e, outside string) {
	t.Helper()
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, e, outside = filepath.Join(base, "p"), filepath.Join(base, "e"), t.TempDir()
	for _, d := range []string{p, e} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, e)
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	t.Setenv("RECARTO_REPOS", e)
	a, err = app.Open(app.Config{CWD: p, Runner: fakeGit{branch: "main", root: p}, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if len(a.Repos) != 1 {
		t.Fatalf("serving %d extra repos, want 1", len(a.Repos))
	}
	ctx := context.Background()
	const src = "package p\n\nfunc Serve() {}\n"
	for i, rc := range a.AllRepos() {
		if err := rc.Graph.IndexFile(ctx, rc.Git.Worktree, "serve.go", []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
		title := []string{"primary Serve bug", "extra Serve bug"}[i]
		if _, _, _, err := rc.Engine.Record(ctx, memory.NewMemory{
			Kind: memory.KindBug, Title: title, Body: title, Branch: rc.Git.Branch, Source: "cli",
			Refs: []memory.Ref{{Type: memory.RefSymbol, Value: "Serve"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return a, p, e, outside
}

// grepAt runs a Grep for pattern with path and cwd through RunHook, as a host
// hook does, and returns the context it injects.
func grepAt(t *testing.T, a *app.App, session, pattern, path, cwd string) string {
	t.Helper()
	var out bytes.Buffer
	ev := Event{SessionID: session, Pattern: pattern, Path: path, CWD: cwd}
	if err := RunHook(context.Background(), Opened(a), contextAdapter{ev}, KindAugment, nil, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// servedRepoHint reports which repos a hint lists Serve and its bug for:
// "p", "e", "pe" or "".
func servedRepoHint(t *testing.T, hint string) string {
	t.Helper()
	got := ""
	for _, r := range []struct{ name, line, bug string }{
		{"p", "  serve.go:3", "primary Serve bug"},
		{"e", "  e:serve.go:3", "extra Serve bug"},
	} {
		line, bug := strings.Contains(hint, r.line), strings.Contains(hint, r.bug)
		if line != bug {
			t.Fatalf("repo %s: symbol listed %v, bug listed %v:\n%s", r.name, line, bug, hint)
		}
		if line {
			got += r.name
		}
	}
	if got == "" && hint != "" {
		t.Fatalf("hint lists neither repo:\n%s", hint)
	}
	return got
}

// A Grep's path picks the served repos its hint covers.
func TestGrepPathScopesTheHint(t *testing.T) {
	a, p, e, outside := servedPair(t)
	cases := []struct{ name, path, cwd, want string }{
		{"outside", outside, p, ""},
		{"no path", "", p, "pe"},
		{"extra repo", e, p, "e"},
		{"dot", ".", p, "p"},
		{"relative", "../e", p, "e"},
		{"home", "~/e", p, "e"},
		{"contains both", filepath.Dir(p), p, "pe"},
	}
	if vol := filepath.VolumeName(e); runtime.GOOS == "windows" && len(vol) == 2 {
		cases = append(cases, struct{ name, path, cwd, want string }{
			"git bash", "/" + strings.ToLower(vol[:1]) + filepath.ToSlash(e[2:]), p, "e"})
	}
	for _, tc := range cases {
		hint := grepAt(t, a, "s-"+tc.name, "Serve", tc.path, tc.cwd)
		if got := servedRepoHint(t, hint); got != tc.want {
			t.Errorf("%s: hint covers %q, want %q:\n%s", tc.name, got, tc.want, hint)
		}
	}
}

// A Grep path counts where its symlinks lead.
func TestGrepPathThroughSymlinkScopesTheHint(t *testing.T) {
	a, p, e, outside := servedPair(t)
	toP, toE, toOutside := symlinkTo(t, p), symlinkTo(t, e), symlinkTo(t, outside)
	for _, tc := range []struct{ name, path, cwd, want string }{
		{"link into a repo", toE, p, "e"},
		{"missing path under a link", filepath.Join(toE, "missing"), p, "e"},
		{"relative to a linked cwd", ".", toP, "p"},
		{"link outside", toOutside, p, ""},
	} {
		hint := grepAt(t, a, "s-"+tc.name, "Serve", tc.path, tc.cwd)
		if got := servedRepoHint(t, hint); got != tc.want {
			t.Errorf("%s: hint covers %q, want %q:\n%s", tc.name, got, tc.want, hint)
		}
	}
}

// A search outside the served repos, or in one of them, leaves the same
// pattern's first hint in another repo intact.
func TestScopedSearchLeavesOtherHints(t *testing.T) {
	a, p, e, outside := servedPair(t)
	for _, step := range []struct{ path, want string }{{outside, ""}, {e, "e"}, {p, "p"}} {
		hint := grepAt(t, a, "s", "Serve", step.path, p)
		if got := servedRepoHint(t, hint); got != step.want {
			t.Errorf("Grep at %s: hint covers %q, want %q:\n%s", step.path, got, step.want, hint)
		}
	}
}
