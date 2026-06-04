package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestMigrateRetriesPastTransientBusy is the regression test for
// "migrate: database is locked (SQLITE_BUSY)": Migrate runs on every Open (the
// MCP server startup, `recarto doctor`, every CLI/hook invocation), but unlike
// WriteTx it had no BUSY retry — so while a long reconcile holds the write lock
// (even in short batches), a concurrent Open exceeded busy_timeout and hard-failed.
// Migrate must retry past a transiently held lock and succeed.
func TestMigrateRetriesPastTransientBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	// Tiny busy_timeout so a contended BEGIN IMMEDIATE returns SQLITE_BUSY almost
	// immediately, isolating the application-level retry layer. Open also migrates,
	// so the store is already at the latest version (Migrate below is a no-op apart
	// from its BEGIN IMMEDIATE, which is exactly the contended statement under test).
	st, err := open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Hold the single WAL writer lock from an independent connection.
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
		t.Fatalf("Migrate should retry past transient BUSY and succeed, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < hold-20*time.Millisecond {
		t.Fatalf("Migrate returned after %v; expected it to wait out the ~%v held lock", elapsed, hold)
	}
}
