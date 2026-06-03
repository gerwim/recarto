package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileIndexesChangedAndRemovesDeleted(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){}")
	writeFile(t, wt, "sub/b.go", "package q\nfunc B(){}")
	writeFile(t, wt, "notes.md", "ignore me")

	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", wt); got != 2 { // a.go, sub/b.go; not notes.md
		t.Fatalf("files = %d, want 2", got)
	}

	// Delete b.go, change a.go; reconcile must drop b and reindex a only.
	if err := os.Remove(filepath.Join(wt, "sub/b.go")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wt, "a.go", "package p\nfunc A(){}\nfunc C(){}")
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", wt); got != 1 {
		t.Errorf("files = %d, want 1 after delete", got)
	}
	if got := countRows(t, e, "nodes", wt); got != 2 { // A, C in a.go
		t.Errorf("nodes = %d, want 2", got)
	}
}

// TestReconcileSkipsUnchangedContentOnMtimeChange proves the content-hash
// fast-path: when only mtime changes but content is identical, the file is NOT
// re-indexed (its node ids stay stable, since IndexFile deletes+reinserts).
func TestReconcileSkipsUnchangedContentOnMtimeChange(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){}")
	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	var id1 string
	if err := e.st.DB.QueryRow(`SELECT id FROM nodes WHERE name='A'`).Scan(&id1); err != nil {
		t.Fatal(err)
	}
	// Bump only mtime (content unchanged) -> reconcile must skip re-indexing.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "a.go"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	var id2 string
	if err := e.st.DB.QueryRow(`SELECT id FROM nodes WHERE name='A'`).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("node re-indexed on mtime-only change (id %s -> %s); hash fast-path failed", id1, id2)
	}
}
