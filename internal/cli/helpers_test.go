package cli

import (
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
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
	case args[0] == "rev-parse" && has("--git-common-dir") && has("--path-format=absolute"):
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

// newTestApp builds an App against a temp store with a fixed branch and clock.
func newTestApp(t *testing.T, branch string) *app.App {
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

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a.go , , b.go ,")
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Errorf("splitCSV = %#v, want [a.go b.go]", got)
	}
	if splitCSV("   ") != nil {
		t.Error("blank input should yield nil")
	}
	if splitCSV("") != nil {
		t.Error("empty input should yield nil")
	}
}
