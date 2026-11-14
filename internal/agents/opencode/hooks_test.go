package opencode

import (
	"context"
	"encoding/json"
	"fmt"
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

// A shell grep lists the exactly named symbol and its bug title through
// OpenCode's payload, and no symbol whose name only contains the pattern.
func TestShellSearchThroughOpenCodePayload(t *testing.T) {
	root := t.TempDir()
	a := newTestApp(t, root)
	ctx := context.Background()
	src := []byte("package p\nfunc handleAugment(){}\nfunc handleAugmentHelper(){}\n")
	if err := a.Graph.IndexFile(ctx, a.Git.Worktree, "h.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow — fix: clamp the size",
		Refs: []memory.Ref{{Type: memory.RefSymbol, Value: "handleAugment"}}, Source: "cli",
		Extra: map[string]any{"fix": "clamp the size"},
	}); err != nil {
		t.Fatal(err)
	}
	cwd, _ := json.Marshal(root)
	var out strings.Builder
	in := `{"session_id":"s","cwd":` + string(cwd) + `,"command":"grep -rn \"handleAugment\" ."}`
	if err := Shell(ctx, a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var res struct{ AdditionalContext string }
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	c := res.AdditionalContext
	if !strings.Contains(c, "handleAugment  h.go:") || !strings.Contains(c, "Widget overflow") ||
		strings.Contains(c, "handleAugmentHelper") || strings.Contains(c, "clamp") {
		t.Errorf("got %q", c)
	}
}
