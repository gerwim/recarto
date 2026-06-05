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
