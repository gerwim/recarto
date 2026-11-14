package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
)

// initGitRepo creates a real, minimal git repo with one committed Go file so
// the graph has something to index. Duplicated per test package (no shared
// testutil package yet).
func initGitRepo(t *testing.T, dir, funcName string) {
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
	src := "package main\n\nfunc " + funcName + "() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
}

// newServedApp opens an app in a non-git scratch dir serving two real repos
// via RECARTO_REPOS — the repo-less verifier shape.
func newServedApp(t *testing.T) (*app.App, string, string) {
	t.Helper()
	return servedApp(t, "")
}

// servedApp is newServedApp with RECARTO_WORKSPACE set to workspace when it
// is not empty.
func servedApp(t *testing.T, workspace string) (*app.App, string, string) {
	t.Helper()
	clearRecartoEnv(t)
	if workspace != "" {
		t.Setenv("RECARTO_WORKSPACE", workspace)
	}
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base := t.TempDir()
	repo1, repo2, scratch := filepath.Join(base, "repo1"), filepath.Join(base, "repo2"), filepath.Join(base, "_scratch")
	for _, d := range []string{repo1, repo2, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo1, "AlphaLookup")
	initGitRepo(t, repo2, "BetaLookup")
	t.Setenv("RECARTO_REPOS", repo1+string(os.PathListSeparator)+repo2)
	a, err := app.Open(app.Config{CWD: scratch, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, repo1, repo2
}

func TestRepoTargetResolvesServedRepoByName(t *testing.T) {
	a, repo1, _ := newServedApp(t)
	rc, err := repoTarget(a, "repo1")
	if err != nil || rc.Git.Worktree != repo1 {
		t.Fatalf("repoTarget(repo1) = (%+v, %v), want the served repo1", rc, err)
	}
	if _, err := repoTarget(a, "nope"); err == nil || !strings.Contains(err.Error(), "repo1") {
		t.Fatalf("unknown repo must error listing served repos, got %v", err)
	}
	// Default: the primary.
	rc, err = repoTarget(a, "")
	if err != nil || rc.Git.RepoID != a.Git.RepoID {
		t.Fatalf("repoTarget(\"\") = (%+v, %v), want the primary", rc, err)
	}
}

func TestGraphToolRoutesToServedRepo(t *testing.T) {
	a, _, _ := newServedApp(t)
	ctx := context.Background()

	out, err := getFileOutline(ctx, a, GetFileOutlineInput{Repo: "repo2", File: "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range out.Symbols {
		if strings.Contains(s.QualifiedName, "BetaLookup") {
			found = true
		}
	}
	if !found {
		t.Fatalf("outline of repo2/main.go missing BetaLookup: %+v (hint: %s)", out.Symbols, out.Hint)
	}

	// The primary (scratch) has no main.go — proves the repo param routed.
	out, err = getFileOutline(ctx, a, GetFileOutlineInput{File: "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 {
		t.Fatalf("primary scratch outline should be empty, got %+v", out.Symbols)
	}
}

func TestMemoryToolsSpanServedRepos(t *testing.T) {
	a, _, _ := newServedApp(t)
	ctx := context.Background()

	// Write into repo1's store from the repo-less session.
	rec, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "decision", Title: "alpha stays sync", Body: "alpha lookup stays synchronous by design",
		Repo: "repo1", Files: []string{"main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// It landed in repo1's store, not the primary's.
	if r, _ := a.Engine.GetByID(ctx, rec.ID); r != nil {
		t.Fatal("repo-routed memory must not land in the primary store")
	}
	if r, _ := a.Repos[0].Engine.GetByID(ctx, rec.ID); r == nil {
		t.Fatal("repo-routed memory missing from repo1's store")
	}

	// Merged recall finds it and labels the repo.
	out, err := recallMemory(ctx, a, RecallMemoryInput{TaskHint: "alpha lookup synchronous"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Memories {
		if m.ID == rec.ID {
			found = true
			if m.Repo != "repo1" {
				t.Errorf("recalled Repo = %q, want repo1", m.Repo)
			}
		}
	}
	if !found {
		t.Fatalf("repo1 memory absent from merged recall: %+v", out.Memories)
	}

	// get/forget resolve the id across served stores.
	got, err := getMemory(ctx, a, GetMemoryInput{ID: rec.ID})
	if err != nil || !got.Found || got.Memory.Repo != "repo1" {
		t.Fatalf("getMemory = (%+v, %v), want found with Repo=repo1", got, err)
	}
	fout, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: rec.ID})
	if err != nil || !fout.Forgotten {
		t.Fatalf("forgetMemory = (%+v, %v), want forgotten via repo fallback", fout, err)
	}
}

func TestRecordMemoryRejectsRepoWithWorkspaceScope(t *testing.T) {
	a, _, _ := newServedApp(t)
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{
		Kind: "decision", Body: "x", Repo: "repo1", Scope: scopeWorkspace,
	}); err == nil {
		t.Fatal("repo + scope=workspace must be rejected")
	}
}
