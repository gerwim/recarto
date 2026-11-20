package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestFTSTablesListsFTS5Only: discovery must find every migrated FTS5 table and
// skip the fts5vocab view, which cannot be optimized.
func TestFTSTablesListsFTS5Only(t *testing.T) {
	st := openTestStore(t)
	got, err := st.FTSTables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memories_fts", "nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		if !slices.Contains(got, want) {
			t.Errorf("FTSTables() = %v, missing %s", got, want)
		}
	}
	if slices.Contains(got, "memories_fts_vocab") {
		t.Errorf("FTSTables() = %v, must exclude the fts5vocab table", got)
	}
}

func TestIsFTS5DDL(t *testing.T) {
	cases := map[string]bool{
		`CREATE VIRTUAL TABLE t USING fts5(body)`:                     true,
		"CREATE VIRTUAL TABLE t USING FTS5 (\n body)":                 true,
		`CREATE VIRTUAL TABLE v USING fts5vocab(memories_fts, 'row')`: false,
		`CREATE VIRTUAL TABLE r USING rtree(id, a, b)`:                false,
		`CREATE TABLE plain(x)`:                                       false,
	}
	for ddl, want := range cases {
		if got := isFTS5DDL(ddl); got != want {
			t.Errorf("isFTS5DDL(%q) = %v, want %v", ddl, got, want)
		}
	}
}

// TestOptimizeFTSCompactsFragmentedIndex: many small committed writes plus
// deletes leave an FTS5 index split across segments with tombstones; optimize
// must merge it into fewer %_data rows while keeping search results intact.
func TestOptimizeFTSCompactsFragmentedIndex(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	for i := range 400 {
		if err := st.WriteTx(ctx, func(c Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO memories_fts(memory_id, search_text) VALUES (?, ?)`,
				fmt.Sprintf("m%d", i), fmt.Sprintf("alpha beta token%d gamma%d", i, i%7))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.ExecContext(ctx,
		`DELETE FROM memories_fts WHERE CAST(substr(memory_id, 2) AS INTEGER) % 2 = 0`); err != nil {
		t.Fatal(err)
	}

	dataRows := func() int {
		var n int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories_fts_data`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	matches := func() int {
		var n int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'alpha'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	before, beforeMatches := dataRows(), matches()
	tables, err := st.OptimizeFTS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tables, "memories_fts") {
		t.Errorf("OptimizeFTS() = %v, should report memories_fts", tables)
	}
	after := dataRows()
	if after >= before {
		t.Errorf("memories_fts_data rows %d -> %d; optimize should merge segments", before, after)
	}
	if got := matches(); got != beforeMatches || got != 200 {
		t.Errorf("MATCH count %d -> %d after optimize, want 200", beforeMatches, got)
	}
	// The index must still pass FTS5's own consistency check.
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO memories_fts(memories_fts) VALUES ('integrity-check')`); err != nil {
		t.Errorf("integrity-check after optimize: %v", err)
	}
}
