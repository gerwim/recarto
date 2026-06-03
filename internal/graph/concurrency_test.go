package graph

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// memSource is an in-memory Source for deterministic concurrency tests.
type memSource struct{ files map[string]string }

func (m memSource) List() ([]FileEntry, error) {
	var out []FileEntry
	for p, c := range m.files {
		out = append(out, FileEntry{Path: p, Mtime: 1, Size: int64(len(c))})
	}
	return out, nil
}
func (m memSource) Read(path string) ([]byte, error) { return []byte(m.files[path]), nil }

func TestConcurrentReconcileAcrossWorktrees(t *testing.T) {
	e := newTestEngine(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt := fmt.Sprintf("/wt%d", i)
			src := memSource{files: map[string]string{
				"a.go": fmt.Sprintf("package p\nfunc A%d(){ B%d() }", i, i),
				"b.go": fmt.Sprintf("package p\nfunc B%d(){}", i),
			}}
			for r := 0; r < 5; r++ {
				if err := e.Reconcile(context.Background(), wt, src); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Integrity + per-worktree isolation: each worktree has exactly its 2 nodes.
	var integrity string
	if err := e.st.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}
	for i := 0; i < n; i++ {
		if got := countRows(t, e, "nodes", fmt.Sprintf("/wt%d", i)); got != 2 {
			t.Errorf("worktree %d nodes = %d, want 2", i, got)
		}
	}
}
