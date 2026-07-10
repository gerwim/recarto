package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates a real, minimal git repo with one committed file.
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
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
}

func TestOpenReposServesListedRepos(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base := t.TempDir()
	repo1 := filepath.Join(base, "repo1")
	repo2 := filepath.Join(base, "repo2")
	scratch := filepath.Join(base, "_scratch")
	for _, d := range []string{repo1, repo2, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo1)
	initGitRepo(t, repo2)
	missing := filepath.Join(base, "not-mounted")
	nonGit := filepath.Join(base, "plain")
	if err := os.MkdirAll(nonGit, 0o755); err != nil {
		t.Fatal(err)
	}
	// repo1 listed twice: the duplicate must collapse. missing/non-git: skipped.
	t.Setenv("RECARTO_REPOS", strings.Join([]string{repo1, repo2, repo1, missing, nonGit}, string(os.PathListSeparator)))

	a, err := Open(Config{CWD: scratch})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	if !a.Git.NonGit {
		t.Fatalf("primary should be the non-git scratch dir, got %+v", a.Git)
	}
	if len(a.Repos) != 2 {
		t.Fatalf("Repos = %d entries, want 2 (dedup + skip missing/non-git): %+v", len(a.Repos), a.Repos)
	}
	names := []string{a.Repos[0].Git.RepoName, a.Repos[1].Git.RepoName}
	if names[0] != "repo1" || names[1] != "repo2" {
		t.Fatalf("served repos = %v, want [repo1 repo2] in listed order", names)
	}
	all := a.AllRepos()
	if len(all) != 3 || all[0].Git.RepoID != a.Git.RepoID {
		t.Fatalf("AllRepos must lead with the primary: %+v", all)
	}
}

func TestOpenReposDropsPrimaryDuplicate(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	repo := t.TempDir()
	initGitRepo(t, repo)
	t.Setenv("RECARTO_REPOS", repo)

	// cwd IS the listed repo: the extra is the primary and must collapse.
	a, err := Open(Config{CWD: repo})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if len(a.Repos) != 0 {
		t.Fatalf("Repos = %+v, want empty (the listed repo is the primary)", a.Repos)
	}
}

func TestOpenReposSharesStoreWithCdSession(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	repo := t.TempDir()
	initGitRepo(t, repo)

	// A session cd'd into the repo and a repo-less session serving it via
	// RECARTO_REPOS must resolve the SAME store id (the git common dir key).
	t.Setenv("RECARTO_REPOS", "")
	cdApp, err := Open(Config{CWD: repo})
	if err != nil {
		t.Fatal(err)
	}
	cdID := cdApp.Git.RepoID
	_ = cdApp.Close()

	scratch := t.TempDir()
	t.Setenv("RECARTO_REPOS", repo)
	a, err := Open(Config{CWD: scratch})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if len(a.Repos) != 1 || a.Repos[0].Git.RepoID != cdID {
		t.Fatalf("served repo id = %+v, want the cd'd session's %s", a.Repos, cdID)
	}
}
