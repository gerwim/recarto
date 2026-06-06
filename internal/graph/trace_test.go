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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
