package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerwim/recarto/internal/app"
)

// pyJSONLen is the length of v written the way Python's json.dumps(v) writes
// it by default (", " and ": " separators, non-ASCII escaped as \uXXXX), the
// measure docs/validation/2026-11-14-tool-definitions.md uses for definition sizes.
func pyJSONLen(t *testing.T, v any) int {
	t.Helper()
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(enc.Bytes()), "", ""); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(buf.String(), ",\n", ", ")
	s = strings.ReplaceAll(s, "\n", "")
	n := 0
	for _, r := range s {
		switch {
		case r < 0x80:
			n++
		case r <= 0xFFFF:
			n += 6
		default:
			n += 12
		}
	}
	return n
}

func properties(tl *sdk.Tool) map[string]any {
	props, _ := tl.InputSchema.(map[string]any)["properties"].(map[string]any)
	return props
}

// paramDescs lists the descriptions of tl's parameters.
func paramDescs(tl *sdk.Tool) []string {
	var out []string
	for _, p := range properties(tl) {
		if d, ok := p.(map[string]any)["description"].(string); ok {
			out = append(out, d)
		}
	}
	return out
}

type defSizes struct{ desc, schema, params int }

func measure(t *testing.T, tools map[string]*sdk.Tool) defSizes {
	t.Helper()
	var s defSizes
	for _, tl := range tools {
		s.desc += len([]rune(tl.Description))
		s.schema += pyJSONLen(t, tl.InputSchema)
		for _, d := range paramDescs(tl) {
			s.params += len([]rune(d))
		}
	}
	return s
}

// Every host that loads all tools up front pays these definitions in every session.
func TestToolDefinitionBudgets(t *testing.T) {
	one := measure(t, listTools(t, newTestApp(t, "main")))
	if one.desc > 7200 || one.desc+one.schema > 18000 {
		t.Errorf("one repo: descriptions %d (max 7200), definitions %d (max 18000)", one.desc, one.desc+one.schema)
	}
	// Several repos plus a workspace offers every parameter and sentence.
	a, _, _ := servedApp(t, "acme")
	all := measure(t, listTools(t, a))
	if all.desc > 7200 || all.params > 6300 {
		t.Errorf("several repos + workspace: descriptions %d (max 7200), parameter descriptions %d (max 6300)", all.desc, all.params)
	}
}

var reposTools = []string{"record_memory", "record_bug", "search_graph", "trace_call_path", "get_architecture",
	"query_graph", "search_content", "get_code_snippet", "get_file_outline", "find_implementations",
	"find_annotated", "find_references", "get_graph_delta"}

func toolsWithRepo(tools map[string]*sdk.Tool) map[string]bool {
	out := map[string]bool{}
	for n, tl := range tools {
		if properties(tl)["repo"] != nil {
			out[n] = true
		}
	}
	return out
}

// recordVia records a memory through the MCP client and returns its id.
func recordVia(t *testing.T, a *app.App, args map[string]any) string {
	t.Helper()
	res, err := connect(t, a).CallTool(context.Background(), &sdk.CallToolParams{Name: "record_memory", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("record_memory %v: %+v", args, res.Content)
	}
	id, _ := res.StructuredContent.(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("record_memory %v returned no id: %+v", args, res.StructuredContent)
	}
	return id
}

func TestSetupDependentParameters(t *testing.T) {
	ctx := context.Background()
	t.Run("one repo", func(t *testing.T) {
		tools := listTools(t, newTestApp(t, "main"))
		if got := toolsWithRepo(tools); len(got) > 0 {
			t.Errorf("tools with repo: %v, want none", got)
		}
		if properties(tools["record_memory"])["scope"] != nil {
			t.Error("record_memory has scope without a workspace")
		}
		if properties(tools["get_architecture"])["scope"] == nil {
			t.Error("get_architecture lost its scope path prefix")
		}
		for _, n := range []string{"record_memory", "recall_memory"} {
			if strings.Contains(tools[n].Description, "workspace") {
				t.Errorf("%s mentions the workspace without one: %q", n, tools[n].Description)
			}
		}
	})
	t.Run("several repos", func(t *testing.T) {
		a, _, _ := newServedApp(t)
		tools := listTools(t, a)
		got := toolsWithRepo(tools)
		for _, n := range reposTools {
			if !got[n] {
				t.Errorf("%s lacks repo with several repos served", n)
			}
			delete(got, n)
		}
		if len(got) > 0 {
			t.Errorf("unexpected tools with repo: %v", got)
		}
		if properties(tools["record_memory"])["scope"] != nil {
			t.Error("record_memory has scope without a workspace")
		}
		id := recordVia(t, a, map[string]any{"kind": "decision", "body": "alpha stays synchronous", "repo": "repo1"})
		if r, _ := a.Repos[0].Engine.GetByID(ctx, id); r == nil {
			t.Error("repo=repo1 memory missing from repo1's store")
		}
		if r, _ := a.Engine.GetByID(ctx, id); r != nil {
			t.Error("repo=repo1 memory landed in the primary store")
		}
	})
	t.Run("workspace", func(t *testing.T) {
		a := newWorkspaceApp(t, "main")
		tools := listTools(t, a)
		if got := toolsWithRepo(tools); len(got) > 0 {
			t.Errorf("tools with repo: %v, want none", got)
		}
		scope, _ := properties(tools["record_memory"])["scope"].(map[string]any)
		if enum, _ := json.Marshal(scope["enum"]); string(enum) != `["repo","workspace"]` {
			t.Errorf("record_memory scope enum = %s, want [repo workspace]", enum)
		}
		for _, n := range []string{"record_memory", "recall_memory"} {
			if !strings.Contains(tools[n].Description, "workspace store") {
				t.Errorf("%s does not mention the workspace store: %q", n, tools[n].Description)
			}
		}
		id := recordVia(t, a, map[string]any{"kind": "preference", "body": "tabs over spaces everywhere", "scope": "workspace"})
		if r, _ := a.Workspace.Engine.GetByID(ctx, id); r == nil {
			t.Error("scope=workspace memory missing from the workspace store")
		}
		if r, _ := a.Engine.GetByID(ctx, id); r != nil {
			t.Error("scope=workspace memory landed in the repo store")
		}
	})
}

// Shortening the definitions must not drop what agents need to use the tools well.
func TestToolDefinitionsKeepFacts(t *testing.T) {
	facts := map[string][]string{
		"search_graph":         {"searching by concept", "candidate_symbols"},
		"get_code_snippet":     {"bare name", "qualified name", "Type.Method", "optional", "candidates", "symbols"},
		"trace_call_path":      {"calls direction in both modes", "candidates to verify", "Empty chains", "root-only", "find_implementations", "find_references"},
		"search_content":       {"whole worktree", ".gitignore", "doesn't index", "literal", "regex=true", "total", "truncated", "per_file_capped", "offset", "max_per_file"},
		"find_references":      {"text sweep", "resolved=false", "text-only", "Type.Method"},
		"query_graph":          {"At least one filter is required"},
		"find_implementations": {"search_graph can't", "don't share"},
		"find_annotated":       {"search_graph can't", "grep"},
		"get_graph_delta":      {"last session here that changed it", "renamed file", "old path"},
		"record_memory":        {"conflict_candidates", "preference", "learning", "decision", "do_not_repeat", "bug", "todo"},
		"report_recarto_issue": {"Recarto itself", "proprietary source"},
	}
	tools := listTools(t, newTestApp(t, "main"))
	for name, phrases := range facts {
		tl := tools[name]
		if tl == nil {
			t.Errorf("%s not offered", name)
			continue
		}
		text := tl.Description + "\n" + strings.Join(paramDescs(tl), "\n")
		for _, p := range phrases {
			if !strings.Contains(text, p) {
				t.Errorf("%s no longer states %q", name, p)
			}
		}
	}
}
