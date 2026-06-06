package agents

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestFormatSessionContextIncludesCaveat(t *testing.T) {
	results := []memory.RecallResult{{
		Record:       &memory.Record{Kind: memory.KindLearning, Title: "ghost", Branch: "feat/x"},
		Score:        0.5,
		DanglingRefs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}}
	out := formatSessionContext(results, graph.Architecture{}, nil, 1500)
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

	out := formatSessionContext(nil, graph.Architecture{Modules: mods}, nil, 1500)
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
	out := formatSessionContext(results, graph.Architecture{}, nil, 1500)
	if !strings.Contains(out, "not present in this worktree") {
		t.Fatalf("expected caveat in pinned do_not_repeat output, got:\n%s", out)
	}
}
