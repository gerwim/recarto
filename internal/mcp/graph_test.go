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
	// name is dropped as the redundant leaf; the canonical identifier is qualified_name.
	if out.Count != 1 || out.Results[0].QualifiedName != "Authenticate" {
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
		if r.QualifiedName == "Greet" && strings.Contains(r.Signature, "func Greet(") {
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
		names = append(names, s.QualifiedName)
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

// TestTraceCallPathMethodCallersDeadEnd proves that a resolved method whose only
// caller is itself (self-recursion → the caller node is already on the frontier)
// returns a root-only chain with a reference-search hint — not an empty result
// that reads as "symbol not found" and pushes the agent to grep.
func TestTraceCallPathMethodCallersDeadEnd(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "f.cs"),
		[]byte("namespace N;\nclass Svc { public void Loop() { Loop(); } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "Loop", Direction: "callers"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Chains) == 0 {
		t.Fatalf("resolved method must return a root-only chain, got empty (reads as 'not found'): %+v", out)
	}
	if len(out.Suggestions) != 0 {
		t.Errorf("resolved method must not emit did-you-mean suggestions, got %v", out.Suggestions)
	}
	if out.Hint == "" || !strings.Contains(out.Hint, "search_content") {
		t.Fatalf("method-callers dead-end = %+v, want a hint steering to search_content", out)
	}
}

// TestTraceCallPathCallersReadInCallsDirection proves the callers chain is
// returned so it reads in the "calls" direction (outer caller first, queried
// symbol last) and carries a legend, so an LLM consuming the structured chains
// does not invert the call direction the way the raw target-first order invites.
func TestTraceCallPathCallersReadInCallsDirection(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "App.cs"),
		[]byte("namespace N;\nclass App {\n  void Outer() { Middle(); }\n  void Middle() { Target(); }\n  void Target() {}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "N.App.Target", Direction: "callers"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Legend == "" || !strings.Contains(out.Legend, "calls") {
		t.Errorf("callers output needs a legend describing the calls direction, got %q", out.Legend)
	}
	// Find the multi-hop chain and assert the queried symbol is the LAST hop.
	var multi []TraceHop
	for _, ch := range out.Chains {
		if len(ch) > len(multi) {
			multi = ch
		}
	}
	if len(multi) < 2 {
		t.Fatalf("expected a multi-hop callers chain, got %+v", out.Chains)
	}
	if multi[len(multi)-1].Symbol != "Target" {
		t.Errorf("queried symbol must be the last hop (calls direction); chain = %+v", multi)
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
	if q.Count != 1 || q.Nodes[0].QualifiedName != "T" {
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

// TestGetCodeSnippetBySymbol proves the tool resolves a symbol when File is
// omitted (the common case after a search returns a name) and lists candidates
// when the bare name is ambiguous, so the agent never has to know the path first.
func TestGetCodeSnippetBySymbol(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\n\nfunc Uniq() int { return 7 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "b.go"),
		[]byte("package q\n\nfunc Dup() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "c.go"),
		[]byte("package r\n\nfunc Dup() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	// Unique name, no File: resolves to the body.
	snip, err := getCodeSnippet(ctx, a, GetCodeSnippetInput{Symbol: "Uniq"})
	if err != nil {
		t.Fatal(err)
	}
	if !snip.Found || !strings.Contains(snip.Snippet, "func Uniq() int") {
		t.Errorf("by-symbol snippet = %+v, want the Uniq body", snip)
	}
	// Ambiguous name, no File: not found, but candidates returned for disambiguation.
	amb, err := getCodeSnippet(ctx, a, GetCodeSnippetInput{Symbol: "Dup"})
	if err != nil {
		t.Fatal(err)
	}
	if amb.Found {
		t.Errorf("ambiguous Dup should not resolve to a single snippet: %+v", amb)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("ambiguous Dup candidates = %d, want 2: %+v", len(amb.Candidates), amb.Candidates)
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
		if r.QualifiedName == "App.PdokAddressLookupService" {
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

// TestCanonicalIdentifier covers the pure projection helper that removes the
// redundant short name from list results: name is always the trailing segment of a
// present qualified_name (verified across go/csharp/ts/tsx/js), so emitting both
// duplicates the leaf on every row. The helper drops name only when it is derivable
// from a non-empty qualified_name, and never leaves a row without any identifier.
func TestCanonicalIdentifier(t *testing.T) {
	cases := []struct {
		name, qualified    string
		wantName, wantQual string
	}{
		// Dotted qualified name: short name is the leaf → dropped.
		{"ExecuteAsync", "HotChocolate.Execution.DefaultRequestExecutor.ExecuteAsync", "", "HotChocolate.Execution.DefaultRequestExecutor.ExecuteAsync"},
		// name == qualified_name (top-level Go func): still redundant → name dropped, qualified kept.
		{"Authenticate", "Authenticate", "", "Authenticate"},
		// name == qualified_name but multi-segment (C# namespace node): exact-equal is still
		// redundant even though the dotted leaf ("Execution") differs from the name.
		{"HotChocolate.Execution", "HotChocolate.Execution", "", "HotChocolate.Execution"},
		// Empty qualified_name: name is the only identifier → kept.
		{"Loner", "", "Loner", ""},
		// name is not the leaf of qualified_name: keep both (defensive; never observed).
		{"Foo", "Bar.Baz", "Foo", "Bar.Baz"},
	}
	for _, c := range cases {
		gotName, gotQual := canonicalIdentifier(c.name, c.qualified)
		if gotName != c.wantName || gotQual != c.wantQual {
			t.Errorf("canonicalIdentifier(%q,%q) = (%q,%q), want (%q,%q)",
				c.name, c.qualified, gotName, gotQual, c.wantName, c.wantQual)
		}
	}
}

// TestSearchGraphOmitsRedundantName proves the handler drops the short name when it
// is the leaf of the qualified_name, keeping the precise (overload-safe) qualified
// identifier the consuming tools accept — so every result row carries one identifier,
// not two copies of the same leaf.
func TestSearchGraphOmitsRedundantName(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.cs"),
		[]byte("namespace App;\nclass PdokAddressLookupService {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "PdokAddressLookupService"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range out.Results {
		if r.QualifiedName == "App.PdokAddressLookupService" {
			found = true
			if r.Name != "" {
				t.Errorf("redundant name should be omitted, got Name=%q", r.Name)
			}
		}
	}
	if !found {
		t.Fatalf("missing App.PdokAddressLookupService in results: %+v", out.Results)
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

func TestGetCodeSnippetHandlerReturnsPath(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbol: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Path != "a.go" {
		t.Fatalf("got found=%v path=%q, want found=true path=a.go", out.Found, out.Path)
	}
}

func TestTraceCallersFlagTestsAndHintWhenAllTest(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "a_test.go"), []byte("package p\nfunc helperTest() { Target() }\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "Target", Direction: "callers"})
	if err != nil {
		t.Fatal(err)
	}
	var sawTestCaller bool
	for _, hops := range out.Chains {
		if len(hops) < 2 {
			continue
		}
		if hops[0].Symbol == "helperTest" {
			sawTestCaller = true
			if !hops[0].IsTest {
				t.Error("caller hop in a_test.go not flagged is_test")
			}
			if hops[len(hops)-1].IsTest {
				t.Error("Target (a.go) wrongly flagged is_test")
			}
		}
	}
	if !sawTestCaller {
		t.Fatalf("no caller chain from helperTest in %+v", out.Chains)
	}
	if !strings.Contains(out.Hint, "test code") || !strings.Contains(out.Hint, "find_references") {
		t.Errorf("hint = %q, want all-test-callers steer toward find_references", out.Hint)
	}
}

func TestFindReferencesHandler(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "b.go"), []byte("package p\nfunc Caller() { Target() }\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "README.md"), []byte("Target is documented here.\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := findReferences(context.Background(), a, FindReferencesInput{Symbol: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved {
		t.Error("Resolved = false, want true")
	}
	var call, text bool
	for _, r := range out.References {
		if r.Path == "b.go" && r.Kind == "calls" {
			call = true
		}
		if r.Path == "README.md" && r.Kind == "text" {
			text = true
		}
	}
	if !call || !text {
		t.Fatalf("want a calls ref from b.go and a text ref from README.md, got %+v", out.References)
	}
	if out.Count != len(out.References) {
		t.Errorf("Count = %d, want %d", out.Count, len(out.References))
	}
}

func TestGetCodeSnippetBatch(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Alpha() {}\nfunc Beta() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: []string{"Alpha", "Beta", "NoSuch"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Snippets) != 3 {
		t.Fatalf("got %d batch entries, want 3: %+v", len(out.Snippets), out.Snippets)
	}
	if out.Snippets[0].Symbol != "Alpha" || !out.Snippets[0].Found || out.Snippets[0].Path != "a.go" {
		t.Errorf("entry 0 = %+v, want found Alpha in a.go", out.Snippets[0])
	}
	if !strings.Contains(out.Snippets[1].Snippet, "func Beta()") {
		t.Errorf("entry 1 snippet = %q, want Beta's source", out.Snippets[1].Snippet)
	}
	if out.Snippets[2].Found {
		t.Errorf("entry 2 (NoSuch) reported found")
	}
}

func TestGetCodeSnippetBatchCapReportsSkipped(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Alpha() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	syms := make([]string, 12)
	for i := range syms {
		syms[i] = "Alpha"
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: syms})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Snippets) != 10 || len(out.Skipped) != 2 {
		t.Fatalf("got %d snippets / %d skipped, want 10 / 2 (no silent caps)", len(out.Snippets), len(out.Skipped))
	}
}

func TestGetCodeSnippetNoSymbolErrors(t *testing.T) {
	a := newTestApp(t, "main")
	a.Git.Worktree = t.TempDir()
	if _, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{}); err == nil {
		t.Fatal("empty input should error, not return an empty result")
	}
}
