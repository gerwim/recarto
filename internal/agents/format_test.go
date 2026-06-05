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
