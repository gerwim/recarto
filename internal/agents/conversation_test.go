package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/memory"
)

// handle runs ev through Handle and returns its context.
func handle(t *testing.T, a *app.App, ev Event) string {
	t.Helper()
	res, err := Handle(context.Background(), a, ev)
	if err != nil {
		t.Fatal(err)
	}
	return res.Context
}

func start(t *testing.T, a *app.App, session, source string) string {
	t.Helper()
	return handle(t, a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: session, Source: source})
}

func TestNoteShownToSubagentIsShownToMainAgent(t *testing.T) {
	a := newTestApp(t, "main")
	rel := "internal/agents/handle.go"
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindDoNotRepeat, Title: "never block the hook", Body: "x", Branch: a.Git.Branch, Source: "cli",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}},
	}); err != nil {
		t.Fatal(err)
	}
	const note = "never block the hook"
	read := func(agent string) string {
		return handle(t, a, Event{Kind: KindPostRead, SessionID: "s", AgentID: agent, FilePath: rel, ToolResult: "x"})
	}
	if !strings.Contains(read("sub"), note) {
		t.Error("the subagent's first read did not show the note")
	}
	if strings.Contains(read("sub"), note) {
		t.Error("the subagent's second read showed the note again")
	}
	if got := handle(t, a, Event{Kind: KindPostEdit, SessionID: "s", FilePath: a.Git.Worktree + "/" + rel}); !strings.Contains(got, note) {
		t.Errorf("the main agent's first edit did not show the note: %q", got)
	}
	if strings.Contains(read(""), note) {
		t.Error("the main agent's read after its edit showed the note again")
	}
}

func TestSearchHintedInSubagentIsHintedInMainAgent(t *testing.T) {
	a, dir := newTreeApp(t)
	indexWidgetBug(t, a)
	search := func(agent string) string {
		return shellHook(t, a, Event{SessionID: "s", AgentID: agent, Command: "rg Widget", CWD: dir})
	}
	for _, agent := range []string{"sub", ""} {
		if got := search(agent); !strings.Contains(got, "Widget overflow") || !strings.Contains(got, shellSearchNudge) {
			t.Errorf("agent %q: first search lacks the hint or the nudge:\n%s", agent, got)
		}
	}
	if got := search(""); got != "" {
		t.Errorf("main agent's second search = %q, want nothing", got)
	}
}

func TestLargeFileNudgeOncePerConversation(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	big, src := writeLines(t, dir, "big.go", 3000)
	big2, src2 := writeLines(t, dir, "big2.go", 3000)
	for rel, b := range map[string][]byte{"big.go": src, "big2.go": src2} {
		if err := a.Graph.IndexFile(ctx, dir, rel, b, 1, int64(len(b))); err != nil {
			t.Fatal(err)
		}
	}
	nudged := func(agent, file string) bool {
		return strings.Contains(handle(t, a, Event{Kind: KindPostRead, SessionID: "s", AgentID: agent, FilePath: file,
			ReadFirst: 1, ReadLast: 3000, ToolResult: largeReadResult()}), "get_file_outline")
	}
	if !nudged("", big) {
		t.Error("the main agent's whole read got no nudge")
	}
	if !nudged("sub", big2) {
		t.Error("the subagent's first whole read got no nudge")
	}
	if nudged("sub", big) {
		t.Error("the subagent got the nudge twice")
	}
}

func TestResumeAfterStartupAddsNothingUnchanged(t *testing.T) {
	t.Setenv("RECARTO_SESSION_BUDGET", "2") // drops the non-pinned decision from the block
	a := newWorkspaceTestApp(t, "main")
	ctx := context.Background()
	rel := "internal/agents/handle.go"
	for _, nm := range []memory.NewMemory{
		{Kind: memory.KindDoNotRepeat, Title: "pinned rule P", Body: "x", Pinned: true},
		{Kind: memory.KindDecision, Title: "decision D", Body: "x", Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}},
	} {
		nm.Branch, nm.Source = a.Git.Branch, "cli"
		if _, _, _, err := a.Engine.Record(ctx, nm); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := a.Workspace.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindPreference, Title: "workspace W", Body: "x", Source: "mcp",
	}); err != nil {
		t.Fatal(err)
	}
	got := start(t, a, "s", "startup")
	if !strings.Contains(got, "pinned rule P") || !strings.Contains(got, "workspace W") || strings.Contains(got, "decision D") {
		t.Fatalf("startup block:\n%s", got)
	}
	if got := start(t, a, "s", "resume"); got != "" {
		t.Errorf("resume with nothing new added:\n%s", got)
	}
	if got := handle(t, a, Event{Kind: KindPostRead, SessionID: "s", FilePath: rel, ToolResult: "x"}); !strings.Contains(got, "decision D") {
		t.Errorf("a decision dropped from the block was not shown on read: %q", got)
	}
}

func TestResumeAddsOnlyMemoryRecordedSince(t *testing.T) {
	a, _ := newTreeApp(t)
	record(t, a, memory.KindLearning, "learning A about caches", "")
	if got := start(t, a, "s", "startup"); !strings.Contains(got, "Code map") {
		t.Fatalf("startup block has no code map:\n%s", got)
	}
	record(t, a, memory.KindLearning, "learning B about queues", "")
	want := "Recarto memory for this repo/branch:\n- [learning] learning B about queues\n  learning B about queues body"
	if got := start(t, a, "s", "resume"); got != want {
		t.Errorf("resume:\n%s\nwant:\n%s", got, want)
	}
	if got := start(t, a, "s", "resume"); got != "" {
		t.Errorf("second resume:\n%s", got)
	}
}

func TestFullBlockForUnknownResumeAndFork(t *testing.T) {
	a := newTestApp(t, "main")
	record(t, a, memory.KindLearning, "wal needs busy timeout", "")
	full := func(got string) bool {
		return strings.Contains(got, "wal needs busy timeout") && strings.Contains(got, "Record new learnings")
	}
	if got := start(t, a, "fresh", "resume"); !full(got) {
		t.Errorf("resume of an unknown session:\n%s", got)
	}
	start(t, a, "s", "startup")
	if got := start(t, a, "s", "fork"); !full(got) {
		t.Errorf("fork:\n%s", got)
	}
}

func TestCompactAndClearForgetTheConversation(t *testing.T) {
	// The tiny budget keeps the decision out of the block, so only the read can show it.
	t.Setenv("RECARTO_SESSION_BUDGET", "2")
	for _, source := range []string{"compact", "clear"} {
		t.Run(source, func(t *testing.T) {
			a, dir := newTreeApp(t)
			big, src := writeLines(t, dir, "big.go", 3000)
			if err := a.Graph.IndexFile(context.Background(), dir, "big.go", src, 1, int64(len(src))); err != nil {
				t.Fatal(err)
			}
			record(t, a, memory.KindDecision, "keep big.go flat", "big.go")
			read := func() string {
				return handle(t, a, Event{Kind: KindPostRead, SessionID: "s", FilePath: big,
					ReadFirst: 1, ReadLast: 3000, ToolResult: largeReadResult()})
			}
			if got := read(); !strings.Contains(got, "keep big.go flat") || !strings.Contains(got, "get_file_outline") {
				t.Fatalf("first read:\n%s", got)
			}
			if got := start(t, a, "s", source); !strings.Contains(got, "Record new learnings") {
				t.Fatalf("%s gave no full block:\n%s", source, got)
			}
			got := read()
			if strings.Contains(got, "you already have") || !strings.Contains(got, "keep big.go flat") || strings.Contains(got, "get_file_outline") {
				t.Errorf("read after %s:\n%s", source, got)
			}
		})
	}
}

func TestResumeStillRunsSessionStartUpkeep(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.MkdirAll(filepath.Join(wt, ".recarto"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".recarto", "memory.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a.Features = off(features.TeamSnapshot, features.AutoCompact)
	start(t, a, "s", "startup")
	if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte("package p\nfunc Zeta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Features = nil
	start(t, a, "s", "resume")
	for _, key := range []string{"snapshot_hash", "compact_last_run_at"} {
		var v string
		if err := a.Store.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
			t.Errorf("meta %s after resume: %v", key, err)
		}
	}
	hits, err := a.Graph.Search(ctx, wt, "Zeta", "", "", 8)
	if err != nil || len(hits) == 0 {
		t.Errorf("Zeta after resume: %v, %v", hits, err)
	}
}

func TestSubagentEditsCountForStopNudge(t *testing.T) {
	a := newTestApp(t, "main")
	start(t, a, "s", "startup")
	for _, agent := range []string{"", "", "sub"} {
		handle(t, a, Event{Kind: KindPostEdit, SessionID: "s", AgentID: agent, FilePath: "/repo/main.go"})
	}
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Block {
		t.Errorf("3 edits across conversations did not nudge: %+v", res)
	}
}

func TestSearchHintSkipsBugShownInRecentBugs(t *testing.T) {
	a, dir := newTreeApp(t)
	// On disk: session start reconciles the graph from the tree.
	if err := os.WriteFile(filepath.Join(dir, "h.go"), []byte("package x\n\nfunc handleAugment() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const title = "recent-only bug on handleAugment"
	recordBug(t, a, title, "guard", memory.Ref{Type: memory.RefSymbol, Value: "handleAugment"})
	a.Features = off(features.SessionMemories)

	if got := start(t, a, "s", "startup"); !strings.Contains(got, "Recent bugs") || !strings.Contains(got, title) {
		t.Fatalf("session start lacks the bug in Recent bugs:\n%s", got)
	}
	hint := handle(t, a, Event{Kind: KindAugment, SessionID: "s", Pattern: "handleAugment"})
	if !strings.Contains(hint, "handleAugment  h.go:3") {
		t.Fatalf("search no longer lists the symbol:\n%s", hint)
	}
	if strings.Contains(hint, title) || strings.Contains(hint, "Known bugs in this code") {
		t.Errorf("search repeats a bug session start showed:\n%s", hint)
	}
}
