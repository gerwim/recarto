package agents

import (
	"strings"
	"testing"
)

func TestInstructionsCoachesCandidateSymbols(t *testing.T) {
	if !strings.Contains(Instructions, "candidate_symbols") {
		t.Error("Instructions block should coach agents to pass candidate_symbols to search_graph")
	}
}

func TestInstructionsMentionTodo(t *testing.T) {
	for _, want := range []string{"todo", "complete_todo"} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions block should mention %q so agents know todos exist", want)
		}
	}
}

func TestInstructionsExplainJustInTimeAnchoring(t *testing.T) {
	// Agents must learn that anchoring a memory to files/symbols (via record_memory
	// refs) is what makes bugs, do-not-repeats and decisions resurface the moment
	// that code is touched — otherwise they record guidance with no refs and it
	// never fires just-in-time.
	for _, want := range []string{"anchor", "do-not-repeat", "decision"} {
		if !strings.Contains(strings.ToLower(Instructions), want) {
			t.Errorf("Instructions should explain just-in-time anchoring; missing %q", want)
		}
	}
}

func TestInstructionsAdvertiseFullToolset(t *testing.T) {
	for _, want := range []string{
		"get_architecture", "query_graph", "list_memory", "get_memory",
		"forget_memory", "confidence", "find_implementations",
		"search_content", "get_file_outline", "find_annotated",
	} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions block missing %q", want)
		}
	}
}
