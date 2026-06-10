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

// TestCreateGraphSecondaryIndexesSurvivesCancellation: a rebuild interrupted
// between two index builds must keep the indexes already built. On a large
// store each CREATE INDEX takes real time; if all 11 share one transaction, a
// client cancellation rolls back every completed build and the next attempt
// starts from zero — under a per-call timeout the rebuild then NEVER completes
// (the production wedge), while each attempt bloats the WAL with rolled-back
// pages.
func TestCreateGraphSecondaryIndexesSurvivesCancellation(t *testing.T) {
	e := newTestEngine(t)
	if err := e.dropGraphSecondaryIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := graphSecondaryIndexes[0].name
	prev := createIndexHook
	createIndexHook = func(name string) {
		if name == first {
			cancel() // simulate the client timeout landing mid-rebuild
		}
	}
	defer func() { createIndexHook = prev }()

	if err := e.createGraphSecondaryIndexes(ctx); err == nil {
		t.Fatal("want error from rebuild cancelled after the first index, got nil")
	}
	if got := secondaryIndexesOnGraphTables(t, e); !got[first] {
		t.Fatalf("index %q built before the cancellation was rolled back: rebuild must commit per index", first)
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
