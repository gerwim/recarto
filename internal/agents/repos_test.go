package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
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
