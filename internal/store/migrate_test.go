package store

import (
	"context"
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

func TestFTSRowIDMigrationBackfillsExistingRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','A.go','go','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		VALUES('n1','f1','function','Foo','Foo','',0,1,1,1)`)
	for _, table := range []string{"nodes_fts", "nodes_fts_stem"} {
		mustExec(t, st, `INSERT INTO `+table+`(node_id,name,qualified_name) VALUES('n1','Foo','Foo')`)
	}
	mustExec(t, st, `INSERT INTO nodes_fts_tri(node_id,content) VALUES('n1','Foo')`)
	mustExec(t, st, `DROP TABLE node_fts_rows`)
	mustExec(t, st, `PRAGMA user_version = 24`)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var mapped, indexed int64
		if err := st.DB.QueryRow(`SELECT r.fts_rowid, f.rowid FROM node_fts_rows r
			JOIN `+table+` f ON f.node_id=r.node_id WHERE r.node_id='n1'`).
			Scan(&mapped, &indexed); err != nil {
			t.Fatalf("%s mapping not backfilled: %v", table, err)
		}
		if mapped != indexed {
			t.Errorf("%s rowid = %d, mapping = %d", table, indexed, mapped)
		}
	}
}

// TestConstructorLinkMigration pins v27: opening a store built before it drops
// the ctor candidates a C# member call wrongly earned, leaves a real `new` and a
// Python module call alone, and touches no memory.
func TestConstructorLinkMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','A.cs','csharp','h',1,1,1),('f2','/wt','m.py','python','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		VALUES('n1','f1','class','Add','N.Add','',0,1,1,1),('n2','f2','class','Cls','Cls','',0,1,1,1)`)
	for _, e := range []struct{ id, file, src, dst, name, recv string }{
		{"member", "f1", "n1", "n1", "Add", "items"},
		{"new", "f1", "n1", "n1", "Add", ""},
		{"pymod", "f2", "n2", "n2", "Cls", "mod"},
	} {
		mustExec(t, st, fmt.Sprintf(`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,dst_recv,kind,file_id,line)
			VALUES('%s','%s','%s','%s','%s','calls','%s',1)`, e.id, e.src, e.dst, e.name, e.recv, e.file))
		mustExec(t, st, fmt.Sprintf(`INSERT INTO edge_candidates(edge_id,dst_node_id,confidence,method)
			VALUES('%s','%s',0.45,'ctor')`, e.id, e.dst))
	}
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('m1','learning','keep me','memory body',1,1)`)
	mustExec(t, st, `DROP TABLE node_fts_rows`)
	mustExec(t, st, `PRAGMA user_version = 24`)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tc := range []struct {
		edge     string
		cands    int
		bound    bool
		wantWhat string
	}{
		{"member", 0, false, "a C# member call"},
		{"new", 1, true, "a C# new"},
		{"pymod", 1, true, "a Python module call"},
	} {
		var cands int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM edge_candidates WHERE edge_id=?`, tc.edge).Scan(&cands); err != nil {
			t.Fatal(err)
		}
		if cands != tc.cands {
			t.Errorf("%s keeps %d ctor candidates, want %d", tc.wantWhat, cands, tc.cands)
		}
		var bound bool
		if err := st.DB.QueryRow(`SELECT dst_node_id IS NOT NULL FROM edges WHERE id=?`, tc.edge).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if bound != tc.bound {
			t.Errorf("%s binding kept = %v, want %v", tc.wantWhat, bound, tc.bound)
		}
	}
	var title, body string
	if err := st.DB.QueryRow(`SELECT title, body FROM memories WHERE id='m1'`).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != "keep me" || body != "memory body" {
		t.Errorf("memory = %q/%q, want it untouched", title, body)
	}
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 27 {
		t.Errorf("user_version = %d, want 27", v)
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

// TestLatestCheckMigrationPreservesMemoriesAndColumns verifies a writable_schema
// CHECK-widening migration (the latest, always schemaVersion-1 from a freshly
// migrated store) preserves data rows and ALTER-appended columns.
func TestLatestCheckMigrationPreservesMemoriesAndColumns(t *testing.T) {
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

func TestSchemaVersionIs27(t *testing.T) {
	if schemaVersion != 27 {
		t.Fatalf("schemaVersion = %d, want 27", schemaVersion)
	}
	if len(migrations) != 27 {
		t.Fatalf("len(migrations) = %d, want 27", len(migrations))
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
	if v != 27 {
		t.Fatalf("user_version = %d, want 27", v)
	}
}

// TestNewLanguageMigrationsResetGraphTokensOnly pins the Rust- and
// Python-support migrations (v23, v24): each clears the graph freshness tokens
// (so the next reconcile indexes the .rs / .py files already on disk) but keeps
// the indexed graph, which no existing language's output changed.
func TestNewLanguageMigrationsResetGraphTokensOnly(t *testing.T) {
	for _, from := range []int{22, 23} {
		t.Run(fmt.Sprintf("from v%d", from), func(t *testing.T) { checkGraphTokenReset(t, from) })
	}
}

func checkGraphTokenReset(t *testing.T, from int) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','a.go','go','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		VALUES('n1','f1','function','F','F','',0,1,1,1)`)
	mustExec(t, st, `INSERT INTO meta(key,value) VALUES('graph_token:/wt','tok'),('keep','1')`)
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, from))
	st.Close()

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var tokens, nodes, kept int
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM meta WHERE key LIKE 'graph_token:%'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM meta WHERE key='keep'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || nodes != 1 || kept != 1 {
		t.Errorf("after migrating from v%d: graph tokens=%d (want 0), nodes=%d (want 1), other meta=%d (want 1)", from, tokens, nodes, kept)
	}
}

func TestV22CreatesMemoryVectorsAndVocab(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, table := range []string{"memory_vectors", "memories_fts_vocab"} {
		var count int
		if err := st.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("table %s exists %d times, want once", table, count)
		}
	}
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at) VALUES('vector-test','learning','v22needle','body',1,1)`)
	mustExec(t, st, `INSERT INTO memories_fts(memory_id,search_text) VALUES('vector-test','v22needle common')`)
	var docs int
	if err := st.DB.QueryRow(`SELECT doc FROM memories_fts_vocab WHERE term='v22needle'`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if docs != 1 {
		t.Fatalf("vocabulary doc count = %d, want 1", docs)
	}
	mustExec(t, st, `INSERT INTO memory_vectors(memory_id,model,source_at,vec) VALUES('vector-test','test',1,X'00000000')`)
	mustExec(t, st, `DELETE FROM memories WHERE id='vector-test'`)
	var vectors int
	if err := st.DB.QueryRow(`SELECT count(*) FROM memory_vectors WHERE memory_id='vector-test'`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 0 {
		t.Fatalf("cascaded vectors = %d, want 0", vectors)
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

func TestV19CreatesGraphDeltas(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('d1', '/wt', 'a.go', 'Target', 'function', 'added', 1)`); err != nil {
		t.Fatalf("graph_deltas not usable after migration: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('d2', '/wt', 'a.go', 'Target', 'function', 'renamed', 1)`); err == nil {
		t.Fatal("CHECK constraint must reject unknown change values")
	}
}
