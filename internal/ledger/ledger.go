// Package ledger maintains atomic token/usage counters in the store.
package ledger

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gerwim/recarto/internal/store"
)

// Ledger updates counters atomically, safe across goroutines and processes.
type Ledger struct {
	st *store.Store
}

// New returns a Ledger backed by st.
func New(st *store.Store) *Ledger { return &Ledger{st: st} }

// Incr atomically adds n to counters[scope, metric], inserting if absent.
func (l *Ledger) Incr(ctx context.Context, scope, metric string, n int64) error {
	return l.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES (?, ?, ?)
			 ON CONFLICT(scope, metric) DO UPDATE SET value = value + excluded.value`,
			scope, metric, n)
		return err
	})
}

// Get returns the counter value, or 0 if it does not exist.
func (l *Ledger) Get(ctx context.Context, scope, metric string) (int64, error) {
	var v int64
	err := l.st.DB.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE scope=? AND metric=?`, scope, metric).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
}
