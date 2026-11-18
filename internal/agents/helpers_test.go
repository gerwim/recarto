package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/graph"
)

// fakeGit + newTestApp duplicated per test package (no shared testutil yet).
// root is the worktree it reports, "/repo" when empty.
type fakeGit struct{ branch, root string }

func (f fakeGit) Run(args ...string) (string, error) {
	root := f.root
	if root == "" {
		root = "/repo"
	}
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir"):
		return filepath.Join(root, ".git"), nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return root, nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

// clearRecartoEnv neutralizes ambient RECARTO_* configuration: the machine
// running the tests may itself legitimately use Recarto (sync, workspace and
// served-repo env all set — e.g. a ForgeBoard session), and tests must not
// inherit that state. Duplicated per test package (no shared testutil yet).
func clearRecartoEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"RECARTO_WORKSPACE", "RECARTO_REPOS",
		"RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO",
		"RECARTO_SYNC_INTERVAL", "RECARTO_SYNC_TIMEOUT",
		"RECARTO_UNUSED_WORKTREE_DAYS",
	} {
		t.Setenv(k, "")
	}
	// A developer's own config file must not switch features off under the tests.
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
}

// newTestApp opens a test App on branch with a clean RECARTO_* env. A Recarto
// MCP server is marked running for its repo, as in a session whose server is
// up, so hooks point at the MCP tools.
func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	a := openTestApp(t, branch)
	markServing(t, a)
	return a
}

// markServing marks a Recarto MCP server running for a's repo until the test ends.
func markServing(t *testing.T, a *app.App) {
	t.Helper()
	release, err := app.MarkServing(a.Git.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// openTestApp is newTestApp without the env reset — for helpers that set
// their own RECARTO_* after clearing (newWorkspaceTestApp).
func openTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: branch},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// finishIndex indexes root as a's worktree to completion. Session start no
// longer indexes, so a test that needs the code map or graph hits does.
func finishIndex(t *testing.T, a *app.App, root string) {
	t.Helper()
	if err := a.Graph.ReconcileIfStale(context.Background(), a.Git.Worktree, graph.FSSource{Root: root}, "test-token"); err != nil {
		t.Fatal(err)
	}
}

// symlinkTo returns a new link to target, skipping the test where symlinks
// can't be made.
func symlinkTo(t *testing.T, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	return link
}
