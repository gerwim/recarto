package store

import (
	"path/filepath"
	"testing"
)

// TestEdgesFileIndexExists guards the index backing the hot per-file rewrite
// (`DELETE FROM edges WHERE file_id=?`, run once per changed file during a
// reconcile) and the resolver's file_id joins. Without it those are full scans
// of the edges table, which dominate re-indexing on a large repo.
func TestEdgesFileIndexExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var name string
	err = st.DB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_edges_file'`).
		Scan(&name)
	if err != nil {
		t.Fatalf("idx_edges_file index missing: %v", err)
	}
}
