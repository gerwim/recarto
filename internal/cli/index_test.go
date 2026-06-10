package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestIndexForceRebuilds: `recarto index --force` must re-run the full
// resolve even when the graph looks up to date — the explicit heal lever for a
// store damaged outside Recarto's view (e.g. by an aborted run under an older
// binary). Without --force, an unchanged worktree skips resolution.
func TestIndexForceRebuilds(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeIndexFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexFile("a.go", "package p\n\nfunc A() {\n\tB()\n}\n")
	writeIndexFile("b.go", "package p\n\nfunc B() {}\n")

	resolved := func() int {
		t.Helper()
		var n int
		if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM edges ed
			JOIN files f ON f.id = ed.file_id
			WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_node_id IS NOT NULL`, wt).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var out bytes.Buffer
	if err := Index(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if resolved() == 0 {
		t.Fatal("setup: index did not resolve the A->B call")
	}

	// Damage the resolution in a way no freshness check can see.
	if _, err := a.Store.DB.Exec(`UPDATE edges SET dst_node_id = NULL WHERE kind='calls'`); err != nil {
		t.Fatal(err)
	}
	if err := Index(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if resolved() != 0 {
		t.Fatal("plain index re-resolved an unchanged worktree; --force would be meaningless")
	}

	if err := Index(context.Background(), a, &out, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	if resolved() == 0 {
		t.Fatal("index --force did not re-resolve")
	}
}

func TestIndexReportsCounts(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Index(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("indexed")) {
		t.Errorf("output = %q, want an 'indexed' summary", out.String())
	}
}
