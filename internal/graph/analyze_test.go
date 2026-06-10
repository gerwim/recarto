package graph

import (
	"context"
	"testing"
)

// TestFullResolveSeedsPlannerStats proves a full resolve leaves planner
// statistics (sqlite_stat1) behind. Without them SQLite costs every join from
// guesses; on a large store that inverted the trace neighbor plan (21s hops) and
// leaves every other ad-hoc query one bad estimate away from a table scan.
// ANALYZE after the (bulk) full resolve is cheap — ~0.3s on a 627MB store —
// relative to the resolve itself.
func TestFullResolveSeedsPlannerStats(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){ B() }\nfunc B(){}")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	var stats int
	if err := e.st.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_stat1 WHERE tbl IN ('edge_candidates','edges','nodes','files')`,
	).Scan(&stats); err != nil {
		t.Fatalf("sqlite_stat1 missing after full resolve: %v", err)
	}
	if stats == 0 {
		t.Fatal("full resolve must ANALYZE the graph tables (sqlite_stat1 has no rows for them)")
	}
}
