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

func TestInstructionsAdvertiseFullToolset(t *testing.T) {
	for _, want := range []string{
		"get_architecture", "query_graph", "list_memory", "get_memory",
		"forget_memory", "confidence",
	} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions block missing %q", want)
		}
	}
}
