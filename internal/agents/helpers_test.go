package agents

import (
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit + newTestApp duplicated per test package (no shared testutil yet).
type fakeGit struct{ branch string }

func (f fakeGit) Run(args ...string) (string, error) {
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
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return "/repo", nil
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
	} {
		t.Setenv(k, "")
	}
}

func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	return openTestApp(t, branch)
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
