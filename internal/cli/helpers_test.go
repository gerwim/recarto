package cli

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/store"
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
	// A developer's own config file must not switch features off under the tests.
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
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
	// A Recarto MCP server runs for the repo, so hooks point at the MCP tools.
	release, err := app.MarkServing(a.Git.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return a
}

func holdWriter(t *testing.T, st *store.Store) (release func()) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+st.Path()+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	release = func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		_ = conn.Close()
		_ = db.Close()
	}
	t.Cleanup(release)
	return release
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
