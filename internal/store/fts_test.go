package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestFTS5IsAvailable is the §15 risk-gate spike: it proves modernc.org/sqlite
// ships FTS5 with a working bm25() ranking function. If this fails, switch the
// lexical backend to the documented Go inverted-index fallback (see plan).
func TestFTS5IsAvailable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fts.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx,
		`CREATE VIRTUAL TABLE t USING fts5(body)`); err != nil {
		t.Fatalf("FTS5 unavailable: %v", err)
	}
	docs := []string{"the quick brown fox", "lazy dog sleeps", "quick quick quick fox"}
	for _, d := range docs {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO t(body) VALUES (?)`, d); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB.QueryContext(ctx,
		`SELECT body, bm25(t) FROM t WHERE t MATCH 'quick' ORDER BY bm25(t) ASC`)
	if err != nil {
		t.Fatalf("bm25/MATCH failed: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows matched 'quick'")
	}
	var top string
	var score float64
	if err := rows.Scan(&top, &score); err != nil {
		t.Fatal(err)
	}
	// The doc with the most/least-diluted 'quick' must rank first.
	if !strings.Contains(top, "quick quick quick") {
		t.Fatalf("bm25 ranking wrong: top=%q score=%v", top, score)
	}
}
