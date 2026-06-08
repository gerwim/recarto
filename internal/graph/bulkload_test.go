package graph

import (
	"context"
	"testing"
)

// secondaryIndexesOnGraphTables returns the names of the user-created (idx_*)
// indexes currently on the bulk-written graph tables.
func secondaryIndexesOnGraphTables(t *testing.T, e *Engine) map[string]bool {
	t.Helper()
	rows, err := e.st.DB.Query(`SELECT name FROM sqlite_master
		WHERE type='index' AND name LIKE 'idx_%'
		  AND tbl_name IN ('nodes','edges','type_bindings')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got[n] = true
	}
	return got
}

// TestGraphSecondaryIndexListMatchesSchema is the drift guard: the hardcoded
// rebuild set must equal exactly the secondary indexes the migrations create on
// the graph tables. If a migration adds or drops one of these indexes without
// updating graphSecondaryIndexes, the bulk-load path would skip or resurrect it —
// this fails the moment that happens.
func TestGraphSecondaryIndexListMatchesSchema(t *testing.T) {
	e := newTestEngine(t)
	schema := secondaryIndexesOnGraphTables(t, e)
	want := map[string]bool{}
	for _, ix := range graphSecondaryIndexes {
		want[ix.name] = true
	}
	for n := range schema {
		if !want[n] {
			t.Errorf("schema has index %q not in graphSecondaryIndexes (bulk load would not rebuild it)", n)
		}
	}
	for n := range want {
		if !schema[n] {
			t.Errorf("graphSecondaryIndexes lists %q that the schema does not create (bulk load would resurrect it)", n)
		}
	}
}

// TestDropRebuildGraphSecondaryIndexes verifies the drop removes exactly the
// rebuild set, the rebuild restores all of them, and both are idempotent (the
// rebuild is also the crash self-heal path, so it must be safe to re-run).
func TestDropRebuildGraphSecondaryIndexes(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	full := secondaryIndexesOnGraphTables(t, e)
	if len(full) != len(graphSecondaryIndexes) {
		t.Fatalf("precondition: %d indexes on graph tables, want %d", len(full), len(graphSecondaryIndexes))
	}

	if err := e.dropGraphSecondaryIndexes(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if got := secondaryIndexesOnGraphTables(t, e); len(got) != 0 {
		t.Fatalf("after drop, %d secondary indexes remain: %v", len(got), got)
	}
	// Idempotent drop (re-run is a no-op via IF EXISTS).
	if err := e.dropGraphSecondaryIndexes(ctx); err != nil {
		t.Fatalf("drop again: %v", err)
	}

	if err := e.createGraphSecondaryIndexes(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := secondaryIndexesOnGraphTables(t, e); len(got) != len(full) {
		t.Fatalf("after rebuild, %d indexes, want %d", len(got), len(full))
	}
	// Idempotent rebuild (the self-heal path re-runs it on every mutating reconcile).
	if err := e.createGraphSecondaryIndexes(ctx); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if got := secondaryIndexesOnGraphTables(t, e); len(got) != len(full) {
		t.Fatalf("after second rebuild, %d indexes, want %d", len(got), len(full))
	}
}
