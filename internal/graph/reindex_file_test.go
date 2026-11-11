package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gerwim/recarto/internal/store"
)

// TestReindexFileIncrementalKeepsUnrelatedCandidates proves the single-file
// reindex path (the post-edit hook) re-resolves only the edges the edit can
// affect. A full Resolve clears EVERY call candidate in the worktree before
// recomputing — ~14s per edit on a large repo — so the sentinel planted on an
// edge unrelated to the edited file must survive the edit.
func TestReindexFileIncrementalKeepsUnrelatedCandidates(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){ B() }\nfunc B(){}")
	writeFile(t, wt, "c.go", "package p\nfunc C(){ D() }\nfunc D(){}")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Sentinel: a synthetic candidate on the C→D edge, anchored to node C (never
	// a real candidate of that edge, so no PK collision). Untouched by an edit of
	// a.go under incremental resolve; wiped by a full resolve.
	var edgeID, nodeC string
	if err := e.st.DB.QueryRow(`SELECT e.id FROM edges e JOIN files f ON f.id=e.file_id
		WHERE f.worktree=? AND f.path='c.go' AND e.kind='calls' LIMIT 1`, wt).Scan(&edgeID); err != nil {
		t.Fatalf("no calls edge in c.go: %v", err)
	}
	if err := e.st.DB.QueryRow(`SELECT n.id FROM nodes n JOIN files f ON f.id=n.file_id
		WHERE f.worktree=? AND n.name='C' LIMIT 1`, wt).Scan(&nodeC); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO edge_candidates(edge_id, dst_node_id, confidence, method) VALUES (?,?,0.9,'test')`,
			edgeID, nodeC)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Edit a.go (new symbol, no extends change) and reindex just that file.
	abs := filepath.Join(wt, "a.go")
	if err := os.WriteFile(abs, []byte("package p\nfunc A(){ B() }\nfunc B(){}\nfunc A2(){ B() }"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReindexFileIncremental(ctx, wt, "a.go", content, info.ModTime().UnixMilli(), info.Size()); err != nil {
		t.Fatal(err)
	}

	// The edit must be visible…
	var a2 int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM nodes n JOIN files f ON f.id=n.file_id
		WHERE f.worktree=? AND n.name='A2'`, wt).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	if a2 != 1 {
		t.Fatalf("edited file not reindexed: A2 count = %d", a2)
	}
	// …and the unrelated sentinel must survive (incremental, not full, resolve).
	var sentinel int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM edge_candidates WHERE method='test'`).Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if sentinel != 1 {
		t.Fatal("single-file reindex ran a full resolve (sentinel candidate wiped); must resolve incrementally")
	}
	// The edited file's own edges still resolve: A2→B got a candidate.
	var resolved int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM edges e JOIN files f ON f.id=e.file_id
		WHERE f.worktree=? AND f.path='a.go' AND e.kind='calls' AND e.dst_node_id IS NOT NULL`, wt).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved < 2 {
		t.Fatalf("edited file's calls not re-resolved: %d resolved", resolved)
	}
}

// TestReindexFileIncrementalRescopesOnExtendsChange proves a structural edit
// (gaining/losing an extends edge) re-resolves interface dispatch in OTHER
// files: the call named after the class's method is back in scope.
func TestReindexFileIncrementalRescopesOnExtendsChange(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "i.cs", "interface IFoo { void M(); }\nclass A : IFoo { public void M() {} }")
	writeFile(t, wt, "c.cs", "class Caller { void Go(IFoo f) { f.M(); } }")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Plant the sentinel on c.cs's edge; the structural edit of i.cs must wipe it
	// by re-resolving the edge.
	var edgeID, anchor string
	if err := e.st.DB.QueryRow(`SELECT e.id FROM edges e JOIN files f ON f.id=e.file_id
		WHERE f.worktree=? AND f.path='c.cs' AND e.kind='calls' LIMIT 1`, wt).Scan(&edgeID); err != nil {
		t.Fatalf("no calls edge in c.cs: %v", err)
	}
	if err := e.st.DB.QueryRow(`SELECT n.id FROM nodes n JOIN files f ON f.id=n.file_id
		WHERE f.worktree=? AND n.name='Caller' LIMIT 1`, wt).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO edge_candidates(edge_id, dst_node_id, confidence, method) VALUES (?,?,0.9,'test')`,
			edgeID, anchor)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Remove the extends relationship (A no longer implements IFoo).
	abs := filepath.Join(wt, "i.cs")
	next := []byte("interface IFoo { void M(); }\nclass A { public void M() {} }")
	if err := os.WriteFile(abs, next, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReindexFileIncremental(ctx, wt, "i.cs", next, info.ModTime().UnixMilli(), info.Size()); err != nil {
		t.Fatal(err)
	}

	var sentinel int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM edge_candidates WHERE method='test'`).Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	if sentinel != 0 {
		t.Fatal("extends change must re-resolve the dispatching call (sentinel survived)")
	}
}
