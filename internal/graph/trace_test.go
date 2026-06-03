package graph

import (
	"context"
	"testing"
)

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
