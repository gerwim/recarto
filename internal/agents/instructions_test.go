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

func TestInstructionsPromptPreferenceCapture(t *testing.T) {
	// Preferences are signalled conversationally (the user corrects you, states a
	// style/workflow choice, rejects a suggestion) — not by a file edit — so without
	// an explicit capture trigger the agent never records one (rollout had 0). The
	// instructions must name that trigger, like the `todo` bullet does for open work.
	lower := strings.ToLower(Instructions)
	for _, want := range []string{"preference", "correct", "style"} {
		if !strings.Contains(lower, want) {
			t.Errorf("Instructions should give a preference-capture trigger; missing %q", want)
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

func TestInstructionsWarnAboutLargeWholeFileReads(t *testing.T) {
	// Agents should know Recarto nudges them toward the graph when they read a large
	// indexed file whole, so the nudge reads as a known guardrail rather than noise.
	lower := strings.ToLower(Instructions)
	if !strings.Contains(lower, "whole-file") || !strings.Contains(lower, "nudge") {
		t.Error("Instructions should warn that a large whole-file read is wasteful and Recarto will nudge toward the graph")
	}
}

func TestInstructionsExplainQualifiedNameCanonical(t *testing.T) {
	// List results omit the redundant short `name` and carry `qualified_name` as the
	// canonical identifier (its last segment is the short name). Agents must know to
	// read qualified_name and feed it back to the symbol-taking tools, rather than
	// expecting a separate `name` field on every row.
	lower := strings.ToLower(Instructions)
	if !strings.Contains(lower, "last segment") || !strings.Contains(lower, "qualified_name") {
		t.Error("Instructions should explain results carry qualified_name as the canonical identifier whose last segment is the short name")
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
