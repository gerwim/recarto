package graph

import (
	"context"
	"testing"
)

func TestQueryNodeFiltersAndEdge(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Caller(){ Callee() }\nfunc Callee(){}\ntype T struct{}")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}

	types, err := e.Query(ctx, "/wt", QuerySpec{Kind: "type"})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0].Name != "T" {
		t.Errorf("kind=type query = %+v, want [T]", types)
	}

	callers, err := e.Query(ctx, "/wt", QuerySpec{
		NameLike: "Call",
		Edge:     &EdgeFilter{Kind: "calls", Direction: "out"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].Name != "Caller" {
		t.Errorf("edge=out calls query = %+v, want [Caller]", callers)
	}
}

// TestQueryRejectsEmptySpec proves a fully-unconstrained query is refused rather
// than dumping the whole graph. On a real repo that dump is a page of whatever
// sorts first by qualified name — typically vendored/generated symbols (yarn
// .cjs `$` functions on graphql-platform) — ~50 lines of pure noise returned
// with a success status, which both wastes tokens and reads as a real answer.
func TestQueryRejectsEmptySpec(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Caller(){ Callee() }\nfunc Callee(){}\ntype T struct{}")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Query(ctx, "/wt", QuerySpec{}); err == nil {
		t.Fatal("empty QuerySpec should error, not list the whole graph")
	}
	// A limit alone is not a constraint — it still selects the whole graph.
	if _, err := e.Query(ctx, "/wt", QuerySpec{Limit: 10}); err == nil {
		t.Fatal("QuerySpec with only a limit should error")
	}
	// Any real filter is accepted.
	if _, err := e.Query(ctx, "/wt", QuerySpec{Kind: "type"}); err != nil {
		t.Fatalf("QuerySpec with a kind filter must be accepted: %v", err)
	}
}
