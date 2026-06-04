package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestMigrateCreatesTables(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	want := []string{"meta", "memories", "memory_refs", "memory_minhash", "counters", "sessions"}
	for _, tbl := range want {
		var name string
		err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", tbl, err)
		}
	}

	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st1, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st1.Close()
	// Re-open: must not error or re-run migrations.
	st2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer st2.Close()
	var v int
	if err := st2.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version after reopen = %d, want %d", v, schemaVersion)
	}
}

func TestMemoriesKindConstraint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`)
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid kind")
	}
}

func TestMigrateCreatesFTS(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var name string
	err = st.DB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memories_fts'`).Scan(&name)
	if err != nil {
		t.Fatalf("memories_fts not created: %v", err)
	}

	var uv int
	if err := st.DB.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != schemaVersion {
		t.Fatalf("user_version = %d, want %d", uv, schemaVersion)
	}
}

func TestMigrateCreatesGraphTables(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, table := range []string{"files", "nodes", "edges", "nodes_fts"} {
		var name string
		err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}

// TestLatestGraphMigrationRebuildsIndex pins the invariant that every schema bump
// which changes the indexer (parser .scm / FTS content) ships a migration that
// invalidates the derived graph, so a store written by the previous version
// rebuilds on upgrade. Generalized over schemaVersion so future bumps keep it.
func TestLatestGraphMigrationRebuildsIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a derived graph index plus its freshness token, exactly as an older
	// indexer (incomplete parser query / un-split FTS) would have left it on disk.
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','A.cs','csharp','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		VALUES('n1','f1','class','Foo','Foo','',0,1,1,1)`)
	mustExec(t, st, `INSERT INTO nodes_fts(node_id,name,qualified_name) VALUES('n1','Foo','Foo')`)
	mustExec(t, st, `INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
		VALUES('e1','n1',NULL,'Bar','calls','f1',1)`)
	mustExec(t, st, `INSERT INTO meta(key,value) VALUES('graph_token:/wt','tok')`)
	// Pretend this store was written by the immediately-previous binary version.
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1))
	st.Close()

	// Reopening with the current binary must invalidate the stale derived graph so
	// the next reconcile rebuilds it with the fixed parser/FTS; memories are kept.
	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	for _, tbl := range []string{"files", "nodes", "edges", "nodes_fts"} {
		var n int
		if err := st2.DB.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s not cleared by rebuild migration: %d rows", tbl, n)
		}
	}
	var tokens int
	if err := st2.DB.QueryRow(
		`SELECT COUNT(*) FROM meta WHERE key LIKE 'graph_token:%'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Errorf("graph_token meta not cleared: %d remain", tokens)
	}
}

func mustExec(t *testing.T, st *Store, q string) {
	t.Helper()
	if _, err := st.DB.Exec(q); err != nil {
		t.Fatalf("seed exec failed: %v\n%s", err, q)
	}
}

func TestMigrateAddsCompactionColumns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The v4 columns must exist and be selectable.
	rows, err := st.DB.Query(`SELECT last_used_epoch, recall_count, archived_epoch FROM memories LIMIT 0`)
	if err != nil {
		t.Fatalf("compaction columns missing: %v", err)
	}
	rows.Close()
}
