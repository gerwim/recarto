package store

import (
	"context"
	"path/filepath"
	"testing"
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
