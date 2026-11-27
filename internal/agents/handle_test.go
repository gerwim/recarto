package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/memory"
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

// largeReadResult returns a tool-result string whose estimated token count is
// safely above the large-read nudge threshold.
func largeReadResult() string { return strings.Repeat("abcd ", 4000) } // ~5263 tokens

// writeLines writes an n-line Go file (one func, then comments) into dir and
// returns its absolute path and content.
func writeLines(t *testing.T, dir, name string, n int) (string, []byte) {
	t.Helper()
	var b strings.Builder
	b.WriteString("package p\nfunc Alpha() {}\n")
	for i := 3; i <= n; i++ {
		fmt.Fprintf(&b, "// line %d\n", i)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, []byte(b.String())
}

func TestPostReadRepeatNotice(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	big, _ := writeLines(t, dir, "big.go", 3000)
	// A step is a read (ranged, first, last; want = expect the notice) or, with
	// change set, a rewrite of big.go to a new mtime.
	type step struct {
		change      bool
		ranged      bool
		first, last int
		want        bool
		wantText    string
	}
	read := func(ranged bool, first, last int, want bool) step {
		return step{ranged: ranged, first: first, last: last, want: want}
	}
	changed := 0
	cases := map[string][]step{
		"disjoint ranges":    {read(true, 100, 149, false), read(true, 2000, 2049, false), read(true, 900, 949, false)},
		"same range twice":   {read(true, 100, 149, false), {ranged: true, first: 100, last: 149, want: true, wantText: "lines 100-149 of big.go"}},
		"whole then part":    {read(false, 1, 3000, false), read(true, 100, 149, true)},
		"part then whole":    {read(true, 1, 100, false), read(false, 1, 3000, false)},
		"changed in between": {read(false, 1, 3000, false), {change: true}, read(false, 1, 3000, false), {first: 1, last: 3000, want: true, wantText: "all of big.go"}},
		"adjacent union":     {read(true, 1, 50, false), read(true, 51, 100, false), read(true, 40, 60, true)},
		"unknown lines":      {read(false, 1, 3000, false), read(true, 0, 0, false)},
	}
	notices := int64(0)
	for name, steps := range cases {
		for i, st := range steps {
			if st.change {
				changed++
				// Same lines (so "whole" still means 1-3000), at a distinct fixed mtime.
				writeLines(t, dir, "big.go", 3000)
				mt := time.Unix(1_600_000_000+int64(changed), 0)
				if err := os.Chtimes(big, mt, mt); err != nil {
					t.Fatal(err)
				}
				continue
			}
			res, err := Handle(ctx, a, Event{Kind: KindPostRead, Agent: "claude-code", SessionID: name, FilePath: big,
				ReadRanged: st.ranged, ReadFirst: st.first, ReadLast: st.last, ToolResult: "x"})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(res.Context, "you already have")
			if got != st.want {
				t.Errorf("%s step %d: notice = %v, want %v:\n%s", name, i, got, st.want, res.Context)
			}
			if got {
				notices++
				if !strings.Contains(res.Context, st.wantText) {
					t.Errorf("%s step %d: notice lacks %q:\n%s", name, i, st.wantText, res.Context)
				}
			}
			if st.wantText == "all of big.go" && strings.Contains(res.Context, "lines ") {
				t.Errorf("%s step %d: a whole-file notice names lines:\n%s", name, i, res.Context)
			}
		}
	}
	if n, _ := a.Ledger.Get(ctx, "lifetime", "repeated_reads_blocked"); n != notices {
		t.Errorf("repeated_reads_blocked = %d, want %d", n, notices)
	}
}

func TestPostReadLargeNudge(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	big, src := writeLines(t, dir, "big.go", 3000)
	big2, src2 := writeLines(t, dir, "big2.go", 3000)
	for rel, b := range map[string][]byte{"big.go": src, "big2.go": src2} {
		if err := a.Graph.IndexFile(ctx, dir, rel, b, 1, int64(len(b))); err != nil {
			t.Fatal(err)
		}
	}
	nudge := func(session, file string, ranged bool, first, last int) bool {
		t.Helper()
		res, err := Handle(ctx, a, Event{Kind: KindPostRead, Agent: "claude-code", SessionID: session, FilePath: file,
			ReadRanged: ranged, ReadFirst: first, ReadLast: last, ToolResult: largeReadResult()})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(res.Context, "get_file_outline")
	}
	if nudge("a", big, true, 100, 149) {
		t.Error("a partial ranged read got the nudge")
	}
	if !nudge("b", big, false, 1, 2000) {
		t.Error("a read with no range (cut by the host) got no nudge")
	}
	if !nudge("c", big, true, 1, 3000) {
		t.Error("a ranged read covering the whole file got no nudge")
	}
	if !nudge("d", big, false, 1, 3000) {
		t.Error("the session's first whole-file read got no nudge")
	}
	if nudge("d", big2, false, 1, 3000) {
		t.Error("a second file in the same session got the nudge again")
	}
	before, _ := a.Ledger.Get(ctx, "lifetime", "large_reads_nudged")

	var wg sync.WaitGroup
	got := make([]bool, 8)
	errs := make([]error, 8)
	for i := range got {
		file := big
		if i%2 == 1 {
			file = big2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Handle(ctx, a, Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "e", FilePath: file,
				ToolResult: largeReadResult()})
			got[i], errs[i] = strings.Contains(res.Context, "get_file_outline"), err
		}()
	}
	wg.Wait()
	won := 0
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if got[i] {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d competing reads got the nudge, want 1", won)
	}
	if n, _ := a.Ledger.Get(ctx, "lifetime", "large_reads_nudged"); n != 4 || n != before+1 {
		t.Errorf("large_reads_nudged = %d (before the race %d), want 4", n, before)
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

func TestAugmentSuppressesRepeatedPattern(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	indexWidgetBug(t, a)
	ev := Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"}

	first, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Context, "Widget overflow") {
		t.Fatalf("first augment should surface the anchored bug:\n%s", first.Context)
	}

	second, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if second.Context != "" {
		t.Errorf("repeated augment = %q, want nothing", second.Context)
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
	indexWidgetBug(t, a)
	fake.calls = 0
	res, err := Handle(ctx, a, Event{Kind: KindAugment, Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Widget overflow") {
		t.Fatalf("augment should surface the anchored bug:\n%s", res.Context)
	}
	if fake.calls != 0 {
		t.Fatalf("augment Embed calls = %d, want 0", fake.calls)
	}
}

func TestAugmentDedupsBugAlreadySurfacedOnRead(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	indexSource(t, a, "widget.go", "package p\n\nfunc Widget() {}\n")
	recordBug(t, a, "Widget overflow", "clamp", memory.Ref{Type: memory.RefFile, Value: "widget.go"})

	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: "widget.go", ToolResult: "package p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Context, "Widget overflow") {
		t.Fatalf("post-read should surface the anchored bug:\n%s", read.Context)
	}

	// The bug already sits in the transcript from the read.
	aug, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aug.Context, "widget.go:3") || strings.Contains(aug.Context, "Widget overflow") {
		t.Fatalf("augment must list the symbol but not a bug already surfaced on read:\n%s", aug.Context)
	}
}

func TestPostReadDedupsBugAlreadySurfacedByAugment(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	indexSource(t, a, "widget.go", "package p\n\nfunc Widget() {}\n")
	recordBug(t, a, "Widget overflow", "clamp", memory.Ref{Type: memory.RefFile, Value: "widget.go"})

	aug, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "s1", Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aug.Context, "Widget overflow") {
		t.Fatalf("augment should surface the bug anchored to the symbol's file:\n%s", aug.Context)
	}

	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s1", FilePath: "widget.go", ToolResult: "package p"})
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
	indexWidgetBug(t, a)
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

func grepHint(t *testing.T, a *app.App, session, pattern string) string {
	t.Helper()
	res, err := Handle(context.Background(), a, Event{Kind: KindAugment, SessionID: session, Pattern: pattern})
	if err != nil {
		t.Fatal(err)
	}
	return res.Context
}

func TestSearchHintListsExactNamesOnly(t *testing.T) {
	a := newTestApp(t, "main")
	indexSource(t, a, "w.go", "package p\n\nfunc Widget() {}\n\nfunc WidgetFactory() {}\n\nfunc widget() {}\n\nfunc Project() {}\n\nfunc Reference() {}\n")
	for _, p := range []string{"Widget", `\.Widget\b`} {
		got := grepHint(t, a, "", p)
		if strings.Count(got, "\n- [") != 1 || !strings.Contains(got, "] Widget  w.go:3") {
			t.Errorf("%q should list Widget only:\n%s", p, got)
		}
	}
	for _, p := range []string{"ProjectReference", "AGENTS.md|CLAUDE.md"} {
		if got := grepHint(t, a, "", p); got != "" {
			t.Errorf("%q listed %q, want nothing", p, got)
		}
	}
}

func TestSearchHintCapsSymbolsProductFirst(t *testing.T) {
	a := newTestApp(t, "main")
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "x_test.go", "testdata/f.go"} {
		indexSource(t, a, f, "package p\n\nfunc Run() {}\n")
	}
	got := grepHint(t, a, "", "Run")
	if n := strings.Count(got, "\n- ["); n != 5 {
		t.Fatalf("%d symbols listed, want 5:\n%s", n, got)
	}
	last := strings.LastIndex(got, "\n- [")
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if i := strings.Index(got, "  "+f+":"); i < 0 || i > last {
			t.Errorf("product %s missing or after the test/fixture line:\n%s", f, got)
		}
	}
}

func TestSearchHintListsValidAnchoredBugs(t *testing.T) {
	a := newTestApp(t, "main")
	indexSource(t, a, "h.go", "package p\n\nfunc handleAugment() {}\n")
	title := strings.Repeat("t", 330)
	fix := strings.Repeat("f", 900)
	recordBug(t, a, title, fix, memory.Ref{Type: memory.RefSymbol, Value: "handleAugment"})
	recordBug(t, a, "handleAugment overflow", "keyword only")
	recordBug(t, a, "gone code bug", "stale", memory.Ref{Type: memory.RefSymbol, Value: "handleAugment"},
		memory.Ref{Type: memory.RefFile, Value: "gone.go"})

	got := grepHint(t, a, "s", "handleAugment")
	if !strings.Contains(got, "\n- ⚠ "+title[:160]+"…") {
		t.Errorf("anchored bug missing or not cut to 160 runes:\n%s", got)
	}
	if strings.Count(got, "- ⚠") != 1 || strings.Contains(got, "fff") ||
		strings.Contains(got, "handleAugment overflow") || strings.Contains(got, "gone code bug") {
		t.Errorf("want only the anchored bug's title, no fix, no keyword or flagged bug:\n%s", got)
	}
}

func TestSearchHintBugCapAndMarksOnlyPrinted(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	indexSource(t, a, "h.go", "package p\n\nfunc Handle() {}\n")
	titles := []string{"bug one", "bug two", "bug three"}
	for _, ti := range titles {
		recordBug(t, a, ti, "fix", memory.Ref{Type: memory.RefFile, Value: "h.go"})
	}
	hint := grepHint(t, a, "s", "Handle")
	if n := strings.Count(hint, "- ⚠"); n != 2 {
		t.Fatalf("%d bugs listed, want 2:\n%s", n, hint)
	}
	read, err := handlePostRead(ctx, a, Event{Kind: KindPostRead, SessionID: "s", FilePath: "h.go", ToolResult: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ti := range titles {
		if strings.Contains(hint, ti) == strings.Contains(read.Context, ti) {
			t.Errorf("%q should show exactly once across hint and read:\nhint: %s\nread: %s", ti, hint, read.Context)
		}
	}
}

func TestSearchHintSizeCap(t *testing.T) {
	a, dir := newTreeApp(t)
	long := strings.Repeat("deep/", 12)
	names := []string{"AlphaWidgetFactoryServiceProvider", "BetaWidgetFactoryServiceProvider",
		"GammaWidgetFactoryServiceProvider", "DeltaWidgetFactoryServiceProvider",
		"EpsilonWidgetFactoryServiceProvider", "ZetaWidgetFactoryServiceProvider"}
	for i := 0; i < 3; i++ {
		rel := fmt.Sprintf("%sfile_with_a_rather_long_name_%d.go", long, i)
		indexSource(t, a, rel, fmt.Sprintf("package p\n\nfunc %s() {}\n\nfunc %s() {}\n", names[2*i], names[2*i+1]))
		recordBug(t, a, strings.Repeat(string(rune('a'+i)), 330), "fix", memory.Ref{Type: memory.RefFile, Value: rel})
	}
	cmd := fmt.Sprintf("rg -e '%s|%s' . && grep -rn '%s\\|%s' . && git grep -n '%s|%s'",
		names[0], names[1], names[2], names[3], names[4], names[5])
	got := shellHook(t, a, Event{Command: cmd, CWD: dir})
	if len(got) > 800 {
		t.Errorf("hint is %d bytes, want ≤ 800:\n%s", len(got), got)
	}
	if n := strings.Count(got, "\n- ["); n == 0 || n > hintSymbolMax {
		t.Errorf("%d symbols listed:\n%s", n, got)
	}
	// Uncapped, five symbol lines and two bug titles alone pass the cap.
	if 5*len("\n- [function] "+names[0]+"  "+long+"file_with_a_rather_long_name_0.go:3")+2*330 <= 800 {
		t.Fatal("setup no longer exceeds the cap")
	}
}

func TestSearchHintFitsTheCapExactly(t *testing.T) {
	const header = "Recarto: symbols named in your search:"
	for _, extra := range []int{0, 1} {
		a := newTestApp(t, "main")
		a.Features = off(features.GraphTools)
		lineFixed := len("\n- [function] Widget  ") + len(".go:3")
		rel := strings.Repeat("p", hintMax-len(header)-lineFixed+extra) + ".go"
		indexSource(t, a, rel, "package p\n\nfunc Widget() {}\n")
		got := grepHint(t, a, "", "Widget")
		if extra == 0 && len(got) != hintMax {
			t.Errorf("hint at the cap is %d bytes, want %d:\n%s", len(got), hintMax, got)
		}
		if extra == 1 && got != "" {
			t.Errorf("a symbol line one byte over the cap was listed:\n%s", got)
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
	s, _ := formatSessionContext(memoryHeader, "", results, "", nil, graph.Architecture{}, "", nil, nil, 0, 0, 60, nil)
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
	s, _ := formatSessionContext(memoryHeader, "", results, "", nil, graph.Architecture{}, "", nil, nil, 0, 0, 2, nil)
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

func TestEditDuringAnotherIndexShowsInTheNextSessionStart(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := t.TempDir()
	a.Git.Worktree = wt
	clk := a.Clock.(*clock.Fake)
	w := filepath.Join(wt, "w.go")
	if err := os.WriteFile(w, []byte("package p\nfunc Work() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finishIndex(t, a, wt)
	clk.Advance(time.Second)

	// Another index of the worktree holds its index lock through S1 and S2.
	gated := newGated(graph.FSSource{Root: wt}, false)
	indexed := make(chan error, 1)
	go func() { indexed <- a.Graph.ReconcileIfStale(ctx, wt, gated, "other-token") }()
	await(t, "index", gated.entered)

	hook := func(kind EventKind, session string) Result {
		t.Helper()
		res, err := Handle(ctx, a, Event{Kind: kind, SessionID: session, Agent: "claude-code", FilePath: w})
		if err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
		return res
	}
	hook(KindSessionStart, "s1")
	if err := os.WriteFile(w, []byte("package p\nfunc Work() {}\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook(KindPostEdit, "s1")
	hook(KindStop, "s1")
	hook(KindSessionStart, "s2")
	hook(KindStop, "s2")
	close(gated.gate)
	if err := await(t, "index", indexed); err != nil {
		t.Fatal(err)
	}

	res := hook(KindSessionStart, "s3")
	if !strings.Contains(res.Context, "Changed since your last working session here") || !strings.Contains(res.Context, "Extra") {
		t.Errorf("S3's start does not list S1's edit:\n%s", res.Context)
	}
	var s1 int64
	if err := a.Store.DB.QueryRow(`SELECT started_at FROM sessions WHERE id='s1'`).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := a.Graph.DeltaBaseline(ctx, wt, clk.Now().UnixMilli()); err != nil || !ok || got != s1 {
		t.Errorf("get_graph_delta baseline = %d, %v, %v; want S1's start %d", got, ok, err, s1)
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

func TestHookNotesUseHourly(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	clk := a.Clock.(*clock.Fake)
	mark := func() int64 {
		t.Helper()
		if _, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "s1", Pattern: "Foo"}); err != nil {
			t.Fatal(err)
		}
		var v int64
		if err := a.Store.DB.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key=?`,
			"worktree_used:"+a.Git.Worktree).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	t0 := clk.Now().UnixMilli()
	if got := mark(); got != t0 {
		t.Fatalf("mark after the first hook = %d, want T0", got)
	}
	clk.Advance(30 * time.Minute)
	if got := mark(); got != t0 {
		t.Errorf("mark after a hook 30 min later = %d, want T0", got)
	}
	clk.Advance(31 * time.Minute)
	if got, want := mark(), clk.Now().UnixMilli(); got != want {
		t.Errorf("mark after a hook 61 min later = %d, want %d", got, want)
	}
}

func TestReadAndEditNotesShowUntitledBodies(t *testing.T) {
	a, wt := newTreeApp(t)
	ctx := context.Background()
	writeIndexed(t, a, "big.go", []byte("package p\n\nfunc Big() {}\n"))
	type note struct{ body, prefix string }
	notes := map[memory.Kind]note{
		memory.KindDoNotRepeat: {"Do not run VACUUM while another process writes.", "🚫 do-not-repeat for big.go: "},
		memory.KindDecision:    {"Keep one writer per store.", "📌 prior decision for big.go: "},
		memory.KindLearning:    {"WAL readers never block the writer.", "📚 prior learning for big.go: "},
		memory.KindTodo:        {"Split the index queue.", "📝 open todo for big.go: "},
		memory.KindBug:         {"Reindex drops the last edge.", "⚠ prior bug in big.go: "},
	}
	for k, n := range notes {
		if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: k, Body: n.body, Branch: a.Git.Branch,
			Refs: []memory.Ref{{Type: memory.RefFile, Value: "big.go"}}}); err != nil {
			t.Fatal(err)
		}
	}
	for session, kind := range map[string]EventKind{"read": KindPostRead, "edit": KindPostEdit} {
		res, err := Handle(ctx, a, Event{Kind: kind, SessionID: session, FilePath: filepath.Join(wt, "big.go")})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range notes {
			if want := n.prefix + n.body; !strings.Contains(res.Context, want) {
				t.Errorf("%s: missing %q:\n%s", session, want, res.Context)
			}
		}
		if strings.Contains(res.Context, "(untitled)") {
			t.Errorf("%s: note shows (untitled):\n%s", session, res.Context)
		}
	}
}

func TestSearchHintShowsUntitledBugBody(t *testing.T) {
	a := newTestApp(t, "main")
	indexSource(t, a, "h.go", "package p\n\nfunc handleAugment() {}\n")
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{Kind: memory.KindBug,
		Body: "Hint drops the last line.", Branch: a.Git.Branch,
		Refs: []memory.Ref{{Type: memory.RefSymbol, Value: "handleAugment"}}}); err != nil {
		t.Fatal(err)
	}
	got := grepHint(t, a, "s", "handleAugment")
	if !strings.Contains(got, "\n- ⚠ Hint drops the last line.") || strings.Contains(got, "(untitled)") {
		t.Errorf("untitled bug hint does not show its body:\n%s", got)
	}
}

// lateIndex holds another process's index of a worktree, so a post-edit there
// is deferred to it, and drives sessions on a fake clock.
type lateIndex struct {
	t       *testing.T
	a       *app.App
	ctx     context.Context
	wt      string
	clk     *clock.Fake
	src     gatedSource
	indexed chan error
	cancel  context.CancelFunc
}

// newLateIndex indexes pkg/x.go and pkg/y.go, then starts an index another
// process holds at its listing.
func newLateIndex(t *testing.T) *lateIndex {
	t.Helper()
	return newLateIndexFiles(t, map[string]string{
		"pkg/x.go": "package pkg\n\nfunc XBase() {}\n",
		"pkg/y.go": "package pkg\n\nfunc YBase() {}\n",
	})
}

// newLateIndexFiles indexes files, then starts an index another process holds
// at its listing.
func newLateIndexFiles(t *testing.T, files map[string]string) *lateIndex {
	t.Helper()
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	for rel, src := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	finishIndex(t, a, wt)
	clk := a.Clock.(*clock.Fake)
	clk.Advance(time.Second)
	src := newGated(graph.FSSource{Root: wt}, false)
	indexed := make(chan error, 1)
	ctx := context.Background()
	indexCtx, cancel := context.WithCancel(ctx)
	go func() { indexed <- a.Graph.ReconcileIfStale(indexCtx, wt, src, "other-token") }()
	await(t, "index", src.entered)
	return &lateIndex{t: t, a: a, ctx: ctx, wt: wt, clk: clk, src: src, indexed: indexed, cancel: cancel}
}

// release lets the held index run and awaits it.
func (f *lateIndex) release() {
	f.t.Helper()
	close(f.src.gate)
	if err := await(f.t, "index", f.indexed); err != nil {
		f.t.Fatal(err)
	}
}

func (f *lateIndex) hook(kind EventKind, session, file, source string) Result {
	f.t.Helper()
	res, err := Handle(f.ctx, f.a, Event{Kind: kind, SessionID: session, Agent: "claude-code", FilePath: file, Source: source})
	if err != nil {
		f.t.Fatal(err)
	}
	f.clk.Advance(time.Second)
	return res
}

// edit appends functions to rel and reports the edit in session.
func (f *lateIndex) edit(session, rel string, fns ...string) {
	f.t.Helper()
	p := filepath.Join(f.wt, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, fn := range fns {
		b = append(b, []byte("\nfunc "+fn+"() {}\n")...)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.hook(KindPostEdit, session, p, "")
}

func (f *lateIndex) rc() *app.RepoContext { return f.a.AllRepos()[0] }

// toolRead is the MCP tool's own range code: LiveRange + DeltasIn.
func toolRead(t *testing.T, a *app.App, rc *app.RepoContext) map[string]string {
	t.Helper()
	ctx := context.Background()
	r, ok, err := LiveRange(ctx, a, rc)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if !ok {
		return out
	}
	ds, _, err := rc.Graph.DeltasIn(ctx, rc.Git.Worktree, r, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		out[d.QualifiedName] = d.Change
	}
	return out
}

func names(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// lateSetup runs S1's deferred edit of x.go (Extra) and S2's start while the
// index is held.
func lateSetup(t *testing.T) *lateIndex {
	t.Helper()
	f := newLateIndex(t)
	f.hook(KindSessionStart, "s1", "", "")
	f.edit("s1", "pkg/x.go", "Extra")
	f.hook(KindStop, "s1", "", "")
	f.hook(KindSessionStart, "s2", "", "")
	return f
}

func TestLateIndexedEditShowsAtTheNextStart(t *testing.T) {
	t.Run("S2 edits", func(t *testing.T) {
		f := lateSetup(t)
		f.release()
		f.edit("s2", "pkg/y.go", "Other")
		f.hook(KindStop, "s2", "", "")
		block := f.hook(KindSessionStart, "s3", "", "").Context
		for _, want := range []string{"Extra", "Other"} {
			if !strings.Contains(block, want) {
				t.Errorf("S3's block lacks %s:\n%s", want, block)
			}
		}
		if got, want := names(toolRead(t, f.a, f.rc())), []string{"Extra", "Other"}; !slices.Equal(got, want) {
			t.Errorf("the tool in S3 = %v; want %v", got, want)
		}
	})
	t.Run("tool in S2", func(t *testing.T) {
		f := lateSetup(t)
		f.release()
		if got := toolRead(t, f.a, f.rc()); got["Extra"] != "added" {
			t.Errorf("the tool in S2 = %v; want Extra", got)
		}
		f.edit("s2", "pkg/y.go", "Other")
		f.hook(KindStop, "s2", "", "")
		block := f.hook(KindSessionStart, "s3", "", "").Context
		for _, want := range []string{"Extra", "Other"} {
			if !strings.Contains(block, want) {
				t.Errorf("S3's block lacks %s:\n%s", want, block)
			}
		}
	})
	t.Run("killed index", func(t *testing.T) {
		f := newLateIndex(t)
		f.hook(KindSessionStart, "s1", "", "")
		f.edit("s1", "pkg/x.go", "Extra")
		f.hook(KindStop, "s1", "", "")
		// The index dies before it writes the edit; its gate then lets it return.
		f.cancel()
		close(f.src.gate)
		if err := await(t, "index", f.indexed); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if got := toolRead(t, f.a, f.rc()); len(got) != 0 {
			t.Fatalf("the dead index journaled %v; want nothing", got)
		}
		f.hook(KindSessionStart, "s2", "", "")
		// The background index S2's start launches.
		if err := f.a.Graph.ReconcileIfStale(f.ctx, f.wt, graph.FSSource{Root: f.wt}, "warm-token"); err != nil {
			t.Fatal(err)
		}
		f.edit("s2", "pkg/y.go", "Other")
		f.hook(KindStop, "s2", "", "")
		block := f.hook(KindSessionStart, "s3", "", "").Context
		for _, want := range []string{"Extra", "Other"} {
			if !strings.Contains(block, want) {
				t.Errorf("S3's block lacks %s:\n%s", want, block)
			}
		}
	})
}

func TestLateChangeCountedNotNamed(t *testing.T) {
	files := map[string]string{
		"xdir/x.go": "package xdir\n\nfunc XBase() {}\n",
		"pkg/y.go":  "package pkg\n\nfunc YBase() {}\n",
	}
	for i := 1; i <= 6; i++ {
		d := fmt.Sprintf("d%d", i)
		files[d+"/a.go"] = "package " + d + "\n\nfunc Base() {}\n"
	}
	f := newLateIndexFiles(t, files)
	f.hook(KindSessionStart, "s1", "", "")
	f.edit("s1", "xdir/x.go", "Extra")
	f.hook(KindStop, "s1", "", "")
	f.hook(KindSessionStart, "s2", "", "")
	f.release()
	for i := 1; i <= 6; i++ {
		d := fmt.Sprintf("d%d", i)
		f.edit("s2", d+"/a.go", fmt.Sprintf("Added%dA", i), fmt.Sprintf("Added%dB", i))
	}
	f.hook(KindStop, "s2", "", "")
	block := f.hook(KindSessionStart, "s3", "", "").Context
	if strings.Contains(block, "Extra") {
		t.Errorf("S3's section names the late symbol:\n%s", block)
	}
	if !strings.Contains(block, "more modules") || !strings.Contains(block, "lists all 13") {
		t.Errorf("S3's section does not count all 13 changes:\n%s", block)
	}
	if got := toolRead(t, f.a, f.rc()); got["Extra"] != "added" {
		t.Errorf("the tool in S3 = %v; want Extra", got)
	}
	f.edit("s3", "pkg/y.go", "S3s")
	f.hook(KindStop, "s3", "", "")
	block4 := f.hook(KindSessionStart, "s4", "", "").Context
	if strings.Contains(block4, "Extra") || !strings.Contains(block4, "lists all 1") {
		t.Errorf("S4's section still counts the late change:\n%s", block4)
	}
	if got := toolRead(t, f.a, f.rc()); got["Extra"] != "" {
		t.Errorf("the tool in S4 = %v; want no Extra", got)
	}
}

func TestLateChangeWithoutASection(t *testing.T) {
	f := lateSetup(t)
	f.release()
	f.a.Features[features.RecentChanges] = false
	defer func() { f.a.Features[features.RecentChanges] = true }()
	block := f.hook(KindSessionStart, "s3", "", "").Context
	if strings.Contains(block, "Changed since your last working session here") {
		t.Errorf("S3's block has a recent-changes section:\n%s", block)
	}
	if got := toolRead(t, f.a, f.rc()); got["Extra"] != "added" {
		t.Errorf("the tool in S3 = %v; want Extra", got)
	}
	f.edit("s3", "pkg/y.go", "S3s")
	f.hook(KindStop, "s3", "", "")
	f.a.Features[features.RecentChanges] = true
	block4 := f.hook(KindSessionStart, "s4", "", "").Context
	if strings.Contains(block4, "Extra") {
		t.Errorf("S4's block repeats the late change:\n%s", block4)
	}
	if !strings.Contains(block4, "S3s") {
		t.Errorf("S4's block lacks S3's change:\n%s", block4)
	}
}

func TestChangeIndexedBeforeTheNextStartShowsOnce(t *testing.T) {
	f := newLateIndex(t)
	f.hook(KindSessionStart, "s1", "", "")
	f.edit("s1", "pkg/x.go", "Extra")
	f.hook(KindStop, "s1", "", "")
	f.release()
	block := f.hook(KindSessionStart, "s2", "", "").Context
	if !strings.Contains(block, "Extra") {
		t.Errorf("S2's block lacks Extra:\n%s", block)
	}
	f.edit("s2", "pkg/y.go", "Other")
	f.hook(KindStop, "s2", "", "")
	block3 := f.hook(KindSessionStart, "s3", "", "").Context
	if strings.Contains(block3, "Extra") || !strings.Contains(block3, "Other") {
		t.Errorf("S3's block = want Other without Extra:\n%s", block3)
	}
}

func TestCompactBlockTakesANewMark(t *testing.T) {
	f := lateSetup(t)
	f.release()
	block := f.hook(KindSessionStart, "s2", "", "compact").Context
	if !strings.Contains(block, "Extra") {
		t.Errorf("S2's compact block lacks Extra:\n%s", block)
	}
	f.edit("s2", "pkg/y.go", "Other")
	f.hook(KindStop, "s2", "", "")
	block3 := f.hook(KindSessionStart, "s3", "", "").Context
	if strings.Contains(block3, "Extra") || !strings.Contains(block3, "Other") {
		t.Errorf("S3's block repeats Extra or lacks Other:\n%s", block3)
	}
}

func TestOlderConversationStartedAgainListsTheLateChange(t *testing.T) {
	f := newLateIndex(t)
	f.hook(KindSessionStart, "s1", "", "")
	f.edit("s1", "pkg/x.go", "Extra")
	f.hook(KindStop, "s1", "", "")
	f.hook(KindSessionStart, "s2", "", "")
	f.edit("s2", "pkg/y.go", "Other")
	f.hook(KindStop, "s2", "", "")
	f.hook(KindSessionStart, "s3", "", "")
	f.hook(KindStop, "s3", "", "")
	f.release()
	block := f.hook(KindSessionStart, "s2", "", "compact").Context
	if !strings.Contains(block, "Extra") {
		t.Errorf("S2's compact block lacks Extra:\n%s", block)
	}
	if got := toolRead(t, f.a, f.rc()); got["Extra"] != "added" {
		t.Errorf("the tool right after S2's block = %v; want Extra", got)
	}
	f.hook(KindStop, "s2", "", "")
	block4 := f.hook(KindSessionStart, "s4", "", "").Context
	if strings.Contains(block4, "Extra") {
		t.Errorf("S4's block repeats Extra:\n%s", block4)
	}
}

func TestPrunedLateChangeIsInNeitherTheBlockNorTheTool(t *testing.T) {
	f := newLateIndex(t)
	f.hook(KindSessionStart, "s1", "", "")
	f.edit("s1", "pkg/x.go", "Extra")
	f.hook(KindStop, "s1", "", "")
	for i := 2; i <= 10; i++ {
		id := fmt.Sprintf("s%d", i)
		f.hook(KindSessionStart, id, "", "")
		f.hook(KindStop, id, "", "")
	}
	f.hook(KindSessionStart, "s11", "", "")
	f.release()
	f.edit("s11", "pkg/y.go", "Other")
	f.hook(KindStop, "s11", "", "")
	block := f.hook(KindSessionStart, "s12", "", "").Context
	if strings.Contains(block, "Extra") || !strings.Contains(block, "Other") {
		t.Errorf("S12's block = want Other without Extra:\n%s", block)
	}
	if got := toolRead(t, f.a, f.rc()); got["Extra"] != "" {
		t.Errorf("the tool in S12 = %v; want the pruned change gone", got)
	}
}
