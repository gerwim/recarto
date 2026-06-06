package mcp

import (
	"context"
	"fmt"
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

func TestSearchContentHandler(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Boom(){\n\tpanic(\"unrecoverable failure\")\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "unrecoverable failure"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Fatalf("searchContent count = %d, want 1: %+v", out.Count, out.Matches)
	}
	m := out.Matches[0]
	if m.Path != "a.go" || m.Line != 3 || !strings.Contains(m.Symbol, "Boom") {
		t.Fatalf("match = %+v, want a.go:3 inside Boom", m)
	}
}

func TestGetFileOutlineHandler(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\n\nfunc First(){}\n\nfunc Second(x int) int { return x }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getFileOutline(context.Background(), a, GetFileOutlineInput{File: "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count < 2 {
		t.Fatalf("outline count = %d, want >= 2: %+v", out.Count, out.Symbols)
	}
	var names []string
	for _, s := range out.Symbols {
		names = append(names, s.Name)
	}
	if !contains(names, "First") || !contains(names, "Second") {
		t.Fatalf("outline names = %v, want First and Second", names)
	}
	if out.Hint != "" {
		t.Errorf("non-empty outline should carry no hint, got %q", out.Hint)
	}

	// A path that doesn't match the indexed relative path returns the guidance hint.
	miss, err := getFileOutline(context.Background(), a, GetFileOutlineInput{File: filepath.Join(wt, "a.go")})
	if err != nil {
		t.Fatal(err)
	}
	if miss.Count != 0 || miss.Hint == "" {
		t.Errorf("absolute-path outline = %+v, want empty with a hint", miss)
	}
}

// TestSearchContentHandlerReportsTruncation proves the MCP tool surfaces the true
// total and truncation flags so an LLM client knows the page is partial.
func TestSearchContentHandlerReportsTruncation(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\n// hit\n// hit\n// hit\n// hit\n// hit\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "hit", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 5 || out.Count != 2 || !out.Truncated {
		t.Fatalf("meta = total %d count %d truncated %v, want 5/2/true", out.Total, out.Count, out.Truncated)
	}
}

// TestTraceCallPathTypeHint proves tracing a type with no call edges returns a
// hint steering to impls/grep instead of a bare empty chain.
func TestTraceCallPathTypeHint(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "f.cs"),
		[]byte("namespace N;\npublic interface IFoo { void Bar(); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "IFoo", Direction: "callers"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Hint == "" || !strings.Contains(out.Hint, "find_implementations") {
		t.Fatalf("trace of a type = %+v, want a hint steering to find_implementations", out)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
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

// TestBudgetArchitecture covers the pure budgeting helper that bounds the
// get_architecture payload: it keeps the highest-signal modules (by symbol count)
// and dependency edges (by reference count), reports the pre-cap totals, and flags
// truncation — but preserves name ordering of whatever subset it keeps.
func TestBudgetArchitecture(t *testing.T) {
	var arch graph.Architecture
	for i := 0; i < 150; i++ {
		// Symbols increases with i, so the highest-i modules are the most significant.
		arch.Modules = append(arch.Modules, graph.Module{
			Name: fmt.Sprintf("mod%03d", i), Files: 1, Symbols: i,
		})
	}
	for i := 0; i < 200; i++ {
		arch.Deps = append(arch.Deps, graph.ModuleEdge{
			From: fmt.Sprintf("a%03d", i), To: "x", Count: i,
		})
	}

	mods, deps, totalMods, totalDeps, truncated := budgetArchitecture(arch, 50)
	if totalMods != 150 || totalDeps != 200 {
		t.Fatalf("totals = (%d,%d), want (150,200)", totalMods, totalDeps)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(mods) != 50 || len(deps) != 50 {
		t.Fatalf("kept (%d,%d), want (50,50)", len(mods), len(deps))
	}
	// Kept modules must be the 50 highest-symbol ones (i=100..149) — none smaller.
	for _, m := range mods {
		if m.Symbols < 100 {
			t.Errorf("kept low-signal module %+v; should have dropped it", m)
		}
	}
	// Kept deps must be the 50 highest-count ones (count 150..199).
	for _, d := range deps {
		if d.Count < 150 {
			t.Errorf("kept low-signal dep %+v; should have dropped it", d)
		}
	}
	// The kept subset is presented in name order, not significance order.
	for i := 1; i < len(mods); i++ {
		if mods[i-1].Name > mods[i].Name {
			t.Errorf("modules not name-sorted: %q before %q", mods[i-1].Name, mods[i].Name)
		}
	}

	// Under the cap: everything kept, nothing truncated.
	small := graph.Architecture{Modules: arch.Modules[:5], Deps: arch.Deps[:5]}
	m2, d2, tm, td, trunc2 := budgetArchitecture(small, 50)
	if trunc2 || len(m2) != 5 || len(d2) != 5 || tm != 5 || td != 5 {
		t.Errorf("small arch: got trunc=%v mods=%d deps=%d totals=(%d,%d)", trunc2, len(m2), len(d2), tm, td)
	}
}

// TestGetArchitectureReportsTotals verifies the handler surfaces pre-cap totals so
// the agent knows the full size even when the payload is budgeted.
func TestGetArchitectureReportsTotals(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc A(){}\ntype T struct{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getArchitecture(ctx, a, GetArchitectureInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.ModuleCount != len(out.Modules) {
		t.Errorf("ModuleCount=%d but returned %d modules (should match when under cap)", out.ModuleCount, len(out.Modules))
	}
	if out.Truncated {
		t.Errorf("tiny repo should not be truncated")
	}
}

func TestRoundScore(t *testing.T) {
	cases := map[float64]float64{
		0.05245901639344262:  0.0525,
		0.019047619047619046: 0.019,
		2.0:                  2.0,
		0:                    0,
	}
	for in, want := range cases {
		if got := roundScore(in); got != want {
			t.Errorf("roundScore(%v) = %v, want %v", in, got, want)
		}
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
