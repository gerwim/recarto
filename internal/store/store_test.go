package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAppliesPragmas(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var journal string
	if err := st.DB.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := st.DB.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy < 5000 {
		t.Fatalf("busy_timeout = %d, want >= 5000", busy)
	}
}

func TestWriteTxRetriesPastTransientBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	// Tiny busy_timeout so a contended BEGIN IMMEDIATE returns SQLITE_BUSY almost
	// immediately rather than waiting — this isolates the application-level retry
	// layer from the busy_timeout PRAGMA.
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

	// Release the lock after a short hold; WriteTx must keep retrying until then.
	const hold = 120 * time.Millisecond
	go func() {
		time.Sleep(hold)
		_, _ = bconn.ExecContext(ctx, "COMMIT")
		_ = bconn.Close()
	}()

	start := time.Now()
	err = st.WriteTx(ctx, func(c Conn) error {
		_, e := c.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('k', 'v')`)
		return e
	})
	if err != nil {
		t.Fatalf("WriteTx should retry past transient BUSY and succeed, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < hold-20*time.Millisecond {
		t.Fatalf("WriteTx returned after %v; expected it to wait out the ~%v held lock", elapsed, hold)
	}

	var v string
	if err := st.DB.QueryRow(`SELECT value FROM meta WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Fatalf("row not written after retry: v=%q err=%v", v, err)
	}
}

func TestWriteTxCommitsAndRollsBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx, `CREATE TABLE t (n INTEGER)`); err != nil {
		t.Fatal(err)
	}

	// Commit path.
	if err := st.WriteTx(ctx, func(c Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO t(n) VALUES (1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Rollback path: returning an error must roll back.
	wantErr := context.Canceled
	_ = st.WriteTx(ctx, func(c Conn) error {
		_, _ = c.ExecContext(ctx, `INSERT INTO t(n) VALUES (2)`)
		return wantErr
	})

	var count int
	if err := st.DB.QueryRow(`SELECT count(*) FROM t`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (rollback failed)", count)
	}
}
