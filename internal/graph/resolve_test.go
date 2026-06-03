package graph

import (
	"context"
	"testing"
)

func TestResolveLinksCallEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Two files in one worktree: a.go's A() calls B() defined in b.go.
	if err := e.IndexFile(ctx, "/wt", "a.go", []byte("package p\nfunc A(){ B() }"), 1, 24); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "b.go", []byte("package p\nfunc B(){}"), 1, 19); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var resolved int
	if err := e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges WHERE kind='calls' AND dst_name='B' AND dst_node_id IS NOT NULL`).
		Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Errorf("resolved call edges to B = %d, want 1", resolved)
	}
}
