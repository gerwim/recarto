package agents

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/memory"
)

// toolNamed matches any MCP tool name as a whole word.
var toolNamed = regexp.MustCompile(`\b(` + strings.Join(toolNames, "|") + `)\b`)

// assertNamesRecartoTools fails unless every tool text names is preceded, on
// its line, by "recarto MCP tool", and text names at least one tool.
func assertNamesRecartoTools(t *testing.T, what, text string) {
	t.Helper()
	named := false
	for _, line := range strings.Split(text, "\n") {
		for _, m := range toolNamed.FindAllStringIndex(line, -1) {
			named = true
			if !strings.Contains(line[:m[0]], "recarto MCP tool") {
				t.Errorf("%s: %q is not called a recarto MCP tool:\n%s", what, line[m[0]:m[1]], line)
			}
		}
	}
	if !named {
		t.Errorf("%s names no tool:\n%s", what, text)
	}
}

// writeIndexed writes src as rel in a's worktree and indexes it.
func writeIndexed(t *testing.T, a *app.App, rel string, src []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.Git.Worktree, rel), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.IndexFile(context.Background(), a.Git.Worktree, rel, src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
}

func TestNoServerKeepsFactsDropsPointers(t *testing.T) {
	a, dir := newUnservedTreeApp(t)
	ctx := context.Background()
	indexWidgetBug(t, a)

	hint := handle(t, a, Event{Kind: KindAugment, SessionID: "g", Pattern: "Widget"})
	if !strings.Contains(hint, "widget.go:3") || !strings.Contains(hint, "Known bugs in this code:\n- ⚠ Widget overflow") {
		t.Errorf("search hint lost its facts:\n%s", hint)
	}
	if strings.Contains(hint, "recarto MCP tool") || toolNamed.MatchString(hint) || len(hint) > hintMax {
		t.Errorf("search hint points at a tool or exceeds %d bytes:\n%s", hintMax, hint)
	}

	_, src := writeLines(t, dir, "big.go", 20)
	writeIndexed(t, a, "big.go", src)
	record(t, a, memory.KindBug, "alpha overflows", "big.go")
	record(t, a, memory.KindDoNotRepeat, "never inline beta", "big.go")
	record(t, a, memory.KindTodo, "rename alpha", "big.go")
	read := handle(t, a, Event{Kind: KindPostRead, SessionID: "r", FilePath: filepath.Join(dir, "big.go"), ReadFirst: 1, ReadLast: 20})
	for _, want := range []string{"prior bug in big.go: alpha overflows", "do-not-repeat for big.go", "open todo for big.go"} {
		if !strings.Contains(read, want) {
			t.Errorf("read notes lack %q:\n%s", want, read)
		}
	}
	if toolNamed.MatchString(read) {
		t.Errorf("read notes point at a tool:\n%s", read)
	}

	record(t, a, memory.KindLearning, "wal mode needs busy timeout", "")
	finishIndex(t, a, dir)
	start(t, a, "s1", "startup")
	clk := a.Clock.(*clock.Fake)
	for _, src := range []string{"package p\nfunc Work() {}\n", "package p\nfunc Work() { _ = 1 }\nfunc Extra() {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := a.Graph.Reconcile(ctx, dir, graph.FSSource{Root: dir}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	block := start(t, a, "s2", "startup")
	for _, want := range []string{"wal mode needs busy timeout", "Code map (modules)", "Recent bugs:\n", "Changed since your last working session"} {
		if !strings.Contains(block, want) {
			t.Errorf("session start lacks %q:\n%s", want, block)
		}
	}
	for _, gone := range []string{"Record new learnings", "Query the code graph", "recarto MCP tool"} {
		if strings.Contains(block, gone) {
			t.Errorf("session start without a server still has %q:\n%s", gone, block)
		}
	}
	if toolNamed.MatchString(block) {
		t.Errorf("session start without a server points at a tool:\n%s", block)
	}
}

func TestNoServerNudgesAreNotCounted(t *testing.T) {
	a, dir := newUnservedTreeApp(t)
	ctx := context.Background()
	indexWidgetBug(t, a)
	_, src := writeLines(t, dir, "big.go", 3000)
	writeIndexed(t, a, "big.go", src)
	_, src2 := writeLines(t, dir, "big2.go", 3000)
	writeIndexed(t, a, "big2.go", src2)
	largeRead := func(rel string) string {
		return handle(t, a, Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s", FilePath: filepath.Join(dir, rel),
			ReadFirst: 1, ReadLast: 3000, ToolResult: largeReadResult()})
	}
	stop := func() Result {
		res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "s"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Memories recorded before the session must not count as recorded in it.
	a.Clock.(*clock.Fake).Advance(time.Second)
	start(t, a, "s", "startup")
	for range stopNudgeEditThreshold {
		handle(t, a, Event{Kind: KindPostEdit, SessionID: "s", FilePath: filepath.Join(dir, "widget.go")})
	}

	if got := shellHook(t, a, Event{SessionID: "s", Command: "rg Widget", CWD: dir}); !strings.Contains(got, "widget.go:3") || strings.Contains(got, shellSearchNudge) {
		t.Errorf("shell search without a server: want the hint without the nudge:\n%s", got)
	}
	if got := largeRead("big.go"); strings.Contains(got, "is large") {
		t.Errorf("large read without a server got the nudge:\n%s", got)
	}
	if res := stop(); res != (Result{}) {
		t.Errorf("stop without a server = %+v, want nothing", res)
	}
	if n, err := a.Ledger.Get(ctx, "s", "stop_nudged"); err != nil || n != 0 {
		t.Errorf("stop_nudged = %d, %v; want 0", n, err)
	}

	markServing(t, a)
	if got := shellHook(t, a, Event{SessionID: "s", Command: "rg Gadget", CWD: dir}); !strings.Contains(got, shellSearchNudge) {
		t.Errorf("first shell search with a server lacks the nudge:\n%s", got)
	}
	if got := largeRead("big2.go"); !strings.Contains(got, "is large") {
		t.Errorf("first large read with a server lacks the nudge:\n%s", got)
	}
	if res := stop(); !res.Block || !strings.Contains(res.Reason, "recorded nothing") {
		t.Errorf("first stop with a server = %+v, want the block", res)
	}
}

func TestPointersNameTheRecartoServer(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	indexWidgetBug(t, a)

	instr := Instructions(nil)
	if i, j := strings.Index(instr, "`recarto` MCP server"), toolNamed.FindStringIndex(instr); i < 0 || j == nil || i > j[0] {
		t.Errorf("the instructions block must name the recarto MCP server before its first tool:\n%s", instr)
	}

	assertNamesRecartoTools(t, "search hint", handle(t, a, Event{Kind: KindAugment, SessionID: "g", Pattern: "Widget"}))
	assertNamesRecartoTools(t, "shell nudge", shellHook(t, a, Event{SessionID: "sh", Command: "rg Widget", CWD: dir}))

	_, src := writeLines(t, dir, "big.go", 3000)
	writeIndexed(t, a, "big.go", src)
	record(t, a, memory.KindBug, "alpha overflows", "big.go")
	record(t, a, memory.KindDoNotRepeat, "never inline beta", "big.go")
	record(t, a, memory.KindTodo, "rename alpha", "big.go")
	read := handle(t, a, Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "r", FilePath: filepath.Join(dir, "big.go"),
		ReadFirst: 1, ReadLast: 3000, ToolResult: largeReadResult()})
	for _, want := range []string{"is large", "prior bug in big.go", "do-not-repeat for big.go", "open todo for big.go"} {
		if !strings.Contains(read, want) {
			t.Fatalf("read lacks %q:\n%s", want, read)
		}
	}
	assertNamesRecartoTools(t, "read notes", read)

	block, _ := formatSessionContext(memoryHeader, "",
		[]memory.RecallResult{{Record: &memory.Record{Kind: memory.KindLearning, Title: "wal mode needs busy timeout", Branch: "main"}, Score: 0.5}},
		"", nil,
		graph.Architecture{Modules: []graph.Module{{Name: "internal", Files: 3, Symbols: 9}}}, "",
		[]*memory.Record{{ID: "b1", Kind: memory.KindBug, Title: "reindex drops edges"}},
		[]graph.Delta{{Path: "internal/x.go", QualifiedName: "X", Kind: "function", Change: "added"}}, 1, 1749340800000, 1500, nil)
	assertNamesRecartoTools(t, "session start", block)

	stop := func(session string, edits int64) Result {
		// Memories recorded before the session must not count as recorded in it.
		a.Clock.(*clock.Fake).Advance(time.Second)
		start(t, a, session, "startup")
		if err := a.Ledger.Incr(ctx, session, "edits", edits); err != nil {
			t.Fatal(err)
		}
		res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: session})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	assertNamesRecartoTools(t, "stop reminder", stop("quiet", 0).SystemMessage)
	if res := stop("busy", 3); !res.Block {
		t.Errorf("stop after 3 edits did not block: %+v", res)
	} else {
		assertNamesRecartoTools(t, "stop reason", res.Reason)
	}
}
