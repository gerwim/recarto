package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/features"
)

// toolNames lists the tools NewServer offers a client for a.
func toolNames(t *testing.T, a *app.App) map[string]bool {
	t.Helper()
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	ss, err := NewServer(a).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Wait() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
	}
	return names
}

func TestToolsFollowFeatures(t *testing.T) {
	a := newTestApp(t, "main")
	groups := map[*features.Feature][]string{
		features.MemoryTools: {"record_memory", "record_bug", "recall_memory", "search_bugs", "get_memory",
			"list_memory", "complete_todo", "forget_memory"},
		features.GraphTools: {"search_graph", "trace_call_path", "get_architecture", "query_graph", "search_content",
			"get_code_snippet", "get_file_outline", "find_implementations", "find_annotated", "find_references", "get_graph_delta"},
		features.IssueReports: {"report_recarto_issue"},
		features.Todos:        {"complete_todo"},
	}
	all := toolNames(t, a)
	grouped := map[string]bool{}
	for _, names := range groups {
		for _, n := range names {
			grouped[n] = true
		}
	}
	for n := range all {
		if !grouped[n] {
			t.Errorf("tool %s belongs to no feature: register it under one in NewServer", n)
		}
	}
	for f, names := range groups {
		a.Features = features.Set{f: false}
		got := toolNames(t, a)
		for _, n := range names {
			if got[n] {
				t.Errorf("%s off: %s still offered", f.Key, n)
			}
		}
		if len(got) != len(all)-len(names) {
			t.Errorf("%s off: %d tools, want %d", f.Key, len(got), len(all)-len(names))
		}
	}
}

func TestTodosOffRejectsTodoMemories(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	hasTodo := func() bool {
		for _, v := range recordMemorySchema(a.Features).Properties["kind"].Enum {
			if v == "todo" {
				return true
			}
		}
		return false
	}
	if !hasTodo() {
		t.Fatal("todos on: todo missing from the kind enum")
	}
	a.Features = features.Set{features.Todos: false}
	if hasTodo() {
		t.Error("todos off: todo still in the kind enum")
	}
	if _, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "todo", Body: "finish it"}); err == nil || !strings.Contains(err.Error(), "todos are off") {
		t.Errorf("todos off: recording a todo = %v", err)
	}
	if _, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "other kinds still record"}); err != nil {
		t.Error(err)
	}
}
