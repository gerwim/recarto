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

func TestSearchGraphReturnsSignature(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Greet(name string) string { return name }"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "Greet"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range out.Results {
		if r.Name == "Greet" && strings.Contains(r.Signature, "func Greet(") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no result carried the Greet signature: %+v", out.Results)
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
		if len(ch) >= 2 && ch[0].Symbol == "A" && ch[1].Symbol == "B" {
			foundChain = true
			// B is a unique Go function → name resolution, medium confidence.
			if ch[1].Resolution != "name" || ch[1].Confidence != "medium" {
				t.Errorf("B hop = %+v, want resolution=name confidence=medium", ch[1])
			}
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

func TestSearchGraphCandidateSymbolsAndHint(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.cs"),
		[]byte("namespace App;\nclass PdokAddressLookupService {}\nclass InvoiceService {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// candidate_symbols drives a concept query to the right symbol; no hint when found.
	out, err := searchGraph(ctx, a, SearchGraphInput{
		Query:            "convert a street into coordinates",
		CandidateSymbols: []string{"AddressLookup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range out.Results {
		if r.Name == "PdokAddressLookupService" {
			found = true
		}
	}
	if !found {
		t.Errorf("candidate_symbols search missed PdokAddressLookupService: %+v", out.Results)
	}
	if out.Hint != "" {
		t.Errorf("hint should be empty when results were found, got %q", out.Hint)
	}

	// Empty result + no candidates → hint nudges the agent to supply candidate_symbols.
	miss, err := searchGraph(ctx, a, SearchGraphInput{Query: "zzzznotarealconcept"})
	if err != nil {
		t.Fatal(err)
	}
	if miss.Count != 0 || miss.Hint == "" {
		t.Errorf("expected zero results with a hint, got count=%d hint=%q", miss.Count, miss.Hint)
	}
}

func TestTraceCallPathSuggestions(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Greet(name string) string { return name }"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "Gree"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Chains) != 0 {
		t.Errorf("expected no chains, got %v", out.Chains)
	}
	if len(out.Suggestions) == 0 {
		t.Fatal("expected suggestions for a near-miss symbol")
	}
}
