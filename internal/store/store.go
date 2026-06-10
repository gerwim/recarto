// Package store opens and manages Recarto's concurrency-safe SQLite database.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (driver name: "sqlite")
)

// Conn is the subset of *sql.Conn that WriteTx callbacks use.
type Conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	// PrepareContext lets a hot write loop compile a statement once and reuse it
	// across many rows in the same transaction (the prepared Stmt is bound to this
	// connection, so it participates in the open BEGIN IMMEDIATE).
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// Store wraps the SQLite connection pool plus an in-process write mutex.
type Store struct {
	DB   *sql.DB
	wmu  sync.Mutex
	path string
}

// Open creates (if needed) and opens the store at path, applies PRAGMAs on
// every connection, and runs migrations.
func Open(path string) (*Store, error) { return open(path, 5000) }

// open is Open with a configurable busy_timeout (ms), so tests can force
// SQLITE_BUSY quickly to exercise the WriteTx retry layer.
func open(path string, busyTimeoutMs int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s"+
		"?_pragma=busy_timeout(%d)"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(NORMAL)"+
		"&_pragma=foreign_keys(ON)", path, busyTimeoutMs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL allows many readers + one writer across processes; in-process we
	// additionally serialize writers via wmu, so a small pool is fine.
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, path: path}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying pool.
func (s *Store) Close() error { return s.DB.Close() }

// Path returns the database file path this store was opened from.
func (s *Store) Path() string { return s.path }

// Checkpoint flushes the WAL into the main database file and truncates the WAL,
// bounding on-disk growth during a long sequence of writes (e.g. a full graph
// index, where the WAL would otherwise accumulate every inserted row until the
// next passive auto-checkpoint). PRAGMA wal_checkpoint returns a status row
// (busy, log frames, checkpointed frames); a non-zero busy is not an error — it
// just means a reader held the WAL and the truncation will happen on a later
// call — so only a genuine statement failure is surfaced.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	err := s.DB.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed)
	return err
}

// maxWriteRetries bounds how many extra times WriteTx restarts a transaction
// that could not acquire the write lock. busy_timeout already blocks each
// attempt (up to 5s in production); this layer covers the rare case where even
// that is exceeded under extreme concurrent cold-start. Exhausting the retries
// surfaces the underlying SQLITE_BUSY so genuine, persistent locking is not
// hidden from callers.
const maxWriteRetries = 6

// WriteTx runs fn inside a serialized `BEGIN IMMEDIATE` transaction. The
// in-process mutex plus IMMEDIATE + busy_timeout guarantee that concurrent
// writers (this process and others) never corrupt state or lose updates. A
// `BEGIN IMMEDIATE` that loses the race for the write lock makes no changes, so
// the whole transaction is safely retried with bounded backoff on SQLITE_BUSY.
func (s *Store) WriteTx(ctx context.Context, fn func(c Conn) error) error {
	for attempt := 0; ; attempt++ {
		err := s.writeTxOnce(ctx, fn)
		if err == nil || !isBusy(err) || attempt >= maxWriteRetries {
			return err
		}
		// The lock is held by another writer; back off (releasing wmu so other
		// in-process work can proceed) and retry.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyBackoff(attempt)):
		}
	}
}

// writeTxOnce performs a single attempt of the serialized transaction. wmu is
// held only for the duration of one attempt so backoff between retries does not
// block other in-process writers.
func (s *Store) writeTxOnce(ctx context.Context, fn func(c Conn) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err // no transaction open yet; nothing to roll back
	}
	// The rollback must run even when — especially when — ctx is already
	// cancelled, which is the usual reason we are rolling back. Issued with the
	// caller's ctx it would be skipped by the ctx check in database/sql, and the
	// connection would return to the pool with the transaction still open:
	// uncommitted writes visible to later readers on that connection, and every
	// later writer failing with "cannot start a transaction within a transaction".
	rollback := func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	}
	if err := fn(conn); err != nil {
		rollback()
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		rollback()
		return err
	}
	return nil
}

// isBusy reports whether err is a transient SQLite lock contention error, the
// only class of WriteTx failure that is safe to retry.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// busyBackoff returns the wait before retry attempt n: 10ms, 20ms, 40ms, ...
// capped at 200ms, plus up to 50% jitter to avoid a multi-process thundering
// herd all retrying in lockstep.
func busyBackoff(attempt int) time.Duration {
	base := 10 * time.Millisecond << attempt
	if base > 200*time.Millisecond || base <= 0 {
		base = 200 * time.Millisecond
	}
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}
