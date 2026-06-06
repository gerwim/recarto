package agents

import (
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestHandleSessionStartEmptyRepo(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "opencode", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Recarto memory for this repo/branch") {
		t.Errorf("missing session context header:\n%s", res.Context)
	}
	if !strings.Contains(res.Context, "record_memory") {
		t.Errorf("missing record nudge:\n%s", res.Context)
	}
	// The session row must carry the adapter-supplied agent name.
	var agent string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT agent FROM sessions WHERE id='s1'`).Scan(&agent); err != nil {
		t.Fatalf("session row not created: %v", err)
	}
	if agent != "opencode" {
		t.Errorf("sessions.agent = %q, want opencode", agent)
	}
}

func TestHandleStopNudge(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "copilot", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "record any new learnings") {
		t.Errorf("missing stop nudge: %q", res.SystemMessage)
	}
}

func TestHandlePostReadWarnsOnRepeat(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: "internal/store/store.go", ToolResult: "package store"}

	first, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Context, "already read") {
		t.Errorf("first read should not warn:\n%s", first.Context)
	}
	second, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Context, "already read") {
		t.Errorf("second read should warn:\n%s", second.Context)
	}
}

// largeReadResult returns a tool-result string whose estimated token count is
// safely above the large-read nudge threshold.
func largeReadResult() string { return strings.Repeat("abcd ", 4000) } // ~5263 tokens

func TestHandlePostReadNudgesGraphForLargeIndexedFile(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := a.Git.Worktree
	src := []byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() int { return 1 }\n\nfunc Gamma() {}\n")
	if err := a.Graph.IndexFile(ctx, wt, "big.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: wt + "/big.go", ToolResult: largeReadResult()}
	res, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "get_file_outline") || !strings.Contains(res.Context, "get_code_snippet") {
		t.Errorf("expected graph-navigation nudge for a large indexed file:\n%s", res.Context)
	}
	if n, _ := a.Ledger.Get(ctx, "lifetime", "large_reads_nudged"); n != 1 {
		t.Errorf("large_reads_nudged = %d, want 1", n)
	}
}

func TestHandlePostReadNoGraphNudgeForSmallRead(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := a.Git.Worktree
	src := []byte("package p\n\nfunc Alpha() {}\n")
	if err := a.Graph.IndexFile(ctx, wt, "small.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: wt + "/small.go", ToolResult: "package p"}
	res, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Context, "get_file_outline") {
		t.Errorf("small read should not trigger the graph nudge:\n%s", res.Context)
	}
}

func TestHandlePostReadNoGraphNudgeForUnindexedFile(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := a.Git.Worktree
	// Large read, but the file is not in the graph (e.g. a data/text file), so
	// get_file_outline/get_code_snippet offer no cheaper alternative — stay silent.
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: wt + "/data.bin", ToolResult: largeReadResult()}
	res, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Context, "get_file_outline") {
		t.Errorf("unindexed file should not trigger the graph nudge:\n%s", res.Context)
	}
}

func TestHandlePostReadGraphNudgeOncePerFile(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := a.Git.Worktree
	src := []byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	if err := a.Graph.IndexFile(ctx, wt, "big.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: wt + "/big.go", ToolResult: largeReadResult()}
	if _, err := Handle(ctx, a, ev); err != nil {
		t.Fatal(err)
	}
	// The second read of the same file gets the stronger repeated-read warning and
	// must not also re-inject the (now-redundant) graph nudge.
	second, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Context, "already read") {
		t.Errorf("repeat read should warn:\n%s", second.Context)
	}
	if strings.Contains(second.Context, "get_file_outline") {
		t.Errorf("repeat read should not re-inject the graph nudge:\n%s", second.Context)
	}
	if n, _ := a.Ledger.Get(ctx, "lifetime", "large_reads_nudged"); n != 1 {
		t.Errorf("large_reads_nudged = %d, want 1 (nudge once per file)", n)
	}
}

func TestAugmentSuppressesRepeatedPattern(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	// Record a bug so the augment hook has injectable content (the empty test
	// graph yields no symbol hits).
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow — fix: clamp",
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"}

	first, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Context, "Widget overflow") {
		t.Fatalf("first augment should surface the matching bug:\n%s", first.Context)
	}

	second, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Context, "Widget overflow") {
		t.Errorf("repeated augment should not re-inject the same block:\n%s", second.Context)
	}
	if !strings.Contains(second.Context, "already searched") {
		t.Errorf("repeated augment should emit a short reminder:\n%s", second.Context)
	}
	n, _ := a.Ledger.Get(ctx, "lifetime", "repeated_augments_blocked")
	if n != 1 {
		t.Errorf("repeated_augments_blocked = %d, want 1", n)
	}
}

func TestAugmentDedupsBugAlreadySurfacedOnRead(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	// A bug both anchored to a file (so post-read surfaces it) and matching a
	// search term (so augment recall finds it).
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch,
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}

	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Context, "Widget overflow") {
		t.Fatalf("post-read should surface the anchored bug:\n%s", read.Context)
	}

	// A later search matching the same bug must NOT re-inject it — it is already
	// sitting in the transcript from the read. Pure token waste otherwise.
	aug, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(aug.Context, "Widget overflow") {
		t.Fatalf("augment must not re-inject a bug already surfaced on read:\n%s", aug.Context)
	}
}

func TestPostReadDedupsBugAlreadySurfacedByAugment(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch,
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}

	aug, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aug.Context, "Widget overflow") {
		t.Fatalf("augment should surface the matching bug:\n%s", aug.Context)
	}

	// Reading the anchored file must NOT re-inject a bug the search already showed.
	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read.Context, "Widget overflow") {
		t.Fatalf("post-read must not re-inject a bug already surfaced by a search:\n%s", read.Context)
	}
}

func TestAugmentNoDedupWithoutSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow — fix: clamp",
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindAugment, Pattern: "Widget"} // no SessionID → cannot scope dedup

	for i := range 2 {
		res, err := Handle(ctx, a, ev)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Context, "Widget overflow") {
			t.Fatalf("call %d without a session should still inject:\n%s", i, res.Context)
		}
	}
}

func TestSessionBudgetEnvOverride(t *testing.T) {
	t.Setenv("RECARTO_SESSION_BUDGET", "50")
	if got := sessionBudget(); got != 50 {
		t.Errorf("sessionBudget() = %d, want 50", got)
	}
	t.Setenv("RECARTO_SESSION_BUDGET", "")
	if got := sessionBudget(); got != sessionStartBudget {
		t.Errorf("sessionBudget() default = %d, want %d", got, sessionStartBudget)
	}
}

func TestFormatSessionContextDegradesToTitles(t *testing.T) {
	results := []memory.RecallResult{
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "AAA", Body: "short body"}},
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "BBB", Body: strings.Repeat("x", 2000)}},
	}
	s, _ := formatSessionContext(results, graph.Architecture{}, nil, 60)
	if !strings.Contains(s, "AAA") || !strings.Contains(s, "short body") {
		t.Errorf("first entry should render with its body:\n%s", s)
	}
	if !strings.Contains(s, "BBB") {
		t.Errorf("second entry title should still appear:\n%s", s)
	}
	if strings.Contains(s, strings.Repeat("x", 100)) {
		t.Errorf("second entry body should be dropped under tight budget:\n%s", s)
	}
}

func TestPostReadSurfacesTodo(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	rel := "internal/agents/handle.go"
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "split this file", Body: "split handle.go",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "split this file") {
		t.Fatalf("todo not surfaced on read; got: %q", res.Context)
	}
}

func TestPostEditSurfacesGuidance(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDoNotRepeat, Title: "do not hand-roll the lexer", Body: "use tree-sitter",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := handlePostEdit(ctx, a, Event{Kind: KindPostEdit, SessionID: "s1", FilePath: a.Git.Worktree + "/" + rel})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "do not hand-roll the lexer") {
		t.Fatalf("do_not_repeat guidance not surfaced on edit; got: %q", res.Context)
	}
}

func TestJustInTimeSurfacingDedupsPerSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDoNotRepeat, Title: "do not hand-roll the lexer", Body: "use tree-sitter",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"}

	first, err := handlePostRead(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Context, "do not hand-roll the lexer") {
		t.Fatalf("first touch should surface guidance; got %q", first.Context)
	}
	second, err := handlePostRead(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Context, "do not hand-roll the lexer") {
		t.Fatalf("repeated touch must not re-inject the same guidance (token waste); got %q", second.Context)
	}
}

func TestSessionStartSuppressesLaterJustInTimeReinjection(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	// A pinned do_not_repeat is always emitted in the session-start block.
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDoNotRepeat, Title: "do not hand-roll the lexer", Body: "use tree-sitter",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli", Pinned: true,
	}); err != nil {
		t.Fatal(err)
	}

	start, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(start.Context, "do not hand-roll the lexer") {
		t.Fatalf("session-start should inject the pinned guardrail:\n%s", start.Context)
	}

	// Touching the anchored file must NOT re-inject a guardrail already sitting in
	// the session-start block — that is duplicate context, pure token waste.
	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read.Context, "do not hand-roll the lexer") {
		t.Fatalf("guardrail already shown at session-start must not re-inject on read; got %q", read.Context)
	}
}

func TestSessionStartDoesNotSuppressDroppedGuidance(t *testing.T) {
	t.Setenv("RECARTO_SESSION_BUDGET", "2") // too small to render any cerebrum entry
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	// Non-pinned guidance: under the tiny budget the session-start block drops it,
	// so it must NOT be marked surfaced and must still appear when the file is read.
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDecision, Title: "settled: cache lives outside the worktree", Body: "rationale",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	start, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(start.Context, "cache lives outside the worktree") {
		t.Fatalf("budget too small; guidance should have been dropped from the block:\n%s", start.Context)
	}

	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: rel, ToolResult: "package agents"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Context, "cache lives outside the worktree") {
		t.Fatalf("guidance dropped from session-start must still surface just-in-time; got %q", read.Context)
	}
}

func TestFormatSessionContextDropsEntryUnderTinyBudget(t *testing.T) {
	results := []memory.RecallResult{
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "WontFitAtAll", Body: "body"}},
	}
	s, _ := formatSessionContext(results, graph.Architecture{}, nil, 2)
	if strings.Contains(s, "WontFitAtAll") {
		t.Errorf("entry should be dropped entirely when even its title exceeds the budget:\n%s", s)
	}
	if strings.Contains(s, "no prior memories yet") {
		t.Errorf("results were non-empty; must not claim none exist:\n%s", s)
	}
}
