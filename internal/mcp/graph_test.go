package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

func TestSearchGraphHandler(t *testing.T) {
	a := newTestApp(t, "main") // reuse the existing mcp test helper
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "Authenticate"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Results[0].Name != "Authenticate" {
		t.Fatalf("searchGraph = %+v, want 1 result Authenticate", out)
	}
}

// TestGraphHandlersMapping covers the projection/mapping logic of the other four
// graph tools (trace/architecture/query/snippet), which the engine tests don't.
func TestGraphHandlersMapping(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc A(){ B() }\nfunc B(){}\ntype T struct{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	tr, err := traceCallPath(ctx, a, TraceCallPathInput{Symbol: "A"})
	if err != nil {
		t.Fatal(err)
	}
	foundChain := false
	for _, ch := range tr.Chains {
		if len(ch) >= 2 && ch[0] == "A" && ch[1] == "B" {
			foundChain = true
		}
	}
	if !foundChain {
		t.Errorf("traceCallPath chains = %v, want one starting A,B", tr.Chains)
	}

	arch, err := getArchitecture(ctx, a, GetArchitectureInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(arch.Modules) == 0 {
		t.Errorf("getArchitecture returned no modules")
	}

	q, err := queryGraph(ctx, a, QueryGraphInput{Kind: "type"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 1 || q.Nodes[0].Name != "T" {
		t.Errorf("queryGraph kind=type = %+v, want [T]", q)
	}

	snip, err := getCodeSnippet(ctx, a, GetCodeSnippetInput{File: "a.go", Symbol: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snip.Snippet, "func A") {
		t.Errorf("getCodeSnippet = %q, want the A function source", snip.Snippet)
	}
}
