package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gerwim/recarto/internal/gitctx"
)

func ownedRepo(dir string) *RepoContext {
	return &RepoContext{Git: gitctx.Context{Worktree: dir}}
}

func writeOwned(t *testing.T, root, name, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOwnedFilesAcrossRepos(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeOwned(t, a, "same.go", "package a")
	writeOwned(t, b, "same.go", "package b")
	repos := []*RepoContext{ownedRepo(a), ownedRepo(b)}

	got := OwnedFiles(repos, "same.go")
	if len(got) != 2 || got[0].Repo != repos[0] || got[0].Rel != "same.go" || got[1].Repo != repos[1] || got[1].Rel != "same.go" {
		t.Fatalf("OwnedFiles(relative) = %+v", got)
	}
	got = OwnedFiles(repos, filepath.Join(b, "same.go"))
	if len(got) != 1 || got[0].Repo != repos[1] || got[0].Rel != "same.go" {
		t.Fatalf("OwnedFiles(absolute) = %+v", got)
	}
}

func TestOwnedFilesFollowsSymlinksAndDeduplicates(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	target := writeOwned(t, b, "store/lock.go", "package b")
	if err := os.Symlink(target, filepath.Join(a, "lock.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(b, "lock.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	repos := []*RepoContext{ownedRepo(a), ownedRepo(b)}

	got := OwnedFiles(repos, "lock.go")
	want := []OwnedFile{{Repo: repos[1], Rel: "store/lock.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnedFiles(link) = %+v", got)
	}
}

func TestOwnedFilesFollowsSymlinkBeforeParent(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeOwned(t, a, "x.go", "package a")
	writeOwned(t, b, "subdir/placeholder", "")
	want := writeOwned(t, b, "x.go", "package b")
	if err := os.Symlink(filepath.Join(b, "subdir"), filepath.Join(a, "jump")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	repos := []*RepoContext{ownedRepo(a), ownedRepo(b)}

	got := OwnedFiles(repos, "jump/../x.go")
	if len(got) != 1 || got[0].Repo != repos[1] || got[0].Rel != "x.go" {
		t.Fatalf("OwnedFiles(jump/../x.go) = %+v, want %s owned by repo B", got, want)
	}
}

func TestOwnedFilesIgnoresOutsideMissingAndDirectories(t *testing.T) {
	a := t.TempDir()
	outside := writeOwned(t, t.TempDir(), "outside.go", "package p")
	if err := os.Symlink(outside, filepath.Join(a, "outside.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(a, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"outside.go", "missing.go", "dir"} {
		if got := OwnedFiles([]*RepoContext{ownedRepo(a)}, path); len(got) != 0 {
			t.Errorf("OwnedFiles(%q) = %+v, want none", path, got)
		}
	}
}

func TestOwnedFilesNestedRepos(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "nested", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOwned(t, inner, "x.go", "package p")
	repos := []*RepoContext{ownedRepo(outer), ownedRepo(inner)}
	got := OwnedFiles(repos, "nested/inner/x.go")
	if len(got) != 1 || got[0].Repo != repos[1] || got[0].Rel != "x.go" {
		t.Fatalf("OwnedFiles(nested) = %+v", got)
	}
	writeOwned(t, inner, "nested/inner/x.go", "package p")
	got = OwnedFiles(repos, "nested/inner/x.go")
	want := []OwnedFile{{Repo: repos[1], Rel: "nested/inner/x.go"}, {Repo: repos[1], Rel: "x.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnedFiles(ambiguous nested) = %+v, want %+v", got, want)
	}
}
