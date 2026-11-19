package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/clock"
)

// servedFrom opens an App in a new folder named folder serving the git repos
// at rels, created under one base.
func servedFrom(t *testing.T, folder string, rels ...string) (*App, string) {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY"} {
		t.Setenv(k, "")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, rel := range append([]string{folder}, rels...) {
		if err := os.MkdirAll(filepath.Join(base, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range rels {
		initGitRepo(t, filepath.Join(base, rel))
		paths = append(paths, filepath.Join(base, rel))
	}
	t.Setenv("RECARTO_REPOS", strings.Join(paths, string(os.PathListSeparator)))
	a, err := Open(Config{CWD: filepath.Join(base, folder)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, base
}

// resolves reports which of a's repos the repo argument v resolves to, as
// the MCP tools resolve it: the first that answers to v.
func resolves(a *App, v string) *RepoContext {
	for _, rc := range a.AllRepos() {
		if rc.AnswersTo(v) {
			return rc
		}
	}
	return nil
}

func TestSelector(t *testing.T) {
	a, _ := servedFrom(t, "_scratch", "x/app", "y/app", "lib")
	for i, want := range []string{a.Repos[0].Git.Worktree, a.Repos[1].Git.Worktree, "lib"} {
		sel, ok := a.Selector(a.Repos[i])
		if !ok || sel != want || resolves(a, sel) != a.Repos[i] {
			t.Errorf("repo %d: selector %q %v, want %q resolving to it", i, sel, ok, want)
		}
	}

	a, _ = servedFrom(t, "second", "repos/second")
	if sel, ok := a.Selector(a.Repos[0]); !ok || sel != a.Repos[0].Git.Worktree || resolves(a, sel) != a.Repos[0] {
		t.Errorf("folder named like the repo: selector %q %v", sel, ok)
	}
}

func TestSelectorPathsDifferingInCase(t *testing.T) {
	a, base := servedFrom(t, "_scratch", "RECARTO", "recarto")
	if _, err := os.Stat(filepath.Join(base, "Recarto")); err == nil {
		t.Skip("case-insensitive file system")
	}
	if len(a.Repos) != 2 {
		t.Fatalf("served %d repos", len(a.Repos))
	}
	if sel, ok := a.Selector(a.Repos[0]); !ok || resolves(a, sel) != a.Repos[0] {
		t.Errorf("the first: selector %q %v", sel, ok)
	}
	if sel, ok := a.Selector(a.Repos[1]); ok {
		t.Errorf("the later one got selector %q, which resolves to %s", sel, resolves(a, sel).Git.Worktree)
	}
}

func TestOpenRepoAndFolderStore(t *testing.T) {
	a, base := servedFrom(t, "_scratch", "lib")
	if st, err := FolderStore(filepath.Join(base, "never-opened")); err != nil || st != nil {
		t.Fatalf("a folder never opened: %v %v", st, err)
	}
	st, err := FolderStore(a.Dir)
	if err != nil || st == nil || st.Path() != a.Store.Path() {
		t.Fatalf("FolderStore(%s) = %v, %v; want the App's store", a.Dir, st, err)
	}
	_ = st.Close()

	lib := filepath.Join(base, "lib")
	rc, err := OpenRepo(lib, clock.Real{})
	if err != nil || rc.Git.RepoID != a.Repos[0].Git.RepoID {
		t.Fatalf("OpenRepo: %v", err)
	}
	_ = rc.Close()
	if err := os.Rename(lib, lib+"-moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRepo(lib, clock.Real{}); err == nil {
		t.Fatal("opened a moved-away checkout")
	}
}
