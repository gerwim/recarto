package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateCurrentSchemaSkipsWriteLock is the regression test for the
// read-path contention that produced intermittent "database is locked"
// (SQLITE_BUSY) on busy stores: Migrate runs on every Open — every CLI query,
// every hook, every MCP startup — but it used to BEGIN IMMEDIATE (acquire the
// single WAL writer lock) unconditionally, even when the schema was already
// current (the overwhelmingly common case). So a read-only open fired while any
// writer held the lock (a reconcile/index, or a second recarto process) competed
// for the writer and could hard-fail once the busy_timeout + retry budget was
// exhausted. With an already-current schema Migrate must instead read
// user_version lock-free (WAL permits a reader under a held writer) and return
// without ever needing the lock.
func TestMigrateCurrentSchemaSkipsWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	// Tiny busy_timeout: if Migrate wrongly tried to take the contended write
	// lock it would SQLITE_BUSY almost immediately rather than mask the bug by
	// waiting. Open already migrated the store to the latest version.
	st, err := open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Hold the single WAL writer lock for the duration from an independent
	// connection, simulating an in-flight reconcile/index or another process.
	blocker, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	bconn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("blocker BEGIN IMMEDIATE: %v", err)
	}
	defer func() { _, _ = bconn.ExecContext(ctx, "ROLLBACK") }()

	// Schema is current, so Migrate must succeed via the lock-free fast path and
	// never touch the held write lock.
	if err := Migrate(st.DB); err != nil {
		t.Fatalf("Migrate on a current schema must not contend for the write lock, got: %v", err)
	}
}

// TestMigratePendingRetriesPastTransientBusy keeps coverage of the slow path:
// when a migration IS pending, Migrate must take the write lock — and retry past
// a transiently held one (busy_timeout per attempt + bounded backoff) rather than
// hard-failing with SQLITE_BUSY, so a concurrent reconcile cannot take down an
// upgrade.
func TestMigratePendingRetriesPastTransientBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Roll the recorded version back one so the last migration is pending again.
	// Re-running it is idempotent (see TestLatestCheckMigrationPreservesMemoriesAndColumns),
	// so this exercises the lock + apply path without a bespoke migration.
	if _, err := st.DB.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion-1)); err != nil {
		t.Fatal(err)
	}

	blocker, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	bconn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("blocker BEGIN IMMEDIATE: %v", err)
	}

	// Release the lock after a short hold; Migrate must keep retrying until then.
	const hold = 120 * time.Millisecond
	go func() {
		time.Sleep(hold)
		_, _ = bconn.ExecContext(ctx, "COMMIT")
		_ = bconn.Close()
	}()

	start := time.Now()
	if err := Migrate(st.DB); err != nil {
		t.Fatalf("Migrate should retry past transient BUSY and apply the pending migration, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < hold-20*time.Millisecond {
		t.Fatalf("Migrate returned after %v; expected it to wait out the ~%v held lock", elapsed, hold)
	}
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after migrate, want %d", v, schemaVersion)
	}
}
