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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "step", "s")
	if got["Saga.step"] != "typed" {
		t.Errorf("s.step() should resolve typed to Saga.step; got %v", got)
	}
}
