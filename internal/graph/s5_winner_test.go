package graph

import (
	"context"
	"testing"
)

// ambiguousCallSource: caller() invokes Target(), which three files define —
// an S5 (name-fallback) case with multiple matches. The locality order makes
// b.go's Target the deterministic winner (same top-level dir as the caller's
// file beats z-deep paths; then path order).
func ambiguousCallSource() *mutMemSource {
	return &mutMemSource{files: map[string]string{
		"caller.go": "package p\n\nfunc Caller() {\n\tTarget()\n}\n",
		"b.go":      "package p\n\nfunc Target() {}\n",
		"sub/c.go":  "package q\n\nfunc Target() {}\n",
		"sub/d.go":  "package q\n\nfunc Target() {}\n",
	}}
}

// s5Rows returns (rows, confidence, winner dst path) for the single call edge.
func s5Rows(t *testing.T, e *Engine, wt string) (n int, conf float64, winnerPath string) {
	t.Helper()
	if err := e.st.DB.QueryRow(`
		SELECT COUNT(*), MAX(ec.confidence) FROM edge_candidates ec
		 JOIN edges ed ON ed.id = ec.edge_id
		 JOIN files f ON f.id = ed.file_id
		WHERE f.worktree = ? AND ec.method = 'name'`, wt).Scan(&n, &conf); err != nil {
		t.Fatal(err)
	}
	err := e.st.DB.QueryRow(`
		SELECT mf.path FROM edges ed
		 JOIN files f ON f.id = ed.file_id
		 JOIN nodes m ON m.id = ed.dst_node_id
		 JOIN files mf ON mf.id = m.file_id
		WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_name = 'Target'`, wt).Scan(&winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	return n, conf, winnerPath
}

// TestNameFallbackMaterializesOnlyTheWinner: S5 used to store up to 10
// candidate rows per unresolved call edge — 89% of edge_candidates rows and
// 60%+ of store size at scale. Only the locality-best row is materialized now;
// the denormalized target and its ambiguity-keyed confidence are unchanged.
func TestNameFallbackMaterializesOnlyTheWinner(t *testing.T) {
	e := newTestEngine(t)
	if err := e.Reconcile(context.Background(), "/wt", ambiguousCallSource()); err != nil {
		t.Fatal(err)
	}
	n, conf, winner := s5Rows(t, e, "/wt")
	if n != 1 {
		t.Errorf("method='name' rows = %d, want 1 (winner only)", n)
	}
	// Confidence must still reflect the FULL match count (3 → ambiguous, 0.2),
	// not the single emitted row.
	if conf != 0.2 {
		t.Errorf("winner confidence = %v, want 0.2 (keyed on uncapped match count)", conf)
	}
	if winner != "b.go" {
		t.Errorf("denormalized target in %q, want b.go (locality order)", winner)
	}
}

// TestNameFallbackWinnerOnlyIncremental: the scoped SQL strategy used by
// incremental resolve must agree with the bulk Go pass — same single row, same
// confidence, same winner.
func TestNameFallbackWinnerOnlyIncremental(t *testing.T) {
	e := newTestEngine(t)
	src := ambiguousCallSource()
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	// Dirty only the calling file: the incremental path re-resolves its edges.
	src.files["caller.go"] = "package p\n\n// edited\nfunc Caller() {\n\tTarget()\n}\n"
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-2"); err != nil {
		t.Fatal(err)
	}
	n, conf, winner := s5Rows(t, e, "/wt")
	if n != 1 {
		t.Errorf("method='name' rows after incremental = %d, want 1", n)
	}
	if conf != 0.2 {
		t.Errorf("confidence after incremental = %v, want 0.2", conf)
	}
	if winner != "b.go" {
		t.Errorf("denormalized target = %q, want b.go", winner)
	}
}

// TestNameFallbackUniqueMatchKeepsMediumConfidence: a unique same-name match
// stays at 0.45 — the cap change must not disturb the unique case.
func TestNameFallbackUniqueMatchKeepsMediumConfidence(t *testing.T) {
	e := newTestEngine(t)
	src := &mutMemSource{files: map[string]string{
		"caller.go": "package p\n\nfunc Caller() {\n\tTarget()\n}\n",
		"b.go":      "package p\n\nfunc Target() {}\n",
	}}
	if err := e.Reconcile(context.Background(), "/wt", src); err != nil {
		t.Fatal(err)
	}
	n, conf, winner := s5Rows(t, e, "/wt")
	if n != 1 || conf != 0.45 || winner != "b.go" {
		t.Errorf("unique S5: rows=%d conf=%v winner=%q, want 1/0.45/b.go", n, conf, winner)
	}
}
