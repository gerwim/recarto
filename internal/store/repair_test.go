package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic corruption
//
// Corruption is induced by overwriting whole PAGES of the database file with a
// byte pattern that is not a valid b-tree page header (0xA5 is not one of the
// legal page types 2/5/10/13), which SQLite reports as SQLITE_CORRUPT the moment
// it tries to init that page.
//
// Aiming the damage is what makes the tests meaningful. SQLite appends new pages
// at the end of the file, so writing table A, recording the file size, then
// writing table B puts B's pages beyond that offset: smashing everything past it
// damages B while A stays perfectly readable. That is exactly the reported
// failure mode — reads work all session, the first write into the damaged
// structure fails.
//
// NOTE: a zero-length file is NOT corruption — SQLite treats it as a brand-new
// empty database — so truncation cannot be used to simulate this.
// ---------------------------------------------------------------------------

// pageSizeOf reads the page size from the SQLite file header (bytes 16..17,
// big-endian; the value 1 encodes 65536).
func pageSizeOf(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdr := make([]byte, 100)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
	ps := int64(hdr[16])<<8 | int64(hdr[17])
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 {
		t.Fatalf("implausible page size %d", ps)
	}
	return ps
}

// smashPagesFrom overwrites every complete page at or after off and returns how
// many pages it damaged.
func smashPagesFrom(t *testing.T, path string, off int64) int {
	t.Helper()
	ps := pageSizeOf(t, path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := make([]byte, ps)
	for i := range junk {
		junk[i] = 0xA5
	}
	n := 0
	for p := ((off + ps - 1) / ps) * ps; p+ps <= fi.Size(); p += ps {
		if _, err := f.WriteAt(junk, p); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no pages to smash (offset %d, size %d)", off, fi.Size())
	}
	return n
}

// smashHeader destroys the file's magic string, which makes SQLite refuse the
// file outright (SQLITE_NOTADB) — the "nothing is salvageable" case.
func smashHeader(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("NOT A DATABASE\x00\x00"), 0); err != nil {
		t.Fatal(err)
	}
}

// seedPrecious populates every table repair must salvage.
func seedPrecious(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	err := st.WriteTx(ctx, func(c Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES ('repo_name','demo'),('graph_token:/wt','tok-abc')`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope,metric,value) VALUES ('lifetime','records',42)`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO sessions(id,agent,branch,worktree,started_at) VALUES ('s1','claude','main','/wt',100)`); err != nil {
			return err
		}
		for i := 0; i < seedMemories; i++ {
			id := fmt.Sprintf("m%03d", i)
			status := "active"
			if i >= seedActiveMemories {
				status = "superseded"
			}
			search := fmt.Sprintf("%s alpha bravo charlie payload %d", id, i)
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,branch,status,simhash)
				 VALUES (?,?,?,?,?,?,?,?,?,?)`,
				id, "learning", "title "+id, "body "+id, search, 1, 1, "main", status, int64(i)); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memory_refs(memory_id,ref_type,ref_value) VALUES (?,?,?)`,
				id, "file", "internal/store/store.go"); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memory_minhash(memory_id,band,hash) VALUES (?,?,?)`,
				id, 0, int64(i)); err != nil {
				return err
			}
			// The write path indexes only active memories; terminal rows are
			// deleted from the FTS index when they reach their terminal status.
			if status == "active" {
				if _, err := c.ExecContext(ctx,
					`INSERT INTO memories_fts(memory_id,search_text) VALUES (?,?)`, id, search); err != nil {
					return err
				}
			}
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memory_sync_outbox(memory_id,updated_at) VALUES (?,?)`, id, 1); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memory_tombstones(id,deleted_at,pushed) VALUES ('gone-1',5,0),('gone-2',6,1)`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memory_sync_merges(id,canonical_id,updated_at) VALUES ('inc-1','m000',7)`); err != nil {
			return err
		}
		for i := 0; i < seedDeltas; i++ {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO graph_deltas(id,worktree,path,qualified_name,kind,change,at)
				 VALUES (?,?,?,?,?,?,?)`,
				fmt.Sprintf("d%03d", i), "/wt", "a.go", "pkg.Fn", "function", "added", int64(i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

const (
	seedMemories       = 60
	seedActiveMemories = 45
	seedDeltas         = 20
)

// seedGraph appends a code graph large enough to occupy many fresh pages.
func seedGraph(t *testing.T, st *Store, files int) {
	t.Helper()
	ctx := context.Background()
	err := st.WriteTx(ctx, func(c Conn) error {
		for i := 0; i < files; i++ {
			fid := fmt.Sprintf("f%04d", i)
			if _, err := c.ExecContext(ctx,
				`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
				 VALUES (?,?,?,?,?,?,?,?)`,
				fid, "/wt", "pkg/"+fid+".go", "go", "hash", 0, 0, 0); err != nil {
				return err
			}
			for j := 0; j < 10; j++ {
				nid := fmt.Sprintf("n%04d_%02d", i, j)
				if _, err := c.ExecContext(ctx,
					`INSERT INTO nodes(id,file_id,kind,name,qualified_name,parent_name,signature,
					   start_byte,end_byte,start_line,end_line)
					 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
					nid, fid, "function", nid, "pkg."+nid, "pkg", "func()", 0, 1, 1, 2); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// buildStoreWithCorruptGraph is the reported scenario: a store whose memories
// are intact and readable but whose code-graph pages are damaged.
func buildStoreWithCorruptGraph(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()

	st, err := Open(path)
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
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	preciousEnd := fi.Size()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedGraph(t, st, 300)
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	smashPagesFrom(t, path, preciousEnd)
	assertCorrupt(t, path)
}

// assertCorrupt proves the file really is damaged before a repair is asked to
// fix it — otherwise a "repair worked" assertion would prove nothing.
func assertCorrupt(t *testing.T, path string) {
	t.Helper()
	db, err := openRepairSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = checkIntegrity(context.Background(), db, `PRAGMA quick_check`)
	if err == nil {
		t.Fatal("expected the seeded store to be corrupt, but quick_check passed")
	}
	if !IsCorrupt(err) {
		t.Fatalf("quick_check error not classified as corruption: %v", err)
	}
}

func scalar(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRepairSalvagesPreciousTablesAndDropsDerivedGraph(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !stats.SourceOpen {
		t.Fatal("SourceOpen = false, want true (the memories were readable)")
	}
	if stats.Quarantine == "" {
		t.Fatal("no quarantine path recorded")
	}
	if _, err := os.Stat(stats.Quarantine); err != nil {
		t.Fatalf("quarantined store missing: %v", err)
	}
	// The lock file is released.
	if _, err := os.Stat(repairLockPath(path)); !os.IsNotExist(err) {
		t.Fatalf("repair lock not released: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open repaired store: %v", err)
	}
	defer st.Close()

	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != seedMemories {
		t.Errorf("memories = %d, want %d", got, seedMemories)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memory_refs`); got != seedMemories {
		t.Errorf("memory_refs = %d, want %d", got, seedMemories)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memory_minhash`); got != seedMemories {
		t.Errorf("memory_minhash = %d, want %d", got, seedMemories)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memory_sync_outbox`); got != seedMemories {
		t.Errorf("memory_sync_outbox = %d, want %d", got, seedMemories)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memory_tombstones`); got != 2 {
		t.Errorf("memory_tombstones = %d, want 2", got)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memory_sync_merges`); got != 1 {
		t.Errorf("memory_sync_merges = %d, want 1", got)
	}
	if got := scalar(t, st.DB, `SELECT value FROM counters WHERE scope='lifetime' AND metric='records'`); got != 42 {
		t.Errorf("counters value = %d, want 42", got)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM sessions`); got != 1 {
		t.Errorf("sessions = %d, want 1", got)
	}
	// graph_deltas is a historical journal read alongside sessions — NOT
	// rebuildable graph state — so it must survive a repair.
	if got := scalar(t, st.DB, `SELECT count(*) FROM graph_deltas`); got != seedDeltas {
		t.Errorf("graph_deltas = %d, want %d", got, seedDeltas)
	}

	// The derived graph is dropped and its freshness token cleared, so the next
	// lazy reconcile rebuilds it from source.
	for _, tbl := range []string{"files", "nodes", "edges", "type_bindings", "edge_candidates"} {
		if got := scalar(t, st.DB, `SELECT count(*) FROM `+tbl); got != 0 {
			t.Errorf("%s = %d after repair, want 0 (derived state)", tbl, got)
		}
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM meta WHERE key LIKE 'graph_token:%'`); got != 0 {
		t.Errorf("graph_token meta keys survived repair (%d); the empty graph would look fresh", got)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM meta WHERE key='repo_name'`); got != 1 {
		t.Error("ordinary meta rows were not salvaged")
	}

	// FTS is rebuilt from the stored search_text — for ACTIVE memories only.
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories_fts`); got != seedActiveMemories {
		t.Errorf("memories_fts = %d, want %d (active only)", got, seedActiveMemories)
	}
	if got := scalar(t, st.DB,
		`SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'bravo'`); got != seedActiveMemories {
		t.Errorf("FTS match returned %d rows, want %d", got, seedActiveMemories)
	}
	if got := scalar(t, st.DB,
		`SELECT count(*) FROM memories_fts WHERE memory_id='m059'`); got != 0 {
		t.Error("a superseded memory was re-indexed into FTS")
	}

	// The replacement is healthy and carries the repair record.
	if err := checkIntegrity(context.Background(), st.DB, `PRAGMA integrity_check`); err != nil {
		t.Errorf("repaired store fails integrity_check: %v", err)
	}
	var ts, blob string
	if err := st.DB.QueryRow(
		`SELECT (SELECT value FROM meta WHERE key='repair_last_ts'),
		        (SELECT value FROM meta WHERE key='repair_last_stats')`).Scan(&ts, &blob); err != nil {
		t.Fatalf("repair metadata missing: %v", err)
	}
	if ts == "" || !strings.Contains(blob, `"memories"`) {
		t.Errorf("repair metadata unhelpful: ts=%q stats=%q", ts, blob)
	}
}

// TestRepairSurvivesAFreshProcess proves the replacement's data was folded out
// of its WAL before the swap: a brand-new connection (what the next process
// sees) must find the salvaged rows.
func TestRepairSurvivesAFreshProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)
	if _, err := Repair(path); err != nil {
		t.Fatal(err)
	}
	// No WAL may be left over from the build; the main file must be complete.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("repaired store left a %d-byte WAL behind", fi.Size())
	}
	db, err := openRepairSource(path) // a raw handle, as unrelated to the repair as possible
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := scalar(t, db, `SELECT count(*) FROM memories`); got != seedMemories {
		t.Fatalf("fresh connection sees %d memories, want %d", got, seedMemories)
	}
}

// TestRepairLeavesOriginalUntouchedWhenVerificationFails is the safety
// invariant: a repair that cannot produce a verified replacement must never
// have modified the store.
func TestRepairLeavesOriginalUntouchedWhenVerificationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	orig := repairVerify
	repairVerify = func(context.Context, *sql.DB) error {
		return &errCorruptCheck{detail: "injected verification failure"}
	}
	defer func() { repairVerify = orig }()

	if _, err := Repair(path); err == nil {
		t.Fatal("expected Repair to fail when the replacement does not verify")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("original store disappeared: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("original store changed size: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("original store modified at byte %d", i)
		}
	}
	for _, sfx := range dbSuffixes {
		if _, err := os.Stat(path + ".repair.tmp" + sfx); !os.IsNotExist(err) {
			t.Errorf("temporary replacement %q not cleaned up", path+".repair.tmp"+sfx)
		}
	}
	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 0 {
		t.Errorf("a failed repair quarantined the store anyway: %v", matches)
	}
}

// TestRepairRebuildsFromAnUnreadableSource covers availability-over-completeness:
// when not one byte can be salvaged, the store is still replaced with a healthy
// empty one and everything is preserved in the quarantine.
func TestRepairRebuildsFromAnUnreadableSource(t *testing.T) {
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

	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if stats.SourceOpen {
		t.Error("SourceOpen = true, want false for a file SQLite refuses entirely")
	}
	if stats.Salvaged() != 0 {
		t.Errorf("salvaged %d rows from an unreadable file", stats.Salvaged())
	}
	if stats.Quarantine == "" {
		t.Fatal("the unreadable original must still be quarantined")
	}
	if _, err := os.Stat(stats.Quarantine); err != nil {
		t.Fatalf("quarantined original missing: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("repaired store does not open: %v", err)
	}
	defer st.Close()
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != 0 {
		t.Errorf("memories = %d, want 0", got)
	}
	if err := checkIntegrity(context.Background(), st.DB, `PRAGMA integrity_check`); err != nil {
		t.Errorf("rebuilt store is not healthy: %v", err)
	}
}

// shrinkLockTimings makes the repair lock's heartbeat and staleness thresholds
// millisecond-scale so the liveness tests below run in real time instead of
// minutes. They are package vars precisely for this.
func shrinkLockTimings(t *testing.T, heartbeat, stale time.Duration) {
	t.Helper()
	prevHeartbeat, prevStale := repairHeartbeatInterval, repairLockStaleAfter
	repairHeartbeatInterval, repairLockStaleAfter = heartbeat, stale
	t.Cleanup(func() {
		repairHeartbeatInterval, repairLockStaleAfter = prevHeartbeat, prevStale
	})
}

func TestRepairIsSerializedByItsLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	// A live lock (as a concurrent repairer would hold) is a deterministic
	// stand-in for the race — no timing assumptions.
	release, err := acquireRepairLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Repair(path); err != ErrRepairContended {
		t.Fatalf("Repair with a live lock = %v, want ErrRepairContended", err)
	}
	release()

	if _, err := Repair(path); err != nil {
		t.Fatalf("Repair once the lock was released: %v", err)
	}
	if _, err := os.Stat(repairLockPath(path)); !os.IsNotExist(err) {
		t.Errorf("lock not released after repair: %v", err)
	}
}

// TestRepairLockHeartbeatProtectsALongRepair: a repair that takes longer than
// the staleness threshold must keep its lock. Before the heartbeat, a repair
// running past the fixed TTL could be "stolen" out from under itself and two
// processes would rebuild the same store at once.
func TestRepairLockHeartbeatProtectsALongRepair(t *testing.T) {
	shrinkLockTimings(t, 20*time.Millisecond, 250*time.Millisecond)
	path := filepath.Join(t.TempDir(), "store.db")

	release, err := acquireRepairLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	time.Sleep(3 * repairLockStaleAfter)

	release2, err := acquireRepairLock(path)
	if err == nil {
		release2()
		t.Fatal("a heartbeating lock was stolen while its owner was still repairing")
	}
	if err != ErrRepairContended {
		t.Fatalf("acquireRepairLock = %v, want ErrRepairContended", err)
	}
}

// TestWaitForRepairWaitsOutTheWholeRepair: openers must not give up on a live
// repair and open the store mid-swap, however long it takes.
func TestWaitForRepairWaitsOutTheWholeRepair(t *testing.T) {
	shrinkLockTimings(t, 20*time.Millisecond, 250*time.Millisecond)
	path := filepath.Join(t.TempDir(), "store.db")

	release, err := acquireRepairLock(path)
	if err != nil {
		t.Fatal(err)
	}

	waited := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		waitForRepair(path)
		waited <- time.Since(start)
	}()

	hold := 3 * repairLockStaleAfter
	select {
	case d := <-waited:
		release()
		t.Fatalf("waitForRepair returned after %v while the repair was still running", d)
	case <-time.After(hold):
	}

	release()
	select {
	case d := <-waited:
		if d < hold {
			t.Fatalf("waitForRepair returned after %v, want at least the %v hold", d, hold)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForRepair did not return after the lock was released")
	}
}

// TestRepairClaimsAnAbandonedLock: a lock whose owner was killed (no heartbeat)
// must not wedge repairs or openers forever.
func TestRepairClaimsAnAbandonedLock(t *testing.T) {
	shrinkLockTimings(t, 20*time.Millisecond, 250*time.Millisecond)
	path := filepath.Join(t.TempDir(), "store.db")
	buildStoreWithCorruptGraph(t, path)

	lock := repairLockPath(path)
	if err := os.WriteFile(lock, []byte("pid=999999 at=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	abandoned := time.Now().Add(-10 * repairLockStaleAfter)
	if err := os.Chtimes(lock, abandoned, abandoned); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { waitForRepair(path); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForRepair blocked on a lock nobody is heartbeating")
	}

	if _, err := Repair(path); err != nil {
		t.Fatalf("Repair did not claim an abandoned lock: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("lock not released after repair: %v", err)
	}
	claimed, _ := filepath.Glob(lock + ".stale-*")
	if len(claimed) != 0 {
		t.Errorf("claimed lock files left behind: %v", claimed)
	}
}

// TestQuarantineRollsBackWhenASidecarCannotMove: a WAL left beside the freshly
// installed replacement would be replayed into it and re-corrupt it, so failing
// to move one must abort the quarantine entirely rather than proceed.
func TestQuarantineRollsBackWhenASidecarCannotMove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	if err := os.WriteFile(path, []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}

	const ts = int64(1_700_000_000)
	q := fmt.Sprintf("%s.corrupt-%d", path, ts)
	// A non-empty directory at the WAL's destination makes its rename fail
	// deterministically, on every platform.
	if err := os.MkdirAll(filepath.Join(q+"-wal", "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := quarantineExisting(path, ts)
	if err == nil {
		t.Fatalf("quarantineExisting succeeded (%q) despite an unmovable WAL", got)
	}
	if !strings.Contains(err.Error(), "-wal") {
		t.Errorf("error does not name the blocking sidecar: %v", err)
	}
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != "main" {
		t.Errorf("main file not rolled back: %q %v", b, rerr)
	}
	if b, rerr := os.ReadFile(path + "-wal"); rerr != nil || string(b) != "wal" {
		t.Errorf("WAL not left in place: %q %v", b, rerr)
	}
	if _, serr := os.Stat(q); !os.IsNotExist(serr) {
		t.Errorf("a quarantined main file was left behind at %s", q)
	}
}

// TestRepairSalvagesWhenSchemaEnumerationFails: a sqlite_master scan can die on
// one damaged page while every table we care about is still readable one
// PRAGMA table_info at a time — that must not be mistaken for "nothing is
// readable, install an empty store".
func TestRepairSalvagesWhenSchemaEnumerationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedPrecious(t, st)
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	orig := listSourceTables
	listSourceTables = func(context.Context, *sql.DB) (map[string]bool, error) {
		return nil, errors.New("injected sqlite_master read failure")
	}
	defer func() { listSourceTables = orig }()

	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if got := stats.Tables["memories"].Salvaged; got != seedMemories {
		t.Errorf("salvaged %d memories, want %d — enumeration failure must not skip salvage", got, seedMemories)
	}
	if got := stats.Tables["graph_deltas"].Salvaged; got != seedDeltas {
		t.Errorf("salvaged %d graph_deltas, want %d", got, seedDeltas)
	}
	if !stats.SourceOpen {
		t.Error("SourceOpen = false although rows were salvaged blind")
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != seedMemories {
		t.Errorf("memories = %d after repair, want %d", got, seedMemories)
	}
}

// TestSalvageDoesNotCountRowsTheReplacementRejected: INSERT OR IGNORE swallows a
// CHECK/UNIQUE violation silently, so counting an exec as a salvage overstates
// what was actually recovered — in the stats, in the repair metadata, and in
// what `recarto repair` tells the user.
func TestSalvageDoesNotCountRowsTheReplacementRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")

	// A source whose memories table has the current columns but NO CHECK
	// constraint, so it can hold rows the real schema refuses.
	db, err := openRepairTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE memories (
  id          TEXT PRIMARY KEY,
  kind        TEXT NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  body        TEXT NOT NULL DEFAULT '',
  search_text TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  status      TEXT NOT NULL DEFAULT 'active'
);`); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"learning", "learning", "learning", "garbled-kind", "garbled-kind"} {
		if _, err := db.Exec(
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("m%d", i), kind, "t", "b", "t b", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	ts := stats.Tables["memories"]
	if ts.Salvaged != 3 {
		t.Errorf("Salvaged = %d, want 3 (only the rows the replacement accepted)", ts.Salvaged)
	}
	if ts.Lost != 2 {
		t.Errorf("Lost = %d, want 2", ts.Lost)
	}
	if !strings.Contains(ts.Note, "rejected") {
		t.Errorf("Note = %q, want it to mention the rejected rows", ts.Note)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != 3 {
		t.Errorf("memories = %d in the repaired store, want 3", got)
	}
}

func TestPruneQuarantinesKeepsNewestAndDropsExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")

	now := time.Now()
	ancient := fmt.Sprintf("%s.corrupt-%d", path, now.Add(-30*24*time.Hour).Unix())
	stale := fmt.Sprintf("%s.corrupt-%d", path, now.Add(-8*24*time.Hour).Unix())
	recent := fmt.Sprintf("%s.corrupt-%d", path, now.Add(-time.Hour).Unix())
	for _, p := range []string{ancient, stale, recent} {
		for _, sfx := range []string{"", "-wal"} {
			if err := os.WriteFile(p+sfx, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	pruneQuarantines(path)

	for _, p := range []string{ancient, stale} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expired quarantine %s not pruned", filepath.Base(p))
		}
		if _, err := os.Stat(p + "-wal"); !os.IsNotExist(err) {
			t.Errorf("expired quarantine WAL %s-wal not pruned", filepath.Base(p))
		}
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent quarantine was pruned: %v", err)
	}

	// The newest set is kept even when it is itself past the retention window —
	// there must always be something left to inspect.
	pruneQuarantines(path)
	if err := os.Remove(recent); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recent + "-wal"); err != nil {
		t.Fatal(err)
	}
	veryOld := fmt.Sprintf("%s.corrupt-%d", path, now.Add(-90*24*time.Hour).Unix())
	older := fmt.Sprintf("%s.corrupt-%d", path, now.Add(-100*24*time.Hour).Unix())
	for _, p := range []string{veryOld, older} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneQuarantines(path)
	if _, err := os.Stat(veryOld); err != nil {
		t.Errorf("the newest quarantine must be kept regardless of age: %v", err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Error("an older expired quarantine should have been pruned")
	}
}

// TestRepairSalvagesFromAnOlderSchema proves salvage is driven by the columns
// the two schemas actually share, not by hard-coded latest-schema SELECTs:
// repair may well run while Migrate itself is failing, so the source can be at
// an older schema version.
func TestRepairSalvagesFromAnOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")

	// A v1-era store: memories without the v4 decay columns, and none of the
	// v19-v21 journal/sync tables.
	db, err := openRepairTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
		 VALUES ('old-1','learning','ancient','body','ancient body','1','1','active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_refs(memory_id,ref_type,ref_value) VALUES ('old-1','file','a.go')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !stats.SourceOpen {
		t.Fatal("SourceOpen = false for a readable older-schema store")
	}
	if got := stats.Tables["memories"].Salvaged; got != 1 {
		t.Errorf("salvaged %d memories from the older schema, want 1", got)
	}
	if note := stats.Tables["memory_sync_outbox"].Note; note == "" {
		t.Error("a table absent from the older source schema should be noted, not silently skipped")
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var title string
	if err := st.DB.QueryRow(`SELECT title FROM memories WHERE id='old-1'`).Scan(&title); err != nil {
		t.Fatalf("older-schema memory not salvaged: %v", err)
	}
	if title != "ancient" {
		t.Errorf("title = %q, want %q", title, "ancient")
	}
	// Columns the source did not have take the destination's defaults.
	if got := scalar(t, st.DB, `SELECT recall_count FROM memories WHERE id='old-1'`); got != 0 {
		t.Errorf("recall_count = %d, want the schema default 0", got)
	}
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories_fts WHERE memories_fts MATCH 'ancient'`); got != 1 {
		t.Errorf("FTS rebuild found %d rows, want 1", got)
	}
}

// TestRepairOnAMissingStoreCreatesAHealthyOne keeps `recarto repair` usable as a
// plain "give me a store" command: nothing to quarantine, nothing to salvage.
func TestRepairOnAMissingStoreCreatesAHealthyOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "store.db")
	stats, err := Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if stats.Quarantine != "" {
		t.Errorf("quarantine = %q, want empty", stats.Quarantine)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := scalar(t, st.DB, `SELECT count(*) FROM memories`); got != 0 {
		t.Errorf("memories = %d, want 0", got)
	}
}
