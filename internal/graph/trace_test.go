package graph

import (
	"context"
	"testing"
)

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
