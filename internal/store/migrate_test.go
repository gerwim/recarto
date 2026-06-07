package store

import (
	"database/sql"
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
	// Several later migrations wipe the derived graph (v11, and most recently v15's
	// signature backfill). Seed an old user_version so the reopen re-runs that
	// graph-wipe chain and clears the rows seeded above.
	mustExec(t, st, `PRAGMA user_version = 10`)
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

func TestMigrateV9AddsResolutionSchema(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// New tables exist.
	for _, table := range []string{"type_bindings", "edge_candidates"} {
		var name string
		if err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}
	// edges has the new dst_recv column.
	rows, err := st.DB.Query(`PRAGMA table_info(edges)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hasRecv := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "dst_recv" {
			hasRecv = true
		}
	}
	if !hasRecv {
		t.Error("edges.dst_recv column missing")
	}
}

// TestV9MigrationPreservesMemories pins the standing invariant that a
// graph-invalidating migration NEVER deletes memories/bugs (bugs are memories
// rows with kind='bug') — only the rebuildable code-graph index is wiped.
func TestV9MigrationPreservesMemories(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('m1','learning','keep me','body',1,1)`)
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('b1','bug','a bug','body',1,1)`)
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','A.cs','csharp','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO meta(key,value) VALUES('graph_token:/wt','tok')`)
	// v11 is the last graph-rebuilding migration; v12 (todo CHECK widening) is not.
	// Seed just before v11 so the reopen runs a graph wipe and we can assert it
	// preserved memories while clearing the rebuildable graph.
	mustExec(t, st, `PRAGMA user_version = 10`)
	st.Close()

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var mem int
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&mem); err != nil {
		t.Fatal(err)
	}
	if mem != 2 {
		t.Errorf("memories after migration = %d, want 2 (migrations must never delete memories/bugs)", mem)
	}
	var files int
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Errorf("graph not rebuilt: files = %d, want 0", files)
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

func TestMigrateV12AllowsTodoAndDone(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// New kind accepted.
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('t1','todo','do the thing','body',1,1)`)
	// New status accepted.
	mustExec(t, st, `UPDATE memories SET status='done' WHERE id='t1'`)

	// Old CHECKs still enforced: a bogus kind/status is rejected.
	if _, err := st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid kind")
	}
	if _, err := st.DB.Exec(`UPDATE memories SET status='bogus' WHERE id='t1'`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid status")
	}
}

func TestV12MigrationPreservesMemoriesAndColumns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('m1','learning','keep me','body',1,1)`)
	// A column added by an ALTER migration must survive the writable_schema edit.
	mustExec(t, st, `UPDATE memories SET last_used_epoch=7 WHERE id='m1'`)
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1))
	st.Close()

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var epoch int
	if err := st2.DB.QueryRow(`SELECT last_used_epoch FROM memories WHERE id='m1'`).Scan(&epoch); err != nil {
		t.Fatalf("row/column lost after migration: %v", err)
	}
	if epoch != 7 {
		t.Errorf("last_used_epoch = %d, want 7", epoch)
	}
	mustExec(t, st2, `INSERT INTO memories(id,kind,status,created_at,updated_at)
		VALUES('t2','todo','done',1,1)`)
}

func TestSchemaVersionIs17(t *testing.T) {
	if schemaVersion != 17 {
		t.Fatalf("schemaVersion = %d, want 17", schemaVersion)
	}
	if len(migrations) != 17 {
		t.Fatalf("len(migrations) = %d, want 17", len(migrations))
	}
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 17 {
		t.Fatalf("user_version = %d, want 17", v)
	}
}

// TestMigrateV17AllowsFeedbackAndResolved mirrors TestMigrateV12AllowsTodoAndDone:
// the widened CHECKs admit the new kind/status while still rejecting bogus values.
func TestMigrateV17AllowsFeedbackAndResolved(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('f1','feedback','search_content rejected query','body',1,1)`)
	mustExec(t, st, `UPDATE memories SET status='resolved' WHERE id='f1'`)

	if _, err := st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid kind")
	}
	if _, err := st.DB.Exec(`UPDATE memories SET status='bogus' WHERE id='f1'`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid status")
	}
}
