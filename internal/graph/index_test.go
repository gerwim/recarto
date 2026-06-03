package graph

import (
	"context"
	"testing"
)

func countRows(t *testing.T, e *Engine, table, worktree string) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table
	var err error
	switch table {
	case "files":
		err = e.st.DB.QueryRow(q+" WHERE worktree=?", worktree).Scan(&n)
	default:
		err = e.st.DB.QueryRow(q+
			" WHERE file_id IN (SELECT id FROM files WHERE worktree=?)", worktree).Scan(&n)
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIndexFileWritesNodesAndEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc A() { B() }\nfunc B() {}")
	if err := e.IndexFile(ctx, "/wt", "p/a.go", src, 111, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 2 { // A, B
		t.Errorf("nodes = %d, want 2", got)
	}
	if got := countRows(t, e, "edges", "/wt"); got < 1 { // A calls B
		t.Errorf("edges = %d, want >=1", got)
	}
	// The call edge must be attributed to its enclosing function A.
	var srcName string
	err := e.st.DB.QueryRow(`
		SELECT n.name FROM edges ed JOIN nodes n ON n.id = ed.src_node_id
		WHERE ed.kind='calls' AND ed.dst_name='B'`).Scan(&srcName)
	if err != nil {
		t.Fatalf("call edge not attributed to enclosing node: %v", err)
	}
	if srcName != "A" {
		t.Errorf("call to B attributed to %q, want A", srcName)
	}
}

func TestIndexFileReplacesOnReindex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}\nfunc B(){}"), 1, 30); err != nil {
		t.Fatal(err)
	}
	// Reindex same path with fewer symbols -> old rows replaced, not duplicated.
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}"), 2, 20); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1 (upsert, not insert)", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 1 {
		t.Errorf("nodes = %d, want 1 after reindex", got)
	}
}

func TestIndexFileSkipsUnsupported(t *testing.T) {
	e := newTestEngine(t)
	if err := e.IndexFile(context.Background(), "/wt", "README.md", []byte("# hi"), 1, 4); err != nil {
		t.Fatalf("unsupported file should be skipped, got %v", err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 0 {
		t.Errorf("files = %d, want 0 (unsupported extension skipped)", got)
	}
}

func TestIndexFileFileScopeEdgeHasNullSrc(t *testing.T) {
	e := newTestEngine(t)
	// The import is at file scope (outside any symbol), so its edge has no
	// enclosing node -> src_node_id must be NULL.
	src := []byte("package p\nimport \"fmt\"\nfunc A() { fmt.Println() }")
	if err := e.IndexFile(context.Background(), "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.st.DB.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='imports' AND src_node_id IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("file-scope import edges with NULL src = %d, want 1", n)
	}
}
