package graph

import (
	"context"
	"strings"
	"testing"
)

func TestCompactReceiver(t *testing.T) {
	long := strings.Repeat("x", 100)
	cases := []struct{ in, want string }{
		{"", ""},
		{"builder", "builder"},
		{"a.b.c", "a.b.c"},
		{"  spaced  ", "spaced"},
		{"builder.Use(\n   x => y\n)", "builder.Use( x => y )"},
		{long, long[:maxReceiverLen-1] + "…"},
	}
	for _, c := range cases {
		got := compactReceiver(c.in)
		if got != c.want {
			t.Errorf("compactReceiver(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r\t") {
			t.Errorf("compactReceiver(%q) leaked whitespace: %q", c.in, got)
		}
		if len([]rune(got)) > maxReceiverLen {
			t.Errorf("compactReceiver(%q) len %d exceeds cap %d", c.in, len([]rune(got)), maxReceiverLen)
		}
	}
}

func TestTracePartialQualifiedName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// LLMs commonly type Type.Method without the namespace. The full qualified
	// name is N.Svc.M; "Svc.M" must still resolve via a dotted-suffix match
	// instead of silently returning nothing.
	src := []byte("namespace N;\nclass Dep { public void Run() {} }\n" +
		"class Svc { private readonly Dep _dep; void M() { _dep.Run(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, suggestions, err := e.TraceCallPathFiltered(ctx, "/wt", "Svc.M", "callees", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	sawRun := false
	for _, ch := range chains {
		for _, n := range ch {
			if n.Name == "Run" {
				sawRun = true
			}
		}
	}
	if !sawRun {
		t.Errorf("partial-qualified 'Svc.M' callees should find Run; chains=%v suggestions=%v", chains, suggestions)
	}
}

func TestTraceCallersAggregatesAmbiguousBareName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Two unrelated classes each define Handle(); each is called by a sibling in
	// its own file (so Resolve binds each call to the same-file Handle). Asking
	// for callers of the *bare* ambiguous name "Handle" must aggregate across BOTH
	// definitions — not arbitrarily pick one (LIMIT 1) and miss the other.
	a := []byte("namespace X;\nclass A { public void Handle() {} }\n" +
		"class UseA { public void RunA() { var a = new A(); a.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "x/A.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	b := []byte("namespace Y;\nclass B { public void Handle() {} }\n" +
		"class UseB { public void RunB() { var b = new B(); b.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "y/B.cs", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "Handle", "callers", 1)
	if err != nil {
		t.Fatal(err)
	}
	sawA, sawB := false, false
	for _, ch := range chains {
		for _, n := range ch {
			switch n.Name {
			case "RunA":
				sawA = true
			case "RunB":
				sawB = true
			}
		}
	}
	if !sawA || !sawB {
		t.Errorf("callers of bare 'Handle' should aggregate both definitions: sawA=%v sawB=%v chains=%v", sawA, sawB, chains)
	}
}

func TestTraceCallersResolvedDeadEndIsNotAMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A self-recursive method whose ONLY caller is itself. Its single caller edge
	// points back to a node already on the start frontier, so the walk skips it and
	// appends no chain. Historically this collapsed to chains==0 — indistinguishable
	// from "symbol not found", which the CLI rendered as "(no symbol named X)" and
	// the MCP as an empty result with no suggestions. That falsely tells the agent
	// the symbol does not exist and pushes it to grep. A resolved symbol must instead
	// yield a root-only chain so the surface can report an honest "no callers found".
	src := []byte("namespace N;\nclass Svc { public void Loop() { Loop(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, suggestions, err := e.TraceCallPathFiltered(ctx, "/wt", "Loop", "callers", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Resolved ⇒ must not look like a miss: exactly the root, no spurious suggestions.
	if len(chains) == 0 {
		t.Fatalf("resolved symbol 'Loop' must yield a root-only chain, got chains=0 suggestions=%v (looks like 'not found')", suggestions)
	}
	if len(suggestions) != 0 {
		t.Errorf("a resolved symbol must not emit did-you-mean suggestions, got %v", suggestions)
	}
	if len(chains) != 1 || len(chains[0]) != 1 || chains[0][0].Name != "Loop" {
		t.Errorf("want single root-only chain [Loop], got %v", chains)
	}
	// The root must carry its real kind so type dead-ends can still be detected.
	if chains[0][0].Kind == "" {
		t.Errorf("root-only dead-end chain must preserve the node kind, got empty")
	}
}

func TestTraceCallPathCallersRespectsResolution(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Two classes in different top-level dirs each define a method named Handle.
	// Each is called only by a sibling in its own file, so Resolve binds each call
	// edge to the same-file Handle. Tracing callers of X.A.Handle must return only
	// its real caller (UseA.Run), NOT UseB.Run — which calls the *other* Handle.
	// A name-keyed caller query collides on "Handle" and reports both.
	a := []byte("namespace X;\nclass A { public void Handle() {} }\n" +
		"class UseA { public void Run() { var a = new A(); a.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "x/A.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	b := []byte("namespace Y;\nclass B { public void Handle() {} }\n" +
		"class UseB { public void Run() { var b = new B(); b.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "y/B.cs", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "X.A.Handle", "callers", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range chains {
		for _, n := range ch {
			if n.Path == "y/B.cs" {
				t.Errorf("callers of X.A.Handle include %s in %s (name collision pollution); chains=%v",
					n.Name, n.Path, chains)
			}
		}
	}
}

func TestCallNeighborsAnnotatesConfidence(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Dep { public void Run() {} }\n" +
		"class Other { public void Run() {} }\n" +
		"class Svc { private readonly Dep _dep; void M() { _dep.Run(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "N.Svc.M", "callees", 2)
	if err != nil {
		t.Fatal(err)
	}
	var sawRun bool
	for _, ch := range chains {
		for _, n := range ch {
			if n.Name == "Run" {
				sawRun = true
				if n.Path != "Svc.cs" || n.Resolution != "typed" || n.Confidence != "high" {
					t.Errorf("Run hop = %+v, want typed/high in Svc.cs", n)
				}
			}
		}
	}
	if !sawRun {
		t.Errorf("expected a Run callee; chains=%v", chains)
	}
}

func TestTraceCallPathCallees(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A -> B -> C
	src := []byte("package p\nfunc A(){ B() }\nfunc B(){ C() }\nfunc C(){}")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "A", "callees", 3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range chains {
		var seq string
		for _, n := range ch {
			seq += n.Name + ">"
		}
		if seq == "A>B>C>" {
			found = true
		}
	}
	if !found {
		t.Errorf("chains = %v, want one A>B>C", chains)
	}
}

func TestTraceSuggestsOnMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Greet(name string) string { return name }")
	if err := e.IndexFile(ctx, "/wt", "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	chains, suggestions, err := e.TraceCallPathFiltered(ctx, "/wt", "Gree", "callees", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 0 {
		t.Errorf("expected no chains for a missed symbol, got %v", chains)
	}
	found := false
	for _, s := range suggestions {
		if s == "Greet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'Greet' among suggestions, got %v", suggestions)
	}
}

// TestTraceRootCarriesKind proves a trace that resolves a non-callable type
// (here an interface) returns a single root-only chain whose node carries the
// symbol's kind, so the caller can steer "find usages" to impls/grep instead of
// reporting a misleading empty/bare result.
func TestTraceRootCarriesKind(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\npublic interface IFoo { void Bar(); }\n")
	if err := e.IndexFile(ctx, "/wt", "f.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	chains, _, err := e.TraceCallPathFiltered(ctx, "/wt", "IFoo", "callers", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 1 || len(chains[0]) != 1 {
		t.Fatalf("want one root-only chain for a type with no callers, got %v", chains)
	}
	if chains[0][0].Kind != "interface" {
		t.Errorf("root kind = %q, want interface", chains[0][0].Kind)
	}
	if !IsTypeKind(chains[0][0].Kind) {
		t.Errorf("IsTypeKind(%q) = false, want true", chains[0][0].Kind)
	}
	if IsTypeKind("method") {
		t.Error("IsTypeKind(method) = true, want false")
	}
}
