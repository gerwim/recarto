package graph

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/store"
)

// TestReconcileSkipsResolveWhenNothingChanged proves the resolver does not re-run
// when a reconcile finds no changed or deleted files. A sentinel row is planted
// in edge_candidates (which Resolve unconditionally clears for the worktree's
// call edges); if it survives a no-op reconcile, Resolve was correctly skipped.
func TestReconcileSkipsResolveWhenNothingChanged(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){ B() }\nfunc B(){}")
	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}

	var edgeID string
	if err := e.st.DB.QueryRow(`SELECT e.id FROM edges e JOIN files f ON f.id=e.file_id
		WHERE f.worktree=? AND e.kind='calls' LIMIT 1`, wt).Scan(&edgeID); err != nil {
		t.Fatalf("no 'calls' edge to anchor the test: %v", err)
	}
	// Anchor the sentinel on the caller's own node (name 'A'); Resolve never makes
	// 'A' a candidate of an A->B call, so there is no primary-key collision.
	var nodeID string
	if err := e.st.DB.QueryRow(`SELECT n.id FROM nodes n JOIN files f ON f.id=n.file_id
		WHERE f.worktree=? AND n.name='A' LIMIT 1`, wt).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO edge_candidates(edge_id,dst_node_id,confidence,method) VALUES (?,?,?,?)`,
			edgeID, nodeID, 0.01, "sentinel")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Nothing changed on disk -> reconcile must not re-resolve.
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := e.st.DB.QueryRow(
		`SELECT COUNT(*) FROM edge_candidates WHERE method='sentinel'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("sentinel candidate count = %d, want 1 (Resolve re-ran on an unchanged worktree)", cnt)
	}
}

// barrierSource releases each Read only once `width` reads are concurrently in
// flight; a sequential reader can never reach the barrier and times out. This
// detects whether Reconcile parses files in parallel.
type barrierSource struct {
	files    []string
	width    int
	mu       sync.Mutex
	n        int
	released chan struct{}
	once     sync.Once
}

func (b *barrierSource) List() ([]FileEntry, error) {
	out := make([]FileEntry, 0, len(b.files))
	for _, f := range b.files {
		out = append(out, FileEntry{Path: f, Mtime: 1, Size: 1})
	}
	return out, nil
}

func (b *barrierSource) Read(string) ([]byte, error) {
	b.mu.Lock()
	b.n++
	if b.n >= b.width {
		b.once.Do(func() { close(b.released) })
	}
	b.mu.Unlock()
	select {
	case <-b.released:
		return []byte("package p\nfunc X(){}"), nil
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("barrier not met: %d concurrent reads, want %d (not parallel)", b.n, b.width)
	}
}

func TestReconcileParsesFilesConcurrently(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs >= 2 CPUs to observe concurrency")
	}
	e := newTestEngine(t)
	ctx := context.Background()
	src := &barrierSource{
		files:    []string{"a.go", "b.go", "c.go", "d.go"},
		width:    2,
		released: make(chan struct{}),
	}
	if err := e.Reconcile(ctx, "/wt", src); err != nil {
		t.Fatalf("reconcile failed (parsing appears sequential): %v", err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 4 {
		t.Errorf("files = %d, want 4", got)
	}
}
