package graph

import (
	"context"
	"testing"
)

// orphanFTSRows counts FTS rows whose node no longer exists — must be zero
// after a prune, since FTS5 tables are not FK-linked and need explicit teardown.
func orphanFTSRows(t *testing.T, e *Engine) int {
	t.Helper()
	total := 0
	for _, tbl := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var n int
		if err := e.st.DB.QueryRow(
			`SELECT COUNT(*) FROM ` + tbl + ` WHERE node_id NOT IN (SELECT id FROM nodes)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

func metaKeyExists(t *testing.T, e *Engine, key string) bool {
	t.Helper()
	var v string
	err := e.st.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return err == nil
}

// TestPruneWorktreeRemovesAllRows: pruning a worktree must remove its files
// (cascading nodes/edges/candidates), its FTS rows, and its meta keys — and
// leave every other worktree fully intact and queryable.
func TestPruneWorktreeRemovesAllRows(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.ReconcileIfStale(ctx, "/wtA", crossFileCallSource(), "tok-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileIfStale(ctx, "/wtB", crossFileCallSource(), "tok-b"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wtB"); got != 2 {
		t.Fatalf("setup: wtB files = %d, want 2", got)
	}

	if err := e.PruneWorktree(ctx, "/wtB"); err != nil {
		t.Fatal(err)
	}

	for _, tbl := range []string{"files", "nodes", "edges"} {
		if got := countRows(t, e, tbl, "/wtB"); got != 0 {
			t.Errorf("%s rows for pruned worktree = %d, want 0", tbl, got)
		}
	}
	if got := orphanFTSRows(t, e); got != 0 {
		t.Errorf("orphaned FTS rows after prune = %d, want 0", got)
	}
	if metaKeyExists(t, e, "graph_token:/wtB") {
		t.Error("graph_token meta key survived the prune")
	}

	// The surviving worktree is untouched and still resolved.
	if got := countRows(t, e, "files", "/wtA"); got != 2 {
		t.Errorf("wtA files = %d, want 2 (prune must be scoped)", got)
	}
	if got := resolvedCallEdges(t, e, "/wtA"); got == 0 {
		t.Error("wtA lost its resolved edges")
	}
	if metaKeyExists(t, e, "graph_token:/wtA") == false {
		t.Error("wtA freshness token lost")
	}
}

// TestWorktreesListsDistinct: Engine.Worktrees feeds the GC's liveness check.
func TestWorktreesListsDistinct(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.Reconcile(ctx, "/wtA", crossFileCallSource()); err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx, "/wtB", crossFileCallSource()); err != nil {
		t.Fatal(err)
	}
	wts, err := e.Worktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 {
		t.Fatalf("Worktrees = %v, want 2 entries", wts)
	}
}
