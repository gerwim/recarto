package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit + newTestApp duplicated per test package (no shared testutil yet). The
// worktree is a real temp dir so its paths are absolute on every OS.
type fakeGit struct{ root string }

func (f fakeGit) Run(args ...string) (string, error) {
	switch {
	case args[0] == "rev-parse" && args[len(args)-1] == "--git-common-dir":
		return filepath.Join(f.root, ".git"), nil
	case args[0] == "rev-parse" && args[len(args)-1] == "--show-toplevel":
		return f.root, nil
	case args[0] == "symbolic-ref":
		return "main", nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T, root string) *app.App {
	t.Helper()
	for _, k := range []string{
		"RECARTO_WORKSPACE", "RECARTO_REPOS",
		"RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{CWD: root, Runner: fakeGit{root}, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestPostToolUseFanOutCountsOnlyTheRightHook replays what Copilot before
// v1.0.63 does with the hook file (postToolUse matchers ignored: all three
// entries run for every tool) and checks each tool lands only in its own
// ledger counter, once.
func TestPostToolUseFanOutCountsOnlyTheRightHook(t *testing.T) {
	root := t.TempDir()
	a := newTestApp(t, root)
	ctx := context.Background()
	file, _ := json.Marshal(filepath.Join(root, "a.go"))
	call := func(session, tool string) {
		in := `{"sessionId":"` + session + `","toolName":"` + tool + `","toolArgs":{"path":` + string(file) + `,"pattern":"Foo"}}`
		for _, run := range []func(context.Context, *app.App, io.Reader, io.Writer) error{PostEdit, PostRead, Augment} {
			if err := run(ctx, a, strings.NewReader(in), io.Discard); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(scope, metric string) int64 {
		n, err := a.Ledger.Get(ctx, scope, metric)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		tool         string
		edits, reads int64
	}{
		{"view", 0, 1},
		{"edit", 1, 0},
		{"create", 1, 0},
		{"grep", 0, 0},
		{"glob", 0, 0},
		{"bash", 0, 0},
		{"powershell", 0, 0},
		{"task", 0, 0},
		{"web_fetch", 0, 0},
		{"ask_user", 0, 0},
	} {
		call(c.tool, c.tool) // one session per tool
		if e, r := count(c.tool, "edits"), count("session:"+c.tool, "reads"); e != c.edits || r != c.reads {
			t.Errorf("%s: edits %d reads %d, want %d %d", c.tool, e, r, c.edits, c.reads)
		}
	}
	if n := count("lifetime", "reads"); n != 1 {
		t.Errorf("lifetime reads = %d, want 1 (the view alone)", n)
	}
}
