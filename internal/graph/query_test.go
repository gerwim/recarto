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
	if err := e.Resolve(ctx, "/wt"); err != nil {
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
