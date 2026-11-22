package agents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
)

func TestFormatSessionContextIncludesCaveat(t *testing.T) {
	results := []memory.RecallResult{{
		Record:       &memory.Record{Kind: memory.KindLearning, Title: "ghost", Branch: "feat/x"},
		Score:        0.5,
		DanglingRefs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}}
	out, _ := formatSessionContext(memoryHeader, "", results, "", nil, graph.Architecture{}, "", nil, nil, 0, 0, 1500, nil)
	if !strings.Contains(out, "not present in this worktree") {
		t.Fatalf("expected caveat in session context, got:\n%s", out)
	}
}

func TestFormatTodoReminders(t *testing.T) {
	out := formatTodoReminders("internal/agents/handle.go", []*memory.Record{
		{Title: "refactor the dispatch"},
	})
	if !strings.Contains(out, "internal/agents/handle.go") || !strings.Contains(out, "refactor the dispatch") {
		t.Fatalf("unexpected reminder text: %q", out)
	}
}

func TestFormatGuidanceMarksKindsDistinctly(t *testing.T) {
	loc := "internal/agents/handle.go"
	out := formatGuidance(loc, []*memory.Record{
		{Kind: memory.KindDoNotRepeat, Title: "do not hand-roll the lexer"},
		{Kind: memory.KindDecision, Title: "we chose modernc sqlite"},
	})
	if !strings.Contains(out, loc) {
		t.Fatalf("guidance must name the location: %q", out)
	}
	if !strings.Contains(out, "do not hand-roll the lexer") || !strings.Contains(out, "we chose modernc sqlite") {
		t.Fatalf("guidance must include both titles: %q", out)
	}
	// The do_not_repeat (guardrail) and decision (rationale) lines must read
	// differently so the agent can tell a hard "don't" from a softer rationale.
	dnrLine, decLine := "", ""
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "hand-roll") {
			dnrLine = l
		}
		if strings.Contains(l, "modernc") {
			decLine = l
		}
	}
	if dnrLine == "" || decLine == "" {
		t.Fatalf("both guidance lines must render: %q", out)
	}
	if firstMarker(dnrLine) == firstMarker(decLine) {
		t.Fatalf("do_not_repeat and decision must use distinct markers:\n%s", out)
	}
}

// firstMarker returns the leading non-space rune of a line, the visual marker.
func firstMarker(line string) string {
	line = strings.TrimSpace(line)
	for _, r := range line {
		return string(r)
	}
	return ""
}

func TestFormatSessionContextMapSelectsProductOverTestModules(t *testing.T) {
	var mods []graph.Module
	// Eight alphabetically-first test modules, each large by file count — exactly
	// the noise the old "first 8 by name" map surfaced.
	for _, n := range []string{
		"Aaa.Tests", "Aab.Tests", "Aac.Tests", "Aad.Tests",
		"Aae.Tests", "Aaf.Tests", "Aag.Tests", "Aah.Tests",
	} {
		mods = append(mods, graph.Module{Name: n, Files: 300, Symbols: 1000})
	}
	// One product module, alphabetically last and smaller — must still win a slot.
	mods = append(mods, graph.Module{Name: "Zproduct", Files: 20, Symbols: 80})

	out, _ := formatSessionContext(memoryHeader, "", nil, "", nil, graph.Architecture{Modules: mods}, "", nil, nil, 0, 0, 1500, nil)
	if !strings.Contains(out, "Zproduct") {
		t.Fatalf("product module must be selected over test modules in the orientation map:\n%s", out)
	}
}

func TestFormatSessionContextPinnedDNRIncludesCaveat(t *testing.T) {
	results := []memory.RecallResult{{
		Record: &memory.Record{
			Kind:   memory.KindDoNotRepeat,
			Pinned: true,
			Title:  "dnr",
			Branch: "feat/x",
		},
		Score:        1.0,
		DanglingRefs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}}
	out, _ := formatSessionContext(memoryHeader, "", results, "", nil, graph.Architecture{}, "", nil, nil, 0, 0, 1500, nil)
	if !strings.Contains(out, "not present in this worktree") {
		t.Fatalf("expected caveat in pinned do_not_repeat output, got:\n%s", out)
	}
}

func TestFormatDeltaTierDegradesUnderBudgetWithLongNames(t *testing.T) {
	// Use unqualified names (no dot) so leafName returns the full name without
	// stripping, ensuring the symbol-names line is long enough to exceed the budget
	// before degradation. 3 names of 160 chars each in one module pushes the output
	// to ~157 tokens (> 150 budget), triggering the degradation path.
	long := strings.Repeat("A", 160)
	deltas := []graph.Delta{
		{Path: "internal/the/module/f0.go", QualifiedName: fmt.Sprintf("%s0", long), Kind: "method", Change: "added"},
		{Path: "internal/the/module/f1.go", QualifiedName: fmt.Sprintf("%s1", long), Kind: "method", Change: "added"},
		{Path: "internal/the/module/f2.go", QualifiedName: fmt.Sprintf("%s2", long), Kind: "method", Change: "added"},
	}
	out := formatDeltaTier(deltas, 3, 1749340800000, true)
	if got := ledger.EstimateTokens(out); got > deltaTierBudget {
		t.Errorf("degraded tier costs %d tokens, budget %d:\n%s", got, deltaTierBudget, out)
	}
	if !strings.Contains(out, "(Jun") {
		t.Errorf("degradation must never strip the baseline date from the header:\n%s", out)
	}
	if !strings.Contains(out, "get_graph_delta") {
		t.Errorf("drill-down pointer must survive degradation:\n%s", out)
	}

	out = formatDeltaTier(deltas, 3, 1749340800000, false)
	if got := ledger.EstimateTokens(out); got > deltaTierBudget {
		t.Errorf("degraded tier costs %d tokens, budget %d:\n%s", got, deltaTierBudget, out)
	}
	if !strings.Contains(out, "(Jun") {
		t.Errorf("degradation must never strip the baseline date from the header:\n%s", out)
	}
	if strings.Contains(out, "get_graph_delta") {
		t.Errorf("graph-tools off must not point at get_graph_delta:\n%s", out)
	}
}

func TestFormatDeltaTier(t *testing.T) {
	deltas := []graph.Delta{
		{Path: "internal/mcp/graph.go", QualifiedName: "findReferences", Kind: "function", Change: "added"},
		{Path: "internal/mcp/graph.go", QualifiedName: "getCodeSnippet", Kind: "function", Change: "changed"},
		{Path: "internal/parse/parse.go", QualifiedName: "ParseFile", Kind: "function", Change: "changed"},
	}
	out := formatDeltaTier(deltas, 3, 1749340800000, true) // 2025-06-08 (any fixed ms)
	if !strings.Contains(out, "Changed since your last working session") {
		t.Errorf("missing tier header: %q", out)
	}
	if !strings.Contains(out, "internal/mcp: +1 ~1") {
		t.Errorf("missing per-module counts: %q", out)
	}
	if !strings.Contains(out, "findReferences") {
		t.Errorf("heaviest module should name symbols: %q", out)
	}
	if !strings.Contains(out, "get_graph_delta") {
		t.Errorf("must point at the drill-down tool: %q", out)
	}
	if got := ledger.EstimateTokens(out); got > deltaTierBudget {
		t.Errorf("tier costs %d tokens, budget %d", got, deltaTierBudget)
	}

	out = formatDeltaTier(deltas, 3, 1749340800000, false)
	if !strings.Contains(out, "Changed since your last working session") || !strings.Contains(out, "internal/mcp: +1 ~1") {
		t.Errorf("graph-tools off must keep the tier and its counts: %q", out)
	}
	if strings.Contains(out, "get_graph_delta") {
		t.Errorf("graph-tools off must not point at get_graph_delta: %q", out)
	}

	if formatDeltaTier(nil, 0, 0, true) != "" {
		t.Error("empty delta must render nothing at all")
	}
}

func TestFormatBugWarningsCutsFix(t *testing.T) {
	fix := strings.Repeat("a", 200) + strings.Repeat("b", 700)
	bug := &memory.Record{ID: "m1", Kind: memory.KindBug, Title: "boom", Extra: map[string]any{"fix": fix}}
	out := formatBugWarnings("h.go", []*memory.Record{bug}, true)
	if !strings.Contains(out, "fix: "+fix[:200]+"…") || strings.Contains(out, fix[:201]) {
		t.Errorf("fix not cut to 200 runes:\n%s", out)
	}
	if !strings.HasSuffix(out, "(full text: recarto MCP tool `get_memory`, id=m1)") {
		t.Errorf("missing get_memory pointer:\n%s", out)
	}
	if out := formatBugWarnings("h.go", []*memory.Record{bug}, false); strings.Contains(out, "get_memory") {
		t.Errorf("pointer shown with memory tools off:\n%s", out)
	}
}

func TestOneLineNotesCutLongTitles(t *testing.T) {
	title := strings.Repeat("x", 160) + strings.Repeat("y", 170)
	cut := title[:160] + "…"
	rec := func(k memory.Kind) *memory.Record { return &memory.Record{ID: string(k), Kind: k, Title: title} }
	pinnedBody := strings.Repeat("keep ", 100)
	mem, _ := formatMemories([]memory.RecallResult{
		{Record: &memory.Record{ID: "l", Kind: memory.KindLearning, Title: title, Body: strings.Repeat("long body ", 400)}},
		{Record: &memory.Record{ID: "d", Kind: memory.KindDoNotRepeat, Pinned: true, Title: title, Body: pinnedBody}},
	}, cerebrumReserve+60)
	recent, _ := formatSessionContext(memoryHeader, "", nil, "", nil, graph.Architecture{}, "", []*memory.Record{rec(memory.KindBug)}, nil, 0, 0, 1500, nil)
	for name, out := range map[string]string{
		"bug warning": formatBugWarnings("h.go", []*memory.Record{rec(memory.KindBug)}, false),
		"guidance": formatGuidance("h.go", []*memory.Record{
			rec(memory.KindDoNotRepeat), rec(memory.KindDecision), rec(memory.KindLearning)}),
		"todo":        formatTodoReminders("h.go", []*memory.Record{rec(memory.KindTodo)}),
		"recent bugs": recent,
		"memories":    mem,
	} {
		if !strings.Contains(out, cut) || strings.Contains(out, title[:161]) {
			t.Errorf("%s: title not cut to 160 runes:\n%s", name, out)
		}
	}
	if !strings.Contains(mem, "- [learning] "+cut+"\n- [") || strings.Contains(mem, "long body") {
		t.Errorf("budget fallback is not the one-line title entry:\n%s", mem)
	}
	if !strings.Contains(mem, pinnedBody) {
		t.Errorf("pinned do-not-repeat body was cut:\n%s", mem)
	}
}

func TestNoteTitleFallsBackToBodyStart(t *testing.T) {
	long := strings.Repeat("é", 200) + strings.Repeat("x", 130)
	r160 := strings.Repeat("ü", 160)
	r170 := strings.Repeat("ü", 170)
	for _, tc := range []struct{ title, body, want string }{
		{"Keep WAL on", "different body text", "Keep WAL on"},
		{"", "Do not run VACUUM while another process writes.", "Do not run VACUUM while another process writes."},
		{"   ", "A short rule.", "A short rule."},
		{"", "First line.\nSecond line.", "First line.…"},
		{"", "\n\n  First line.\r\nSecond", "First line.…"},
		{"", "First line.\n\n  ", "First line."},
		{"", long, strings.Repeat("é", 160) + "…"},
		{"", r160 + "\nmore", r160 + "…"},
		{"", r170 + "\nmore", r160 + "…"},
		{"", "  \n  ", "(untitled)"},
	} {
		if got := noteTitle(&memory.Record{Title: tc.title, Body: tc.body}); got != tc.want {
			t.Errorf("noteTitle(%q, %q) = %q, want %q", tc.title, tc.body, got, tc.want)
		}
	}
}

func TestRecentBugsListShowsUntitledBodyStart(t *testing.T) {
	bug := &memory.Record{ID: "b1", Kind: memory.KindBug, Body: "Reindex drops the last edge."}
	out, _ := formatSessionContext(memoryHeader, "", nil, "", nil, graph.Architecture{}, "", []*memory.Record{bug}, nil, 0, 0, 1500, nil)
	if !strings.Contains(out, "\n- Reindex drops the last edge.\n") {
		t.Errorf("recent bugs list does not show the untitled bug's body:\n%s", out)
	}
	var src shareSource
	bugSteps(&src, []*memory.Record{bug}, false)
	if items := src.steps[2].items; len(items) != 1 || items[0].text != "- Reindex drops the last edge.\n" {
		t.Errorf("shared recent bugs = %+v, want the untitled bug's body", items)
	}
}

func TestSessionStartMemoryEntriesKeepUntitledHeading(t *testing.T) {
	learning := memory.RecallResult{Record: &memory.Record{ID: "l", Kind: memory.KindLearning, Body: "WAL readers never block the writer."}}
	results := []memory.RecallResult{
		{Record: &memory.Record{ID: "d", Kind: memory.KindDoNotRepeat, Pinned: true, Body: "Do not run VACUUM while another process writes."}},
		learning,
	}
	ws := []memory.RecallResult{{Record: &memory.Record{ID: "c", Kind: memory.KindDecision, Body: "Keep one writer per store."}}}
	out, _ := formatSessionContext(memoryHeader, "", results, "w", ws, graph.Architecture{}, "", nil, nil, 0, 0, 1500, nil)
	for _, want := range []string{
		"- [learning] (untitled)\n  WAL readers never block the writer.\n",
		"- [do_not_repeat] (untitled)\n  Do not run VACUUM while another process writes.\n",
		"- [decision] (untitled)\n  Keep one writer per store.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	_, full, _, title := memoryTexts(learning, "repo x")
	if full != "- [learning, repo x] (untitled)\n  WAL readers never block the writer.\n" || title != "- [learning, repo x] (untitled)\n" {
		t.Errorf("shared entry: full %q, title %q", full, title)
	}
}
