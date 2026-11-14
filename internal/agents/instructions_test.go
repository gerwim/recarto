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
	for _, want := range []string{"anchor", "do-not-repeat", "decision", "read or edited"} {
		if !strings.Contains(strings.ToLower(Instructions(nil)), want) {
			t.Errorf("Instructions should explain just-in-time anchoring; missing %q", want)
		}
	}
}

func TestInstructionsFitBudget(t *testing.T) {
	if n := len(recartoBlockBegin + "\n" + Instructions(nil) + "\n" + recartoBlockEnd); n > 2500 {
		t.Errorf("instructions block is %d characters with every feature on, want at most 2500", n)
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
		"find_implementations", "find_annotated", "find_references", "get_graph_delta",
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
			absent:  append(memoryNames, "**Record**", "**Anchor**", "Near-duplicates"),
			present: []string{"search_graph", "report_recarto_issue", "persists across sessions"},
		},
		{
			name:    "todos",
			f:       off(features.Todos),
			absent:  []string{"complete_todo", "Track open work", "`todo`"},
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
			present: []string{"# Recarto", "persists across sessions", "report_recarto_issue"},
		},
		{
			name:    "memory-tools, graph-tools and issue-reports",
			f:       off(features.MemoryTools, features.GraphTools, features.IssueReports),
			absent:  append(append(append([]string{}, memoryNames...), graphNames...), "report_recarto_issue"),
			present: []string{"# Recarto", "persists across sessions"},
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
