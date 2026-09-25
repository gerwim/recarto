package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
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

func TestHandleStopNudgePromptsPreferences(t *testing.T) {
	// Preferences are signalled conversationally (the user corrects your approach or
	// states a style/workflow choice), not by a file edit, so they are the kind most
	// often missed in the moment. The end-of-session nudge is the backstop, so it must
	// explicitly prompt for them — not just learnings and bug fixes.
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(res.SystemMessage), "preference") {
		t.Errorf("stop nudge should prompt for preferences, got: %q", res.SystemMessage)
	}
}

func TestHandleStopNudgesFeedback(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "report_recarto_issue") {
		t.Fatalf("Stop message missing feedback nudge:\n%s", res.SystemMessage)
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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

type augmentCountingEmbedder struct{ calls int }

func (e *augmentCountingEmbedder) ID() string { return "augment-test" }
func (e *augmentCountingEmbedder) Embed(string) []float32 {
	e.calls++
	return []float32{1, 0}
}

func TestAugmentSkipsSemanticEmbedding(t *testing.T) {
	a := newTestApp(t, "main")
	fake := &augmentCountingEmbedder{}
	a.Engine.SetEmbedder(fake)
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow — fix: clamp",
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Handle(ctx, a, Event{Kind: KindAugment, Pattern: "Widget"}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Fatalf("augment Embed calls = %d, want 0", fake.calls)
	}
}

func TestAugmentDedupsBugAlreadySurfacedOnRead(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	// A bug both anchored to a file (so post-read surfaces it) and matching a
	// search term (so augment recall finds it).
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	s, _ := formatSessionContext(results, "", nil, nil, graph.Architecture{}, nil, nil, 0, 0, 60)
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
	s, _ := formatSessionContext(results, "", nil, nil, graph.Architecture{}, nil, nil, 0, 0, 2)
	if strings.Contains(s, "WontFitAtAll") {
		t.Errorf("entry should be dropped entirely when even its title exceeds the budget:\n%s", s)
	}
	if strings.Contains(s, "no prior memories yet") {
		t.Errorf("results were non-empty; must not claim none exist:\n%s", s)
	}
}

func TestStopNudgesOnceWhenEditsButNoMemories(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = a.Ledger.Incr(ctx, "ns1", "edits", 1)
	}
	res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Block || !strings.Contains(res.Reason, "recorded nothing") {
		t.Fatalf("3 edits + 0 memories must block-once with the nudge, got %+v", res)
	}
	// Second stop: never nudge twice.
	res, err = Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block || res.Reason != "" {
		t.Fatalf("second stop must pass through, got %+v", res)
	}
}

func TestStopDoesNotNudgeBelowThresholdOrWithMemories(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	// Below threshold: 2 edits, no nudge.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns2", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns2", "edits", 2)
	if res, _ := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns2"}); res.Block {
		t.Fatalf("2 edits must not nudge, got %+v", res)
	}

	// At threshold but a memory was recorded: no nudge.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns3", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns3", "edits", 3)
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: "learned something", Branch: a.Git.Branch,
		Worktree: a.Git.Worktree, Author: "test", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	if res, _ := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns3"}); res.Block {
		t.Fatalf("a recorded memory must satisfy the nudge, got %+v", res)
	}
}

func TestStopDoesNotNudgeWhenRecordOnlyReinforced(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	clk := a.Clock.(*clock.Fake)

	// A prior session left this memory behind, created before this session starts.
	body := "always run gofmt before committing because CI rejects unformatted files"
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: body, Branch: a.Git.Branch,
		Worktree: a.Git.Worktree, Author: "test", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	clk.Advance(time.Minute)
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "nr1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "nr1", "edits", 3)

	// The session records the same learning again. Recarto's contract is "record
	// freely — near-duplicates reinforce", so a reinforcement IS a recording and
	// must satisfy the capture nudge exactly like a fresh insert.
	clk.Advance(time.Second)
	_, reinforced, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: body, Branch: a.Git.Branch,
		Worktree: a.Git.Worktree, Author: "test", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reinforced {
		t.Fatal("test setup: second record must reinforce, not insert")
	}

	res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "nr1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block {
		t.Fatalf("a reinforced record must satisfy the capture nudge, got block with reason %q", res.Reason)
	}
}

func TestStopActiveLoopGuardPassesThrough(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns4", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns4", "edits", 5)
	res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns4", StopActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block {
		t.Fatal("stop_hook_active must always pass through (loop guard)")
	}
}

func TestSessionStartShowsDeltaSinceLastWorkingSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	// Point the app at a real temp dir so graph.Reconcile can walk real files.
	wt := t.TempDir()
	a.Git.Worktree = wt

	// Advance the fake clock so each session gets a distinct started_at.
	clk := a.Clock.(*clock.Fake)

	// Session 1 does real work: start it, then index a file, then re-index it changed.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	// Write and reconcile initial version.
	if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte("package p\nfunc Work() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	// Advance clock so the reindex delta timestamp falls inside s1's window.
	clk.Advance(time.Second)
	// Reindex with a change so deltas are journaled.
	if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte("package p\nfunc Work() { _ = 1 }\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Advance clock to a new instant for session 2.
	clk.Advance(time.Second)

	// Session 2 is a no-op (/clear): no deltas in its window.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s2", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}

	// Advance clock to a new instant for session 3.
	clk.Advance(time.Second)

	// Session 3 is also a no-op: still no deltas in its window.
	// This is the genuine /clear-skip stress case: s2 is now a candidate for
	// "last prior session with started_at < s3.started_at", but only the EXISTS
	// clause (no deltas) correctly skips it and walks back to s1.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s3", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}

	// Advance clock to a new instant for session 4.
	clk.Advance(time.Second)

	// Session 4 must still show session 1's changes even with two consecutive no-ops
	// between it and the last working session. The EXISTS clause is what makes this
	// work — without it, s3 (started_at < s4, no deltas) would be picked as the
	// baseline, yielding an empty delta tier.
	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s4", Agent: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Changed since your last working session") ||
		(!strings.Contains(res.Context, "Extra") && !strings.Contains(res.Context, "~")) {
		t.Errorf("session 4 context missing the delta tier (EXISTS clause must skip both no-op sessions):\n%s", res.Context)
	}
}

func TestDeltaTierSurvivesManyNoOpSessions(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := t.TempDir()
	a.Git.Worktree = wt
	clk := a.Clock.(*clock.Fake)

	// Session w1 does real work: index a file, then reindex it changed.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "w1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte("package p\nfunc Work() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte("package p\nfunc Work() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Eleven consecutive no-op sessions (/clear spam, quick questions): more
	// than the 10-session retention window, so a prune keyed only on the
	// 10th-most-recent session row would erase w1's journal — exactly the rows
	// the unbounded baseline walk still reports.
	for i := range 11 {
		clk.Advance(time.Second)
		if _, err := Handle(ctx, a, Event{Kind: KindSessionStart,
			SessionID: fmt.Sprintf("noop%d", i), Agent: "claude-code"}); err != nil {
			t.Fatal(err)
		}
	}

	clk.Advance(time.Second)
	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "final", Agent: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Changed since your last working session") {
		t.Errorf("delta tier must survive any number of no-op sessions (prune must not pass the baseline):\n%s", res.Context)
	}
}
