package graph

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// explainFirstStep returns the detail text of the first step in the query plan —
// the table SQLite drives the join from.
func explainFirstStep(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("empty query plan")
	}
	var id, parent, notused int
	var detail string
	if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

// TestCallNeighborsPlanDrivesFromConstrainedTable pins the join order of the
// trace neighbor queries. Without a forced order the (stat-less) planner is free
// to drive the callers query from `files` — enumerating every call edge in the
// worktree per hop, which turned `graph callers ExecuteAsync` (an aggregated
// 128-definition root) into a 21-second query on graphql-platform. The CROSS
// JOIN form pins the driver to the IN-constrained table, making the plan
// independent of table sizes and ANALYZE state.
func TestCallNeighborsPlanDrivesFromConstrainedTable(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){ B() }\nfunc B(){}")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// The pathological shape is a wide aggregated root: many ids in the IN list.
	const nIDs = 128
	args := make([]any, 0, nIDs+1)
	for range nIDs {
		args = append(args, "x")
	}
	args = append(args, wt)

	// The driver appears as "SEARCH <alias> USING ..." or (tiny table) "SCAN <alias>".
	callers := explainFirstStep(t, e.st.DB, callNeighborsQuery("callers", nIDs), args...)
	if !strings.Contains(callers+" ", " ec ") {
		t.Fatalf("callers neighbor query must drive from edge_candidates (ec), got first step: %s", callers)
	}
	callees := explainFirstStep(t, e.st.DB, callNeighborsQuery("callees", nIDs), args...)
	if !strings.Contains(callees+" ", " ed ") {
		t.Fatalf("callees neighbor query must drive from edges (ed), got first step: %s", callees)
	}
}
