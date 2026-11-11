package graph

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite/vec" // registers the vec0 virtual table on the modernc driver

	"github.com/gerwim/recarto/internal/store"
)

// TestVec0KNNProbe validates that sqlite-vec's vec0 virtual table works through
// recarto's existing modernc.org/sqlite store: a TEXT primary key, a metadata
// column for worktree scoping, KNN via `MATCH ? AND k = ?`, and JSON vector
// literals. This is the substrate the semantic tier is built on, so prove it in
// isolation before wiring the embedder.
func TestVec0KNNProbe(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx, `
CREATE VIRTUAL TABLE node_vectors USING vec0(
  node_id TEXT PRIMARY KEY,
  worktree TEXT,
  embedding float[4]
)`); err != nil {
		t.Fatalf("create vec0 table: %v", err)
	}

	vecs := map[string][]float32{
		"a": {1, 0, 0, 0},
		"b": {0, 1, 0, 0},
		"c": {0.9, 0.1, 0, 0}, // close to a
	}
	for id, v := range vecs {
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO node_vectors(node_id, worktree, embedding) VALUES (?,?,?)`,
			id, "/wt", vecJSONProbe(v)); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	rows, err := st.DB.QueryContext(ctx, `
SELECT node_id, distance FROM node_vectors
 WHERE worktree = ? AND embedding MATCH ? AND k = ?
 ORDER BY distance`, "/wt", vecJSONProbe([]float32{1, 0, 0, 0}), 2)
	if err != nil {
		t.Fatalf("knn query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("KNN for [1,0,0,0] = %v, want [a c] (a exact, c nearest)", got)
	}
}

func vecJSONProbe(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
