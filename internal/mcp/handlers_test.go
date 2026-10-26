package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

// clearRecartoEnv neutralizes ambient RECARTO_* configuration: the machine
// running the tests may itself legitimately use Recarto (sync, workspace and
// served-repo env all set — e.g. a ForgeBoard session), and tests must not
// inherit that state.
func clearRecartoEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"RECARTO_WORKSPACE", "RECARTO_REPOS",
		"RECARTO_SYNC_URL", "RECARTO_SYNC_KEY", "RECARTO_SYNC_REPO",
		"RECARTO_SYNC_INTERVAL", "RECARTO_SYNC_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
	// A developer's own config file must not switch features off under the tests.
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
}

func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	return openTestApp(t, branch)
}

// openTestApp is newTestApp without the env reset — for helpers that set
// their own RECARTO_* after clearing (newWorkspaceApp).
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

func TestRecallMemoryExposesMetadata(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	_, _ = recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "alpha beta gamma", Files: []string{"x.go"}})
	out, err := recallMemory(ctx, a, RecallMemoryInput{TaskHint: "alpha beta gamma", Files: []string{"x.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Memories[0].MatchedOn) == 0 {
		t.Fatalf("expected matched_on populated, got %+v", out.Memories)
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

	id, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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

func TestForgetMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "temp"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: rec.ID, Reason: "test"})
	if err != nil || !out.Forgotten {
		t.Fatalf("forgetMemory = %+v, %v; want Forgotten=true", out, err)
	}
	miss, _ := forgetMemory(ctx, a, ForgetMemoryInput{ID: "nope"})
	if miss.Forgotten {
		t.Error("unknown id reported as forgotten")
	}
}

func TestRecordMemorySurfacesConflictCandidates(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "preference", Title: "tabs", Body: "always use hard tab characters when indenting Go code in this project",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "preference", Title: "spaces", Body: "always use two spaces when indenting Go code in this project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ConflictCandidates) == 0 {
		t.Fatal("middle-band conflict must surface in record_memory output")
	}
	if out.Note == "" || !strings.Contains(out.Note, "forget_memory") {
		t.Errorf("note must teach the resolution path, got %q", out.Note)
	}
}

// drainSyncSignal reports whether a's long-lived-serve sync loop was signaled
// (a.SyncTrigger has a pending wakeup), draining it if so. a.SyncTrigger is
// always allocated (app.Open), even with no RECARTO_SYNC_URL/KEY configured —
// exactly the state newTestApp builds — so this checks handler-side signaling
// in isolation from whether a real Syncer is wired up.
func drainSyncSignal(a *app.App) bool {
	select {
	case <-a.SyncTrigger:
		return true
	default:
		return false
	}
}

func TestRecordMemorySignalsSyncLoop(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "signals the sync loop on record"}); err != nil {
		t.Fatal(err)
	}
	if !drainSyncSignal(a) {
		t.Error("record_memory must signal the long-lived serve sync loop so it converges promptly")
	}
}

func TestRecordBugSignalsSyncLoop(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordBug(ctx, a, RecordBugInput{ErrorMessage: "boom"}); err != nil {
		t.Fatal(err)
	}
	if !drainSyncSignal(a) {
		t.Error("record_bug must signal the long-lived serve sync loop")
	}
}

func TestCompleteTodoSignalsSyncLoopOnlyWhenCompleted(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	id, _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindTodo, Title: "t", Body: "ship the feature"})
	if err != nil {
		t.Fatal(err)
	}
	// a.Engine.Record is called directly here (bypassing the record_memory MCP
	// handler), so it never signals — only the handler layer does.

	if _, err := completeTodo(ctx, a, CompleteTodoInput{ID: "missing"}); err != nil {
		t.Fatal(err)
	}
	if drainSyncSignal(a) {
		t.Error("completing an unknown id must not signal the sync loop")
	}

	if _, err := completeTodo(ctx, a, CompleteTodoInput{ID: id}); err != nil {
		t.Fatal(err)
	}
	if !drainSyncSignal(a) {
		t.Error("a genuine completion must signal the sync loop")
	}
}

func TestForgetMemorySignalsSyncLoopOnlyWhenFound(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "temp, will be forgotten"})
	if err != nil {
		t.Fatal(err)
	}
	drainSyncSignal(a) // clear the signal recordMemory itself raised

	if _, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: "missing"}); err != nil {
		t.Fatal(err)
	}
	if drainSyncSignal(a) {
		t.Error("forgetting an unknown id must not signal the sync loop")
	}

	if _, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: rec.ID}); err != nil {
		t.Fatal(err)
	}
	if !drainSyncSignal(a) {
		t.Error("a genuine forget must signal the sync loop")
	}
}
