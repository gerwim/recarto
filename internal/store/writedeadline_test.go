package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteDeadlineSkipsABusyWriter(t *testing.T) {
	// With an hour of busy timeout, a write that ignored the deadline would hang
	// the test.
	const hour = 60 * 60 * 1000
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := open(path, hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	blocker, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	bconn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	insert := func(ctx context.Context, key string) error {
		return st.WriteTx(ctx, func(c Conn) error {
			_, err := c.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, 'v')`, key)
			return err
		})
	}
	if err := insert(WithWriteDeadline(ctx, time.Now()), "skipped"); !isBusy(err) {
		t.Fatalf("writer held, deadline passed: want the busy error, got %v", err)
	}

	if _, err := bconn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := insert(ctx, "written"); err != nil {
		t.Fatalf("writer free: %v", err)
	}
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key IN ('skipped', 'written')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; want only the plain write", n, err)
	}
}
