package store

import (
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
