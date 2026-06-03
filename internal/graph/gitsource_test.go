package graph

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestGitSourceHonorsGitignore(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	writeFile(t, dir, ".gitignore", "ignored.go\n")
	writeFile(t, dir, "tracked.go", "package p\nfunc A(){}")
	writeFile(t, dir, "ignored.go", "package p\nfunc Ignored(){}")
	writeFile(t, dir, "untracked.go", "package p\nfunc U(){}")
	// stage tracked.go so it is --cached
	if out, err := exec.Command("git", "-C", dir, "add", "tracked.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	entries, err := NewSource(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if !slices.Contains(paths, "tracked.go") || !slices.Contains(paths, "untracked.go") {
		t.Errorf("paths = %v, want tracked.go + untracked.go", paths)
	}
	if slices.Contains(paths, "ignored.go") {
		t.Errorf("paths = %v, must NOT include the gitignored file", paths)
	}
}

func TestWorktreeTokenEmptyForNonGit(t *testing.T) {
	if WorktreeToken(t.TempDir()) != "" {
		t.Error("non-git worktree should yield an empty token")
	}
}

func TestWorktreeTokenChangesWithCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	writeFile(t, dir, "a.go", "package p\nfunc A(){}")
	if out, err := exec.Command("git", "-C", dir, "add", "a.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	tok1 := WorktreeToken(dir)
	if tok1 == "" {
		t.Fatal("token must not be empty for a git repo")
	}

	// Dirty the worktree.
	writeFile(t, dir, "b.go", "package p\nfunc B(){}")
	tok2 := WorktreeToken(dir)
	if tok1 == tok2 {
		t.Error("token must change when working tree is dirtied")
	}
}

func TestNewSourceFallsBackToFSSourceForNonGit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package p\nfunc A(){}")
	writeFile(t, dir, filepath.Join("sub", "b.go"), "package p\nfunc B(){}")

	entries, err := NewSource(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if !slices.Contains(paths, "a.go") {
		t.Errorf("paths = %v, want a.go in fallback", paths)
	}
}
