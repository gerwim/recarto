package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/memory"
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
	// A developer's own config file must not switch features off under the tests.
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
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
		for _, run := range []func(context.Context, *app.App, io.Reader, io.Writer) error{PostEdit, PostRead, Augment, Shell} {
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

// A bash view of a file surfaces its anchored todo through Copilot's payload.
func TestShellViewThroughCopilotPayload(t *testing.T) {
	root := t.TempDir()
	a := newTestApp(t, root)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "split a.go", Body: "x", Source: "cli", Branch: a.Git.Branch,
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	cwd, _ := json.Marshal(root)
	var out strings.Builder
	in := `{"sessionId":"s","cwd":` + string(cwd) + `,"toolName":"bash","toolArgs":{"command":"head -n 20 a.go | cat -n"}}`
	if err := Shell(ctx, a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var res struct{ AdditionalContext string }
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil || !strings.Contains(res.AdditionalContext, "split a.go") {
		t.Errorf("output %q (%v), want the anchored todo", out.String(), err)
	}
}

func TestPostReadRepeatNoticeThroughView(t *testing.T) {
	root := t.TempDir()
	a := newTestApp(t, root)
	ctx := context.Background()
	file := filepath.Join(root, "a.go")
	if err := os.WriteFile(file, []byte("package a\n// 2\n// 3\n// 4\n// 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(file)
	in := `{"sessionId":"s","toolName":"view","toolArgs":{"path":` + string(path) + `,"view_range":[2,3]},` +
		`"toolResult":{"resultType":"success","textResultForLlm":"2. // 2\n3. // 3"}}`
	var out strings.Builder
	for range 2 {
		out.Reset()
		if err := PostRead(ctx, a, strings.NewReader(in), &out); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "you already have lines 2-3") {
		t.Errorf("second view = %q, want the repeat-read notice", out.String())
	}
}

// A resume of a session that already got the block adds nothing when no
// memory changed since.
func TestSessionStartResumeAddsNothingNew(t *testing.T) {
	root := t.TempDir()
	a := newTestApp(t, root)
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "builds need cgo off", Body: "x", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	start := func(source string) string {
		t.Helper()
		var out strings.Builder
		if err := SessionStart(ctx, a, strings.NewReader(`{"sessionId":"c1","source":"`+source+`"}`), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := start("new"); !strings.Contains(got, "additionalContext") || !strings.Contains(got, "builds need cgo off") {
		t.Fatalf("new session: %q, want the block with the learning", got)
	}
	if got := start("resume"); strings.Contains(got, "additionalContext") {
		t.Errorf("resume: %q, want no context", got)
	}
}
