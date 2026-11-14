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

// Metrics returns the names of scope's metrics that start with prefix.
func (l *Ledger) Metrics(ctx context.Context, scope, prefix string) ([]string, error) {
	// substr, not LIKE, so % and _ in a file path match literally.
	rows, err := l.st.DB.QueryContext(ctx,
		`SELECT metric FROM counters WHERE scope = ? AND substr(metric, 1, length(?)) = ?`,
		scope, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // a read-only cursor: closing it cannot lose data
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Delete removes scope's metrics that start with any of prefixes.
func (l *Ledger) Delete(ctx context.Context, scope string, prefixes ...string) error {
	return l.st.WriteTx(ctx, func(c store.Conn) error {
		for _, p := range prefixes {
			// substr, not LIKE, so % and _ in a file path match literally.
			if _, err := c.ExecContext(ctx,
				`DELETE FROM counters WHERE scope = ? AND substr(metric, 1, length(?)) = ?`,
				scope, p, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// Claim sets counters[scope, metric] to 1 unless it already exists and reports
// whether this call created it: of concurrent callers, in this process or
// another, exactly one gets true.
func (l *Ledger) Claim(ctx context.Context, scope, metric string) (bool, error) {
	var won bool
	err := l.st.WriteTx(ctx, func(c store.Conn) error {
		res, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES (?, ?, 1)
			 ON CONFLICT(scope, metric) DO NOTHING`, scope, metric)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		// WriteTx may retry this callback, so only the committed attempt's result counts.
		won = n == 1
		return err
	})
	if err != nil {
		return false, err
	}
	return won, nil
}
