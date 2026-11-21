package graph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gerwim/recarto/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestHasCode(t *testing.T) {
	ctx := context.Background()
	repo := func(t *testing.T) string {
		dir := t.TempDir()
		gitInit(t, dir)
		return dir
	}
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T) (root string, nonGit bool, candidates []string)
		want, isOK bool
	}{
		{"git, only Java and Markdown", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "A.java", "class A {}")
			writeFile(t, dir, "README.md", "# a")
			return dir, false, nil
		}, false, true},
		{"git, committed Go file deleted", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "a.go", "package a")
			gitRun(t, dir, "add", "a.go")
			gitRun(t, dir, "commit", "-qm", "a")
			if err := os.Remove(filepath.Join(dir, "a.go")); err != nil {
				t.Fatal(err)
			}
			return dir, false, []string{"a.go"}
		}, false, true},
		{"git, only Go file ignored", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, ".gitignore", "a.go\n")
			writeFile(t, dir, "a.go", "package a")
			return dir, false, nil
		}, false, true},
		{"git, Go file", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "a.go", "package a")
			return dir, false, nil
		}, true, true},
		{"git, candidate untracked and ignored but on disk", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "a.go", "package a")
			gitRun(t, dir, "add", "a.go")
			gitRun(t, dir, "commit", "-qm", "a")
			gitRun(t, dir, "rm", "-q", "--cached", "a.go")
			writeFile(t, dir, ".gitignore", "a.go\n")
			return dir, false, []string{"a.go"}
		}, false, true},
		{"git, candidate tracked", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "a.go", "package a")
			gitRun(t, dir, "add", "a.go")
			return dir, false, []string{"a.go"}
		}, true, true},
		{"outside git, empty", func(t *testing.T) (string, bool, []string) {
			return t.TempDir(), true, nil
		}, false, true},
		{"outside git, only node_modules", func(t *testing.T) (string, bool, []string) {
			dir := t.TempDir()
			writeFile(t, dir, "node_modules/x.js", "x")
			return dir, true, nil
		}, false, true},
		{"outside git, Python file", func(t *testing.T) (string, bool, []string) {
			dir := t.TempDir()
			writeFile(t, dir, "x/y.py", "x = 1")
			return dir, true, nil
		}, true, true},
		{"outside git, a directory named a.go", func(t *testing.T) (string, bool, []string) {
			dir := t.TempDir()
			writeFile(t, dir, "a.go/notes.txt", "x")
			return dir, true, []string{"a.go"}
		}, false, true},
		{"git, a directory named a.go", func(t *testing.T) (string, bool, []string) {
			dir := repo(t)
			writeFile(t, dir, "a.go/notes.txt", "x")
			return dir, false, []string{"a.go"}
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, nonGit, cands := tc.setup(t)
			got, err := HasCode(ctx, root, nonGit, cands...)
			if got != tc.want || (err == nil) != tc.isOK {
				t.Fatalf("HasCode = %v, %v; want %v, ok=%v", got, err, tc.want, tc.isOK)
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, nonGit := range []bool{false, true} {
		dir := repo(t)
		writeFile(t, dir, "a.go", "package a")
		if _, err := HasCode(cancelled, dir, nonGit); err == nil {
			t.Errorf("nonGit=%v: a cancelled check must fail, not answer", nonGit)
		}
	}
}

func TestIndexRunning(t *testing.T) {
	e := newTestEngine(t)
	wt := t.TempDir()
	running := func() bool {
		t.Helper()
		r, err := e.IndexRunning(wt)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if running() {
		t.Fatal("no lock file yet: want not running")
	}
	release, err := store.LockFile(context.Background(), e.indexLockPath(wt), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !running() {
		t.Error("lock held: want running")
	}
	release()
	if running() {
		t.Error("lock released: want not running")
	}
}

func TestPresentFiles(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wt := t.TempDir()
	present := func(n int) ([]string, bool) {
		t.Helper()
		p, listed, err := e.PresentFiles(ctx, wt, n)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(p)
		return p, listed
	}
	if p, listed := present(8); listed || len(p) != 0 {
		t.Fatalf("nothing stored: got %v, listed=%v", p, listed)
	}
	writeFile(t, wt, "a.go", "package a")
	writeFile(t, wt, "b.go", "package a")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	if p, listed := present(8); !listed || !slices.Equal(p, []string{"a.go", "b.go"}) {
		t.Errorf("both on disk: got %v, listed=%v", p, listed)
	}
	if p, listed := present(1); !listed || len(p) != 1 {
		t.Errorf("n=1: got %v, listed=%v", p, listed)
	}
	if err := os.Remove(filepath.Join(wt, "a.go")); err != nil {
		t.Fatal(err)
	}
	if p, listed := present(8); !listed || !slices.Equal(p, []string{"b.go"}) {
		t.Errorf("a.go removed: got %v, listed=%v", p, listed)
	}
	if err := os.Remove(filepath.Join(wt, "b.go")); err != nil {
		t.Fatal(err)
	}
	if p, listed := present(8); !listed || len(p) != 0 {
		t.Errorf("all removed: got %v, listed=%v", p, listed)
	}
}
