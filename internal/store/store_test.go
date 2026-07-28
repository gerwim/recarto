package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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

	var tempStore int
	if err := st.DB.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil {
		t.Fatal(err)
	}
	// The pool default must NOT be an in-memory temp store: MEMORY is applied only per
	// bulk transaction (WriteTxBulk), so a global VACUUM keeps its transient DB on disk.
	if tempStore != 0 {
		t.Fatalf("temp_store = %d, want 0 (DEFAULT/on-disk; MEMORY is scoped to WriteTxBulk)", tempStore)
	}
}

// TestWriteTxBulkRestoresPragmas: after a bulk transaction — whether it commits or
// its callback returns an error — the connection profile (foreign_keys, cache_size)
// must be back to the pool default, so a later ordinary writer is unaffected. A
// single-connection pool guarantees the same connection is inspected afterward.
func TestWriteTxBulkRestoresPragmas(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1) // one conn: the post-tx PRAGMA check reuses it
	ctx := context.Background()

	assertProfile := func(when string) {
		var fk, cache int
		if err := st.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("%s: foreign_keys = %d, want 1 (restored)", when, fk)
		}
		if err := st.DB.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil {
			t.Fatal(err)
		}
		if cache != -2000 {
			t.Fatalf("%s: cache_size = %d, want -2000 (default restored)", when, cache)
		}
		var temp int
		if err := st.DB.QueryRow("PRAGMA temp_store").Scan(&temp); err != nil {
			t.Fatal(err)
		}
		if temp != 0 {
			t.Fatalf("%s: temp_store = %d, want 0 (default restored)", when, temp)
		}
	}

	// Inside the tx, the profile must be active.
	if err := st.WriteTxBulk(ctx, func(c Conn) error {
		var fk, cache int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			return err
		}
		if fk != 0 {
			t.Errorf("in bulk tx: foreign_keys = %d, want 0", fk)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cache); err != nil {
			return err
		}
		if cache != -bulkCacheKB() {
			t.Errorf("in bulk tx: cache_size = %d, want %d", cache, -bulkCacheKB())
		}
		var temp int
		if err := c.QueryRowContext(ctx, "PRAGMA temp_store").Scan(&temp); err != nil {
			return err
		}
		if temp != 2 {
			t.Errorf("in bulk tx: temp_store = %d, want 2 (memory)", temp)
		}
		return nil
	}); err != nil {
		t.Fatalf("WriteTxBulk (commit path): %v", err)
	}
	assertProfile("after commit")

	// The callback-error (rollback) path must restore the profile too.
	wantErr := context.Canceled
	if err := st.WriteTxBulk(ctx, func(c Conn) error { return wantErr }); err != wantErr {
		t.Fatalf("WriteTxBulk error path returned %v, want %v", err, wantErr)
	}
	assertProfile("after rollback")

	// A subsequent ordinary WriteTx must still enforce foreign keys — proving the
	// bulk profile did not leak FK-off onto the pooled connection.
	if _, err := st.DB.ExecContext(ctx, `CREATE TABLE parent(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))`); err != nil {
		t.Fatal(err)
	}
	orphanErr := st.WriteTx(ctx, func(c Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO child(id, pid) VALUES (1, 999)`)
		return err
	})
	if orphanErr == nil {
		t.Fatal("WriteTx after WriteTxBulk did not enforce foreign keys (orphan insert succeeded)")
	}
}

// TestWriteTxBulkDiscardsConnOnFailedRestore: if the profile cannot be restored,
// the connection must be discarded, not returned to the pool with foreign keys
// still off. A one-connection pool plus a forced restore failure proves it: the
// next operation must observe foreign_keys=1, which is only possible on a fresh
// connection (the discarded one still had FK off).
func TestWriteTxBulkDiscardsConnOnFailedRestore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1)
	ctx := context.Background()

	orig := execRestore
	execRestore = func(context.Context, *sql.Conn, priorProfile) error {
		return context.DeadlineExceeded // force the restore to fail
	}
	defer func() { execRestore = orig }()

	fkInTx := -1
	if err := st.WriteTxBulk(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkInTx)
	}); err != nil {
		t.Fatalf("WriteTxBulk: %v", err)
	}
	if fkInTx != 0 {
		t.Fatalf("bulk profile not applied: foreign_keys in tx = %d, want 0", fkInTx)
	}

	// The profiled connection was discarded on the failed restore, so the pool must
	// hand out a fresh connection with foreign keys back on.
	var fk int
	if err := st.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d after failed restore, want 1: the FK-off connection was reused", fk)
	}
}

// TestWriteTxBulkCleansProfileOnSetupFailure: if entering the bulk profile fails after
// the cache and temp store were already changed, the connection must be cleaned up
// (restored, or discarded) — never returned to the pool still carrying the 128 MiB cache
// or the in-memory temp store. This guards the P1 fix: the old code rolled cache_size
// back inline and ignored that rollback's error.
func TestWriteTxBulkCleansProfileOnSetupFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1)
	ctx := context.Background()

	orig := execApplyFKOff
	execApplyFKOff = func(context.Context, *sql.Conn) error {
		return context.DeadlineExceeded // fail the last setup step (cache+temp already moved)
	}
	defer func() { execApplyFKOff = orig }()

	ran := false
	if err := st.WriteTxBulk(ctx, func(Conn) error { ran = true; return nil }); err == nil {
		t.Fatal("WriteTxBulk should surface the profile-setup failure")
	}
	if ran {
		t.Fatal("callback ran despite the profile setup failing")
	}

	// The enlarged cache and in-memory temp store must not have leaked to the pool.
	var cache, temp, fk int
	if err := st.DB.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil {
		t.Fatal(err)
	}
	if cache != -2000 {
		t.Fatalf("cache_size = %d after setup failure, want -2000 (not leaked)", cache)
	}
	if err := st.DB.QueryRow("PRAGMA temp_store").Scan(&temp); err != nil {
		t.Fatal(err)
	}
	if temp != 0 {
		t.Fatalf("temp_store = %d after setup failure, want 0 (not leaked)", temp)
	}
	if err := st.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d after setup failure, want 1", fk)
	}
}

// TestWriteTxBulkDiscardsConnOnFailedSetupRestore: when the profile setup fails AND the
// cleanup restore also fails, the connection must be discarded, not pooled. execApplyFKOff
// really turns foreign keys off, then reports failure; with the restore forced to fail,
// the only way the next checkout can see foreign_keys=1 is a fresh (discarded-and-reopened)
// connection.
func TestWriteTxBulkDiscardsConnOnFailedSetupRestore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1)
	ctx := context.Background()

	origApply := execApplyFKOff
	execApplyFKOff = func(ctx context.Context, conn *sql.Conn) error {
		_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`) // really turn it off
		return context.DeadlineExceeded                         // then report the step failed
	}
	defer func() { execApplyFKOff = origApply }()

	origRestore := execRestore
	execRestore = func(context.Context, *sql.Conn, priorProfile) error {
		return context.DeadlineExceeded // cleanup restore also fails -> must discard
	}
	defer func() { execRestore = origRestore }()

	if err := st.WriteTxBulk(ctx, func(Conn) error { return nil }); err == nil {
		t.Fatal("WriteTxBulk should surface the profile-setup failure")
	}

	var fk int
	if err := st.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d after failed setup restore, want 1: the FK-off connection was reused", fk)
	}
}

// TestWriteTxBulkRestoresProfileOnCanceledContext: a caller context cancelled mid-tx must
// not leave the pooled connection profiled. Cleanup runs under a detached context, and a
// restore that fails discards the connection, so either way the pool never serves a
// connection still carrying the bulk profile.
func TestWriteTxBulkRestoresProfileOnCanceledContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1)

	ctx, cancel := context.WithCancel(context.Background())
	err = st.WriteTxBulk(ctx, func(c Conn) error {
		cancel()         // cancel the caller ctx mid-transaction
		return ctx.Err() // return the cancellation error -> rollback path
	})
	if err != context.Canceled {
		t.Fatalf("WriteTxBulk = %v, want context.Canceled", err)
	}

	// Despite the cancellation, cleanup ran: the pooled connection is not left profiled.
	var cache, temp, fk int
	if err := st.DB.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil {
		t.Fatal(err)
	}
	if cache != -2000 {
		t.Fatalf("cache_size = %d after cancelled bulk tx, want -2000", cache)
	}
	if err := st.DB.QueryRow("PRAGMA temp_store").Scan(&temp); err != nil {
		t.Fatal(err)
	}
	if temp != 0 {
		t.Fatalf("temp_store = %d after cancelled bulk tx, want 0", temp)
	}
	if err := st.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d after cancelled bulk tx, want 1", fk)
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

// ---------------------------------------------------------------------------
// Corruption recovery at open time
//
// The corruption helpers (page smashing, seeding) live in repair_test.go.
// ---------------------------------------------------------------------------

// buildStoreWithCorruptMemories is the mirror image of
// buildStoreWithCorruptGraph: the code graph is written first, so smashing
// everything past that point damages the MEMORY pages while leaving the graph
// readable. That is the shape of the reported failure — graph reads work all
// session, then record_memory fails.
func buildStoreWithCorruptMemories(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedGraph(t, st, 200)
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	graphEnd := fi.Size()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedPrecious(t, st)
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	smashPagesFrom(t, path, graphEnd)
	assertCorrupt(t, path)
}

func quarantines(t *testing.T, path string) []string {
	t.Helper()
	all, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range all {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			out = append(out, m)
		}
	}
	return out
}

// TestOpenAutoRepairsAnUnopenableStore is the reactive open-time trigger: a
// store SQLite refuses outright must not take the session down — Open rebuilds
// it and returns a usable store, keeping the damaged file for forensics.
func TestOpenAutoRepairsAnUnopenableStore(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedPrecious(t, st)
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	smashHeader(t, path)

	// Sanity: without repair this open fails.
	if _, err := openWith(path, 5000, openConfig{}, false); err == nil {
		t.Fatal("expected a header-smashed store to be unopenable")
	} else if !IsCorrupt(err) {
		t.Fatalf("unopenable store error not classified as corruption: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("Open did not self-heal: %v", err)
	}
	defer st.Close()
	if err := st.WriteTx(context.Background(), func(c Conn) error {
		_, err := c.ExecContext(context.Background(),
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at)
			 VALUES ('fresh','learning','t','b','t b',1,1)`)
		return err
	}); err != nil {
		t.Fatalf("repaired store is not writable: %v", err)
	}
	if q := quarantines(t, path); len(q) != 1 {
		t.Fatalf("quarantined files = %v, want exactly one", q)
	}
}

// TestOpenHonorsAutoRepairKillSwitch: RECARTO_AUTO_REPAIR=0 must surface the
// original error untouched, so an operator can inspect the store before
// anything rewrites it.
func TestOpenHonorsAutoRepairKillSwitch(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "0")
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	smashHeader(t, path)

	if _, err := Open(path); err == nil {
		t.Fatal("Open repaired the store despite RECARTO_AUTO_REPAIR=0")
	} else if !IsCorrupt(err) {
		t.Fatalf("error = %v, want the original corruption error", err)
	}
	if q := quarantines(t, path); len(q) != 0 {
		t.Fatalf("store was quarantined despite the kill switch: %v", q)
	}
}

// TestOpenWithQuickCheckRepairsAStoreThatOpensCleanly is the reported repro: the
// store opens and reads perfectly, so nothing reactive ever fires — only the
// proactive check catches it, before the session's first write does.
func TestOpenWithQuickCheckRepairsAStoreThatOpensCleanly(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	// A plain open succeeds and serves memory reads — this is why the damage
	// went unnoticed until a write hit it.
	plain, err := Open(path)
	if err != nil {
		t.Fatalf("plain Open failed, so this is not the reported scenario: %v", err)
	}
	var n int
	if err := plain.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("memory reads should still work on this store: %v", err)
	}
	plain.Close()
	if q := quarantines(t, path); len(q) != 0 {
		t.Fatalf("a plain open must not repair: %v", q)
	}

	st, err := Open(path, WithQuickCheck())
	if err != nil {
		t.Fatalf("Open with quick check: %v", err)
	}
	defer st.Close()
	if q := quarantines(t, path); len(q) != 1 {
		t.Fatalf("quick check did not repair the store (quarantines: %v)", q)
	}
	if err := checkIntegrity(context.Background(), st.DB, `PRAGMA integrity_check`); err != nil {
		t.Errorf("store still corrupt after quick-check repair: %v", err)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != n {
		t.Errorf("memories = %d after repair, want the %d that were readable", got, n)
	}
}

// TestQuickCheckStampRateLimitsTheCheck: the check is expensive on a large
// store, so a passing one is stamped and not repeated within the interval.
func TestQuickCheckStampRateLimitsTheCheck(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
	path := filepath.Join(t.TempDir(), "store.db")

	st, err := Open(path, WithQuickCheck())
	if err != nil {
		t.Fatal(err)
	}
	seedPrecious(t, st)
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(quickCheckStamp(path)); err != nil {
		t.Fatalf("a passing quick check must leave a stamp: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedGraph(t, st, 200)
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	smashPagesFrom(t, path, fi.Size())
	assertCorrupt(t, path)

	// Stamp is fresh → the check is skipped and the damage is not noticed yet.
	st, err = Open(path, WithQuickCheck())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if q := quarantines(t, path); len(q) != 0 {
		t.Fatalf("the check ran despite a fresh stamp: %v", q)
	}

	// Age the stamp past the interval → the check runs and repairs.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(quickCheckStamp(path), old, old); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, WithQuickCheck())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if q := quarantines(t, path); len(q) != 1 {
		t.Fatalf("an expired stamp must let the check run again (quarantines: %v)", q)
	}
}

func TestQuickCheckIntervalZeroDisablesTheCheck(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "0")
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	st, err := Open(path, WithQuickCheck())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if q := quarantines(t, path); len(q) != 0 {
		t.Fatalf("the check ran despite RECARTO_QUICKCHECK_INTERVAL=0: %v", q)
	}
	if _, err := os.Stat(quickCheckStamp(path)); !os.IsNotExist(err) {
		t.Error("a disabled check must not stamp")
	}
}

// TestWriteTxWrapsCorruptionWithGuidance: a write that hits corruption inside a
// live session cannot be repaired in place (renaming the store out from under an
// open pool is not safe), so it must at least tell the caller what to do — this
// is the message record_memory now returns instead of a bare SQLite error.
func TestWriteTxWrapsCorruptionWithGuidance(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptMemories(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("this store is supposed to open cleanly: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	err = st.WriteTx(ctx, func(c Conn) error {
		// Rewriting every row is guaranteed to walk into the damaged pages.
		_, err := c.ExecContext(ctx, `UPDATE memories SET title = title || '!'`)
		return err
	})
	if err == nil {
		t.Fatal("expected the write to fail on the corrupt store")
	}
	if !IsCorrupt(err) {
		t.Fatalf("wrapped error lost its corruption classification: %v", err)
	}
	for _, want := range []string{"memory store is corrupt", "recarto repair"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// And the store heals on the next open, which is what the guidance promises.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	healed, err := Open(path, WithQuickCheck())
	if err != nil {
		t.Fatalf("next open did not heal the store: %v", err)
	}
	defer healed.Close()
	if err := healed.WriteTx(ctx, func(c Conn) error {
		_, err := c.ExecContext(ctx, `UPDATE memories SET title = title || '!'`)
		return err
	}); err != nil {
		t.Fatalf("write still failing after the healing open: %v", err)
	}
}
