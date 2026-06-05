package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
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

func TestRecordBugThenSearch(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	add, err := recordBug(ctx, a, RecordBugInput{
		ErrorMessage: "nil pointer in session refresh",
		RootCause:    "missing nil guard",
		Fix:          "add a nil check before deref",
		File:         "internal/auth/session.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if add.ID == "" {
		t.Fatal("record_bug returned no id")
	}

	out, err := searchBugs(ctx, a, SearchBugsInput{Query: "session refresh"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 {
		t.Fatalf("search_bugs found nothing: %+v", out)
	}
	if out.Bugs[0].Title != "nil pointer in session refresh" {
		t.Errorf("top bug = %q", out.Bugs[0].Title)
	}
}

func TestRecordBugRequiresError(t *testing.T) {
	a := newTestApp(t, "main")
	if _, err := recordBug(context.Background(), a, RecordBugInput{}); err == nil {
		t.Error("expected error when error_message is empty")
	}
}

func TestRecordBugCapturesSymbols(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordBug(ctx, a, RecordBugInput{
		ErrorMessage: "panic in Tokenize",
		Fix:          "guard the slice",
		Symbols:      []string{"Tokenize"},
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := a.Engine.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "panic in Tokenize" {
		t.Fatalf("want 1 bug by symbol, got %d: %+v", len(bugs), bugs)
	}
}

func TestCompleteTodoHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	id, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "ship it", Body: "ship the feature", Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := completeTodo(ctx, a, CompleteTodoInput{ID: id})
	if err != nil || !out.Completed {
		t.Fatalf("completeTodo = (%+v, %v), want Completed=true", out, err)
	}
	rec, _ := a.Engine.GetByID(ctx, id)
	if rec.Status != "done" {
		t.Errorf("status = %q, want \"done\"", rec.Status)
	}

	out, err = completeTodo(ctx, a, CompleteTodoInput{ID: "missing"})
	if err != nil || out.Completed {
		t.Fatalf("completeTodo(missing) = (%+v, %v), want Completed=false", out, err)
	}
}

func TestGetAndListMemory(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "decision", Title: "sqlite is source of truth", Body: "markdown is render-only"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := getMemory(ctx, a, GetMemoryInput{ID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Memory == nil || got.Memory.Title != "sqlite is source of truth" {
		t.Errorf("get_memory = %+v", got)
	}

	missing, err := getMemory(ctx, a, GetMemoryInput{ID: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Found {
		t.Error("get_memory for a missing id should report Found=false")
	}

	list, err := listMemory(ctx, a, ListMemoryInput{Kind: "decision"})
	if err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Memories[0].Kind != "decision" {
		t.Errorf("list_memory(kind=decision) = %+v", list)
	}
}
