package graph

import (
	"context"
	"fmt"
	"testing"
)

// TestReconcileIndexesManyFilesAcrossBatchBoundary guards the batched-write path:
// indexing more files than one write batch holds must still persist every file
// and symbol (no file dropped at a batch boundary).
func TestReconcileIndexesManyFilesAcrossBatchBoundary(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	const n = indexBatchSize*2 + 5 // span several full batches plus a partial tail
	for i := 0; i < n; i++ {
		writeFile(t, wt, fmt.Sprintf("f%03d.go", i),
			fmt.Sprintf("package p\nfunc F%03d(){}", i))
	}

	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", wt); got != n {
		t.Errorf("files = %d, want %d", got, n)
	}
	if got := countRows(t, e, "nodes", wt); got != n { // one func per file
		t.Errorf("nodes = %d, want %d", got, n)
	}
}
