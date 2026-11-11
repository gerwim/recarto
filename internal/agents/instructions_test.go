package agents

import (
	"os"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/features"
)

func TestInstructionsCoachesCandidateSymbols(t *testing.T) {
	if !strings.Contains(Instructions(nil), "candidate_symbols") {
		t.Error("Instructions block should coach agents to pass candidate_symbols to search_graph")
	}
}

func TestInstructionsMentionTodo(t *testing.T) {
	for _, want := range []string{"todo", "complete_todo"} {
		if !strings.Contains(Instructions(nil), want) {
			t.Errorf("Instructions block should mention %q so agents know todos exist", want)
		}
	}
}

func TestInstructionsPromptPreferenceCapture(t *testing.T) {
	// Preferences are signalled conversationally (the user corrects you, states a
	// style/workflow choice, rejects a suggestion) — not by a file edit — so without
	// an explicit capture trigger the agent never records one (rollout had 0). The
	// instructions must name that trigger, like the `todo` bullet does for open work.
	lower := strings.ToLower(Instructions(nil))
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
		if !strings.Contains(strings.ToLower(Instructions(nil)), want) {
			t.Errorf("Instructions should explain just-in-time anchoring; missing %q", want)
		}
	}
}

func TestInstructionsWarnAboutLargeWholeFileReads(t *testing.T) {
	// Agents should know Recarto nudges them toward the graph when they read a large
	// indexed file whole, so the nudge reads as a known guardrail rather than noise.
	lower := strings.ToLower(Instructions(nil))
	if !strings.Contains(lower, "whole-file") || !strings.Contains(lower, "nudge") {
		t.Error("Instructions should warn that a large whole-file read is wasteful and Recarto will nudge toward the graph")
	}
}

func TestInstructionsExplainQualifiedNameCanonical(t *testing.T) {
	// List results omit the redundant short `name` and carry `qualified_name` as the
	// canonical identifier (its last segment is the short name). Agents must know to
	// read qualified_name and feed it back to the symbol-taking tools, rather than
	// expecting a separate `name` field on every row.
	lower := strings.ToLower(Instructions(nil))
	if !strings.Contains(lower, "last segment") || !strings.Contains(lower, "qualified_name") {
		t.Error("Instructions should explain results carry qualified_name as the canonical identifier whose last segment is the short name")
	}
}

func TestInstructionsMentionReportTool(t *testing.T) {
	if !strings.Contains(Instructions(nil), "report_recarto_issue") {
		t.Fatal("Instructions should encourage report_recarto_issue")
	}
}

func TestInstructionsAdvertiseFullToolset(t *testing.T) {
	for _, want := range []string{
		"get_architecture", "query_graph", "list_memory", "get_memory",
		"forget_memory", "confidence", "find_implementations",
		"search_content", "get_file_outline", "find_annotated",
	} {
		if !strings.Contains(Instructions(nil), want) {
			t.Errorf("Instructions block missing %q", want)
		}
	}
}

// TestInstructionsAllOnUnchanged pins the all-on block to today's text: gating
// must only remove parts, never edit them.
func TestInstructionsAllOnUnchanged(t *testing.T) {
	b, err := os.ReadFile("testdata/instructions.md")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(b), "\r\n", "\n")
	if got := Instructions(nil) + "\n"; got != want {
		t.Errorf("all-on instructions drifted from testdata/instructions.md:\ngot %q\nwant %q", got, want)
	}
	if got, all := Instructions(features.Set{}), Instructions(nil); got != all {
		t.Error("an empty Set must mean every feature on, like nil")
	}
}

func TestInstructionsFollowFeatures(t *testing.T) {
	memoryNames := []string{
		"record_memory", "record_bug", "recall_memory", "search_bugs",
		"get_memory", "list_memory", "complete_todo", "forget_memory",
	}
	graphNames := []string{
		"search_graph", "trace_call_path", "get_architecture", "query_graph",
		"search_content", "get_code_snippet", "get_file_outline",
		"find_implementations", "find_annotated", "find_references", "find_*",
	}
	for _, tc := range []struct {
		name    string
		f       features.Set
		absent  []string
		present []string
	}{
		{
			name:    "memory-tools",
			f:       off(features.MemoryTools),
			absent:  append(memoryNames, "Capture preferences", "**Anchor**", "reinforces near-duplicate"),
			present: []string{"search_graph", "report_recarto_issue", "At session start"},
		},
		{
			name:    "todos",
			f:       off(features.Todos),
			absent:  []string{"complete_todo", "Track open work"},
			present: []string{"record_memory", "recall_memory", "search_graph"},
		},
		{
			name:    "graph-tools",
			f:       off(features.GraphTools),
			absent:  append(graphNames, "code graph"),
			present: []string{"record_memory", "complete_todo", "report_recarto_issue"},
		},
		{
			name:    "issue-reports",
			f:       off(features.IssueReports),
			absent:  []string{"report_recarto_issue"},
			present: []string{"record_memory", "search_graph"},
		},
		{
			name:    "memory-tools and graph-tools",
			f:       off(features.MemoryTools, features.GraphTools),
			absent:  append(append([]string{}, memoryNames...), graphNames...),
			present: []string{"# Recarto", "At session start", "report_recarto_issue"},
		},
		{
			name:    "memory-tools, graph-tools and issue-reports",
			f:       off(features.MemoryTools, features.GraphTools, features.IssueReports),
			absent:  append(append(append([]string{}, memoryNames...), graphNames...), "report_recarto_issue"),
			present: []string{"# Recarto", "At session start"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Instructions(tc.f)
			if !strings.HasPrefix(got, "# Recarto\n\nThis repo uses **Recarto**") {
				t.Errorf("dropped parts must not break the intro:\n%s", got)
			}
			if strings.Contains(got, "\n\n\n") {
				t.Errorf("dropped parts left a hole:\n%s", got)
			}
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Errorf("%q still named with the feature off:\n%s", s, got)
				}
			}
			for _, s := range tc.present {
				if !strings.Contains(got, s) {
					t.Errorf("%q missing with the feature off:\n%s", s, got)
				}
			}
		})
	}
}
