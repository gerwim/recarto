package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// worktreeFakeGit is like fakeGit but returns a real writable directory as the
// worktree root so that snapshot export can create .recarto/memory.jsonl there.
type worktreeFakeGit struct {
	branch   string
	worktree string // real writable temp dir
}

func (f worktreeFakeGit) Run(args ...string) (string, error) {
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
		return f.worktree + "/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return f.worktree, nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

// newTestAppWithWorktree builds an App whose Git.Worktree is a real temp dir.
func newTestAppWithWorktree(t *testing.T, branch string) *app.App {
	t.Helper()
	worktree := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    worktree,
		Runner: worktreeFakeGit{branch: branch, worktree: worktree},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestSnapshotExportThenImport(t *testing.T) {
	ctx := context.Background()
	// Use a real writable worktree so the export can write .recarto/memory.jsonl.
	a := newTestAppWithWorktree(t, "main")
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "L", Body: "shareable learning about queues",
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Snapshot(ctx, a, &buf, []string{"export"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Git.Worktree, ".recarto", "memory.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not written: %v", err)
	}
	if !strings.Contains(buf.String(), "exported 1 memories") {
		t.Fatalf("export should report a count of 1, got: %s", buf.String())
	}
	// Re-importing into the same store: the id already exists → 1 skipped.
	buf.Reset()
	if err := Snapshot(ctx, a, &buf, []string{"import"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1 skipped") {
		t.Fatalf("re-import of same store should skip 1, got: %s", buf.String())
	}
}

func TestSnapshotUsageError(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Snapshot(ctx, a, &buf, nil); err == nil {
		t.Fatal("expected usage error with no subcommand")
	}
	if err := Snapshot(ctx, a, &buf, []string{"bogus"}); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}
