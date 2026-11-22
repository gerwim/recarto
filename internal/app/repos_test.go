package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	assertDescribeMatches(t, Config{CWD: scratch}, a)
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
	assertDescribeMatches(t, Config{CWD: repo}, a)
}

func TestOpenDecidesCodelessOnlyWhenRequested(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_WORKSPACE", "")
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")
	repo := t.TempDir()
	initGitRepo(t, repo)

	open := func(folder string, decide bool, served bool) *App {
		t.Helper()
		if served {
			t.Setenv("RECARTO_REPOS", repo)
		} else {
			t.Setenv("RECARTO_REPOS", "")
		}
		a, err := Open(Config{CWD: folder, DecideCodeless: decide})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	empty := t.TempDir()
	a := open(empty, true, true)
	if !a.Codeless {
		t.Fatal("empty non-git folder serving a repo was not marked code-less")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	withCode := t.TempDir()
	if err := os.WriteFile(filepath.Join(withCode, "x.go"), []byte("package p\nfunc X() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a = open(withCode, true, true)
	if a.Codeless {
		t.Fatal("folder with indexable code was marked code-less")
	}
	_ = a.Close()

	a = open(t.TempDir(), true, false)
	if !a.Codeless || a.CodelessWithRepos() {
		t.Fatal("empty folder decision should be code-less but not code-less with served repos")
	}
	_ = a.Close()

	a = open(t.TempDir(), false, true)
	if a.Codeless {
		t.Fatal("code-less decision ran with DecideCodeless disabled")
	}
	_ = a.Close()

	codeless := false
	a, err := Open(Config{CWD: t.TempDir(), DecideCodeless: true, Codeless: &codeless})
	if err != nil {
		t.Fatal(err)
	}
	if a.Codeless {
		t.Fatal("explicit false code-less decision was ignored")
	}
	_ = a.Close()
}

func setCodeCheckDeadline(t *testing.T, f func(context.Context) (context.Context, context.CancelFunc)) {
	t.Helper()
	old := codeCheckDeadline
	codeCheckDeadline = f
	t.Cleanup(func() { codeCheckDeadline = old })
}

func TestCodelessNowCountsACheckCutShortAsCode(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_REPOS", "")
	t.Setenv("RECARTO_WORKSPACE", "")
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")
	cfg := Config{CWD: t.TempDir(), DecideCodeless: true}

	t.Run("finished", func(t *testing.T) {
		setCodeCheckDeadline(t, func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, time.Minute)
		})
		codeless, err := CodelessNow(context.Background(), cfg)
		if err != nil || !codeless {
			t.Fatalf("CodelessNow() = (%v, %v), want (true, nil)", codeless, err)
		}
	})
	t.Run("cut short", func(t *testing.T) {
		setCodeCheckDeadline(t, func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			return ctx, cancel
		})
		codeless, err := CodelessNow(context.Background(), cfg)
		if err != nil || codeless {
			t.Fatalf("CodelessNow() = (%v, %v), want (false, nil)", codeless, err)
		}
	})
}

func TestCodeLessWithinDoesNotWaitForACheckCutShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(time.Minute):
			t.Error("check did not finish after release")
		}
	})
	result := make(chan bool, 1)
	go func() {
		result <- codeLessWithin(ctx, func(context.Context) bool {
			defer close(finished)
			close(started)
			<-release
			return true
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("check did not start")
	}
	cancel()
	select {
	case codeless := <-result:
		if codeless {
			t.Fatal("cut-short check was counted as no code")
		}
	case <-time.After(time.Minute):
		t.Fatal("codeLessWithin waited for the cut-short check")
	}
}

// assertDescribeMatches checks that Describe(cfg) offers what the App opened
// with cfg serves.
func assertDescribeMatches(t *testing.T, cfg Config, a *App) {
	t.Helper()
	sv, err := Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Repos != (len(a.Repos) > 0) || sv.Workspace != (a.Workspace != nil) {
		t.Errorf("Describe = %+v, want Repos=%v Workspace=%v as opened", sv, len(a.Repos) > 0, a.Workspace != nil)
	}
	if !slices.Equal(sv.RepoIDs, a.Served().RepoIDs) {
		t.Errorf("Describe repo ids = %v, opened = %v", sv.RepoIDs, a.Served().RepoIDs)
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

func TestWorktreesListsServedRootsWithoutOpeningStores(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cache)
	base := t.TempDir()
	repo, scratch, plain := filepath.Join(base, "repo"), filepath.Join(base, "_scratch"), filepath.Join(base, "plain")
	for _, d := range []string{repo, scratch, plain} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo)
	t.Setenv("RECARTO_REPOS", strings.Join([]string{repo, plain, filepath.Join(base, "missing")}, string(os.PathListSeparator)))

	wts, err := Worktrees(Config{CWD: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || wts[0] != scratch || !sameDir(t, wts[1], repo) {
		t.Fatalf("Worktrees = %v, want [%s %s] (non-git and missing extras skipped)", wts, scratch, repo)
	}
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.Name() != "gitctx" {
			t.Errorf("Worktrees opened a store: %s", e.Name())
		}
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}
