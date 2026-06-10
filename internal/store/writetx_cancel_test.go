package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestWriteTxRollsBackOnCancelledContext: when the caller's context is
// cancelled mid-transaction, WriteTx must still roll the transaction back. If
// the ROLLBACK is issued with the same cancelled context it never executes,
// and the connection returns to the pool with the transaction still open —
// uncommitted writes then leak to later readers handed that pooled connection,
// and later writers fail with "cannot start a transaction within a
// transaction". This is exactly the state an MCP client timeout produces.
func TestWriteTxRollsBackOnCancelledContext(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = st.WriteTx(ctx, func(c Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES ('cancelled-tx','1')`); err != nil {
			return err
		}
		cancel() // the client timeout lands here, mid-transaction
		_, err := c.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES ('after','1')`)
		return err
	})
	if err == nil {
		t.Fatal("want error from cancelled WriteTx, got nil")
	}

	// The uncommitted insert must not be visible anywhere — including on the
	// very pooled connection the aborted transaction ran on.
	for range 8 { // probe more conns than the pool holds
		var n int
		if err := st.DB.QueryRow(
			`SELECT COUNT(*) FROM meta WHERE key='cancelled-tx'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("uncommitted insert visible after cancellation: transaction was not rolled back")
		}
	}

	// And the store must still accept writes (no dangling open transaction).
	if err := st.WriteTx(context.Background(), func(c Conn) error {
		_, err := c.ExecContext(context.Background(),
			`INSERT INTO meta(key,value) VALUES ('next-write','1')`)
		return err
	}); err != nil {
		t.Fatalf("WriteTx after cancelled WriteTx: %v", err)
	}
}
