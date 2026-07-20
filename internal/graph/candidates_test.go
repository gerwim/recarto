package graph

import (
	"context"
	"testing"
)

// candidateTargets returns {target qualified_name: resolution method} for the
// call edge whose dst_name + dst_recv match.
func candidateTargets(t *testing.T, e *Engine, dstName, recv string) map[string]string {
	t.Helper()
	rows, err := e.st.DB.Query(`
SELECT n.qualified_name, ec.method
  FROM edges ed
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes n ON n.id = ec.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name=? AND ed.dst_recv=?`, dstName, recv)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var qn, method string
		if err := rows.Scan(&qn, &method); err != nil {
			t.Fatal(err)
		}
		out[qn] = method
	}
	return out
}

func TestResolveS2TypedReceiverCSharp(t *testing.T) {
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
	got := candidateTargets(t, e, "Run", "_dep")
	if got["N.Dep.Run"] != "typed" {
		t.Errorf("Run via _dep should have typed candidate N.Dep.Run; got %v", got)
	}
	if _, bad := got["N.Other.Run"]; bad {
		t.Errorf("Run via _dep should NOT bind Other.Run; got %v", got)
	}
}

func TestResolveS1SelfCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Other { void Helper() {} }\n" +
		"class Svc { void Helper() {} void M() { Helper(); this.Helper(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	// Both bare Helper() and this.Helper() capture with recv="" (this has no
	// expression child in the C# grammar), and S1 binds them to the sibling.
	got := candidateTargets(t, e, "Helper", "")
	if got["N.Svc.Helper"] != "self" {
		t.Errorf("Helper()/this.Helper() inside Svc should bind self to N.Svc.Helper; got %v", got)
	}
	if _, bad := got["N.Other.Helper"]; bad {
		t.Errorf("self-call must not bind Other.Helper; got %v", got)
	}
}

func TestResolveDenormalizesBestCandidate(t *testing.T) {
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
	var qn string
	if err := e.st.DB.QueryRow(`
SELECT n.qualified_name FROM edges ed JOIN nodes n ON n.id = ed.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name='Run' AND ed.dst_recv='_dep'`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != "N.Dep.Run" {
		t.Errorf("denormalized dst_node_id = %q, want N.Dep.Run (typed beats name)", qn)
	}
}

func TestResolveS4InterfaceExpansionCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"interface IHandler { void Handle(); }\n" +
		"class A : IHandler { public void Handle() {} }\n" +
		"class B : IHandler { public void Handle() {} }\n" +
		"class Svc { private readonly IHandler _h; void M() { _h.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "H.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Handle", "_h")
	if got["N.A.Handle"] != "iface" || got["N.B.Handle"] != "iface" {
		t.Errorf("_h.Handle() should expand to A.Handle and B.Handle (iface); got %v", got)
	}
}

func TestResolveS3StaticCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Factory { public static Thing Create() { return null; } }\n" +
		"class Svc { void M() { Factory.Create(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Create", "Factory")
	if got["N.Factory.Create"] != "static" {
		t.Errorf("Factory.Create() should bind static to N.Factory.Create; got %v", got)
	}
}

func TestResolveS2TypedReceiverGo(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	if err := e.IndexFile(ctx, "/wt", "saga.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "step", "s")
	if got["Saga.step"] != "typed" {
		t.Errorf("s.step() should resolve typed to Saga.step; got %v", got)
	}
}
