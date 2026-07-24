// Package store opens and manages Recarto's concurrency-safe SQLite database.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
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
	// cache_size and temp_store are left at the pool default here (modernc's
	// SQLITE_TEMP_STORE=1 makes the default an on-disk temp store) and applied
	// transiently by WriteTxBulk instead. An in-memory temp store speeds a large
	// resolve — and on Windows keeps its spill files off the Defender scan path — but
	// set GLOBALLY it would also make an unrelated VACUUM (recarto compact --vacuum)
	// build its transient database, which can be as large as the whole store, in RAM:
	// an OOM hazard on a multi-GB database. Scoping both to the bulk write transaction
	// keeps the resolve win while leaving VACUUM and every other connection on disk.
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
	return s.writeTx(ctx, fn, false)
}

// bulkCacheMBDefault is the WriteTxBulk page-cache size in MiB when
// RECARTO_BULK_CACHE_MB is unset.
const bulkCacheMBDefault = 128

// bulkCacheKB returns the WriteTxBulk page-cache size in KiB (the unit SQLite's
// negative `PRAGMA cache_size` takes). RECARTO_BULK_CACHE_MB overrides the default
// for constrained environments; it is read per call so a test can set it with
// t.Setenv.
func bulkCacheKB() int {
	mb := bulkCacheMBDefault
	if v := os.Getenv("RECARTO_BULK_CACHE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mb = n
		}
	}
	return mb * 1024
}

// WriteTxBulk is WriteTx with a bulk-write connection profile: a large page cache
// (128 MiB, or RECARTO_BULK_CACHE_MB), an in-memory temp store, and foreign-key
// enforcement OFF for the duration of the transaction. All three are applied to the
// checked-out connection before BEGIN IMMEDIATE (foreign_keys and temp_store cannot be
// meaningfully toggled mid-transaction) and restored — under a detached context, so a
// cancelled ctx cannot skip the restore — after COMMIT/ROLLBACK. A connection whose
// profile cannot be restored is discarded rather than returned to the pool, so a
// mis-profiled connection never serves a later writer.
//
// CONTRACT (caller must guarantee): the transaction only (a) inserts child rows
// whose parent rows it has read in the same transaction, and (b) deletes or updates
// rows that no other table references. It must never delete or rewrite parent rows
// that FK CASCADE/SET NULL actions would otherwise clean up after — with foreign
// keys off, SQLite performs no such cascade and the store would be left with
// dangling children.
func (s *Store) WriteTxBulk(ctx context.Context, fn func(c Conn) error) error {
	return s.writeTx(ctx, fn, true)
}

// writeTx is the shared retry loop for WriteTx / WriteTxBulk. bulk selects the
// bulk-write connection profile for each attempt.
func (s *Store) writeTx(ctx context.Context, fn func(c Conn) error, bulk bool) error {
	for attempt := 0; ; attempt++ {
		err := s.writeTxOnce(ctx, fn, bulk)
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

// priorProfile records the connection settings WriteTxBulk overrode, captured before
// the bulk profile is applied so the restore reproduces them exactly regardless of the
// pool default.
type priorProfile struct {
	cacheSize int // PRAGMA cache_size before the bulk profile
	tempStore int // PRAGMA temp_store before the bulk profile
}

// execRestore reverses the bulk-write profile on conn, restoring the prior cache_size
// and temp_store captured on entry and re-enabling foreign keys (unconditionally ON for
// every pooled connection, per the DSN). It is a package variable so a test can force a
// restore failure and prove a connection that cannot be un-profiled is discarded.
var execRestore = func(ctx context.Context, conn *sql.Conn, prior priorProfile) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA cache_size=%d`, prior.cacheSize)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA temp_store=%d`, prior.tempStore)); err != nil {
		return err
	}
	return nil
}

// execApplyFKOff turns foreign-key enforcement off — the last step of entering the bulk
// profile, after the cache and temp store are already changed. It is a package variable
// so a test can force this final setup step to fail and exercise the partial-profile
// cleanup path (cache and temp_store already moved).
var execApplyFKOff = func(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`)
	return err
}

// restoreOrDiscard reverses the bulk-write profile on conn. If any restore step fails,
// the connection is discarded (Raw returning driver.ErrBadConn removes it from the pool)
// rather than returned to it, so a connection left with foreign keys off, an oversized
// cache, or an in-memory temp store can never serve a later writer. Callers pass a
// detached context so a cancelled caller ctx cannot skip the restore.
func restoreOrDiscard(ctx context.Context, conn *sql.Conn, prior priorProfile) {
	if err := execRestore(ctx, conn, prior); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// applyBulkProfile enters the bulk-write connection profile on conn before BEGIN — an
// enlarged page cache, an in-memory temp store, and foreign keys OFF — and returns a
// restore closure that reverses it under a detached context, discarding conn on a failed
// restore. It captures the prior cache_size and temp_store so the restore is exact
// regardless of the pool default.
//
// temp_store=MEMORY is applied here, not in the DSN, so ONLY the bulk transaction uses an
// in-memory temp store. A global in-memory temp store would make an unrelated VACUUM
// (recarto compact --vacuum) build its transient database — as large as the whole store —
// in RAM, risking OOM on a multi-GB store. Scoping it confines the in-memory temp store
// to the resolve transaction, whose temp size is bounded by edge counts.
//
// Changing temp_store immediately drops any existing TEMP tables on the connection; this
// is safe because the profile is entered before BEGIN, before the callback creates any
// TEMP table.
func applyBulkProfile(ctx context.Context, conn *sql.Conn) (restore func(), err error) {
	var prior priorProfile
	if err := conn.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&prior.cacheSize); err != nil {
		return nil, err
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&prior.tempStore); err != nil {
		return nil, err
	}
	// After the first mutating PRAGMA below, any failure may leave the connection partly
	// profiled, so every error path restores via restoreOrDiscard (which discards the
	// connection if the restore itself fails) before surfacing the error — a partly
	// profiled connection must never reach the pool. (This is the P1 fix: the prior code
	// rolled cache_size back inline in the foreign_keys=OFF failure branch but ignored
	// that rollback's error, so a failed rollback could return a 128 MiB / FK-uncertain
	// connection to the pool.)
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA cache_size=-%d`, bulkCacheKB())); err != nil {
		restoreOrDiscard(context.WithoutCancel(ctx), conn, prior)
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA temp_store=MEMORY`); err != nil {
		restoreOrDiscard(context.WithoutCancel(ctx), conn, prior)
		return nil, err
	}
	if err := execApplyFKOff(ctx, conn); err != nil {
		restoreOrDiscard(context.WithoutCancel(ctx), conn, prior)
		return nil, err
	}
	return func() {
		restoreOrDiscard(context.WithoutCancel(ctx), conn, prior)
	}, nil
}

// writeTxOnce performs a single attempt of the serialized transaction. wmu is
// held only for the duration of one attempt so backoff between retries does not
// block other in-process writers.
func (s *Store) writeTxOnce(ctx context.Context, fn func(c Conn) error, bulk bool) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if bulk {
		restore, err := applyBulkProfile(ctx, conn)
		if err != nil {
			return err
		}
		// Runs before the deferred conn.Close (LIFO), on every exit path below —
		// failed BEGIN, callback error, failed COMMIT, or success.
		defer restore()
	}

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
