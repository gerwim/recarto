package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
// Duplicated per test package (no shared testutil package yet).
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

func TestRecordMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind:  "learning",
		Body:  "use wal mode and busy_timeout for cross process writers",
		Files: []string{"internal/store/store.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.Reinforced {
		t.Errorf("first record = %+v, want a fresh id and Reinforced=false", out)
	}

	// A near-duplicate of the same kind reinforces instead of inserting.
	out2, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning",
		Body: "use wal mode and busy_timeout for cross process writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Reinforced {
		t.Errorf("near-duplicate = %+v, want Reinforced=true", out2)
	}
}

func TestRecordMemoryRejectsBadKind(t *testing.T) {
	a := newTestApp(t, "main")
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "bogus", Body: "x"}); err == nil {
		t.Error("expected error for invalid kind")
	}
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning"}); err == nil {
		t.Error("expected error for missing body")
	}
}

func TestRecallMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind:  "learning",
		Title: "WAL setup",
		Body:  "use wal mode and busy_timeout for cross process writers",
		Files: []string{"internal/store/store.go"},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := recallMemory(ctx, a, RecallMemoryInput{Files: []string{"internal/store/store.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Memories) == 0 {
		t.Fatalf("recall returned nothing: %+v", out)
	}
	if out.Memories[0].Title != "WAL setup" {
		t.Errorf("top recall = %q, want WAL setup", out.Memories[0].Title)
	}
}
