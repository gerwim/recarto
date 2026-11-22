package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestGraphWipeRollsBackWithItsMigration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	for _, query := range []string{
		`INSERT INTO files(id, worktree, path, language, content_hash, mtime, size, last_indexed_at)
		 VALUES('f1', '/wt', 'f.go', 'go', 'hash', 1, 1, 1)`,
		`INSERT INTO nodes(id, file_id, kind, name, qualified_name, signature,
		 start_byte, end_byte, start_line, end_line)
		 VALUES('n1', 'f1', 'function', 'Zanzibar', 'Zanzibar', '', 0, 1, 1, 1)`,
		`INSERT INTO edges(id, src_node_id, dst_node_id, kind, file_id, line)
		 VALUES('e1', 'n1', 'n1', 'calls', 'f1', 1)`,
		`INSERT INTO edge_candidates(edge_id, dst_node_id) VALUES('e1', 'n1')`,
		`INSERT INTO type_bindings(id, file_id, name, type_name, scope_start_byte, scope_end_byte)
		 VALUES('tb1', 'f1', 'w', 'Widget', 0, 1)`,
		`INSERT INTO node_fts_rows(fts_rowid, node_id) VALUES(1, 'n1')`,
		`INSERT INTO nodes_fts(rowid, node_id, name, qualified_name)
		 VALUES(1, 'n1', 'Zanzibar', 'Zanzibar')`,
		`INSERT INTO nodes_fts_stem(rowid, node_id, name, qualified_name)
		 VALUES(1, 'n1', 'Zanzibar', 'Zanzibar')`,
		`INSERT INTO nodes_fts_tri(rowid, node_id, content)
		 VALUES(1, 'n1', 'Zanzibar Zanzibar')`,
		`INSERT INTO meta(key, value) VALUES
		 ('graph_token:/wt', 'token'), ('arch_cache:/wt', 'cache'), ('graph_built:/wt2', 'built')`,
	} {
		mustExec(t, st, query)
	}

	tables := []string{
		"files", "nodes", "edges", "edge_candidates", "type_bindings", "node_fts_rows",
		"nodes_fts", "nodes_fts_stem", "nodes_fts_tri",
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		counts[table] = scalar(t, st.DB, "SELECT count(*) FROM "+table)
		if counts[table] == 0 {
			t.Fatalf("test premise failed: %s is empty", table)
		}
	}
	keys := []string{"graph_token:/wt", "arch_cache:/wt", "graph_built:/wt2"}
	keyCount := scalar(t, st.DB,
		"SELECT count(*) FROM meta WHERE key IN (?, ?, ?)", keys[0], keys[1], keys[2])
	if keyCount != len(keys) {
		t.Fatalf("test premise has %d graph keys, want %d", keyCount, len(keys))
	}
	schema := SchemaSnapshot(t, st.DB)

	errBoom := errors.New("force migration rollback")
	list := append(slices.Clone(migrations), migration{run: func(ctx context.Context, c Conn) error {
		if err := wipeCodeGraph(ctx, c); err != nil {
			t.Errorf("wipeCodeGraph: %v", err)
			return err
		}
		return errBoom
	}})
	err = migrateOnce(context.Background(), st.DB, list)
	if !errors.Is(err, errBoom) {
		t.Fatalf("migrateOnce error = %v, want wrapped sentinel", err)
	}
	var version int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
	for _, table := range tables {
		if got := scalar(t, st.DB, "SELECT count(*) FROM "+table); got != counts[table] {
			t.Errorf("%s row count after rollback = %d, want %d", table, got, counts[table])
		}
	}
	if got := scalar(t, st.DB,
		"SELECT count(*) FROM meta WHERE key IN (?, ?, ?)", keys[0], keys[1], keys[2]); got != keyCount {
		t.Errorf("graph metadata count after rollback = %d, want %d", got, keyCount)
	}
	if got := SchemaSnapshot(t, st.DB); !slices.Equal(got, schema) {
		t.Errorf("schema changed after rollback:\n got: %v\nwant: %v", got, schema)
	}
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		if got := scalar(t, st.DB, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s MATCH '"zanzibar"'`, table, table)); got != 1 {
			t.Errorf("%s MATCH after rollback = %d, want 1", table, got)
		}
	}
}

func TestGraphWipeKeepsIndexDefinitions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	for _, query := range []string{
		`DROP TABLE nodes_fts`,
		`CREATE VIRTUAL TABLE nodes_fts USING fts5(node_id UNINDEXED, name, qualified_name, prefix = '2')`,
		`DROP TABLE nodes_fts_stem`,
		`CREATE VIRTUAL TABLE nodes_fts_stem USING fts5(node_id UNINDEXED, name, qualified_name, tokenize = 'porter ascii')`,
		`DROP TABLE nodes_fts_tri`,
		`CREATE VIRTUAL TABLE nodes_fts_tri USING fts5(node_id UNINDEXED, content, tokenize = 'trigram case_sensitive 1')`,
		`INSERT INTO nodes_fts(rowid, node_id, name, qualified_name) VALUES(1, 'n1', 'Zanzibar', 'Zanzibar')`,
		`INSERT INTO nodes_fts_stem(rowid, node_id, name, qualified_name) VALUES(1, 'n1', 'Zanzibar', 'Zanzibar')`,
		`INSERT INTO nodes_fts_tri(rowid, node_id, content) VALUES(1, 'n1', 'Zanzibar Zanzibar')`,
	} {
		mustExec(t, st, query)
	}
	definitions := make(map[string]string, 3)
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var definition string
		if err := st.DB.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		definitions[table] = definition
	}
	if err := MigrateWithGraphWipe(st.DB); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var got string
		if err := st.DB.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != definitions[table] {
			t.Errorf("%s definition = %q, want %q", table, got, definitions[table])
		}
		if count := scalar(t, st.DB, "SELECT count(*) FROM "+table); count != 0 {
			t.Errorf("%s row count = %d, want 0", table, count)
		}
	}
	var version int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion+1 {
		t.Errorf("user_version = %d, want %d", version, schemaVersion+1)
	}
}
