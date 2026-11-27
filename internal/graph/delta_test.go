package graph

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/store"
)

func TestDeltaJournalAddedChangedRemoved(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	// First index of the file journals NOTHING (no old set — fresh clones and
	// wipe migrations must not flood the journal).
	indexInto(t, e, wt, "a.go", "package p\nfunc Keep() {}\nfunc Drop() {}\n")
	d, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(d) != 0 {
		t.Fatalf("first index journaled %d deltas, want 0: %+v", total, d)
	}

	// Reindex: Keep changes body, Drop disappears, Add appears.
	indexInto(t, e, wt, "a.go", "package p\nfunc Keep() { _ = 1 }\nfunc Add() {}\n")
	d, total, err = e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Keep": "changed", "Drop": "removed", "Add": "added"}
	if total != len(want) {
		t.Fatalf("total = %d, want %d: %+v", total, len(want), d)
	}
	for _, dd := range d {
		if want[dd.QualifiedName] != dd.Change {
			t.Errorf("%s = %s, want %s", dd.QualifiedName, dd.Change, want[dd.QualifiedName])
		}
		if dd.Path != "a.go" {
			t.Errorf("path = %q, want a.go", dd.Path)
		}
	}
}

func TestDeltaFoldLatestDisposition(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\n")

	// added then changed → reported once as added (it did not exist at baseline).
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\n")
	// added then removed → nets out to nothing.
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\nfunc C() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\n")

	d, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, dd := range d {
		got[dd.QualifiedName] = dd.Change
	}
	if got["B"] != "added" {
		t.Errorf("B = %q, want added (added+changed folds to added)", got["B"])
	}
	if _, ok := got["C"]; ok {
		t.Error("C must net out (added then removed within the window)")
	}
	if total != len(d) {
		t.Errorf("total %d != len %d after folding", total, len(d))
	}
}

func TestDeltaJournalOnFileDelete(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "gone.go", "package p\nfunc Bye() {}\n")
	n := e.clk.Now().UnixMilli()
	if err := e.deleteFile(ctx, wt, "gone.go", stamp{at: n, seen: n, now: n}); err != nil {
		t.Fatal(err)
	}
	d, _, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, dd := range d {
		if dd.QualifiedName == "Bye" && dd.Change == "removed" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("deleted file's symbols must journal as removed: %+v", d)
	}
}

func TestDeltaOverloadSetUnchangedJournalsNothingForIt(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "C.cs", "class C {\n  void Run(int a) {}\n  void Run(string b) {}\n  void Other() {}\n}\n")
	// Change ONLY Other — the Run overload set is untouched.
	indexInto(t, e, wt, "C.cs", "class C {\n  void Run(int a) {}\n  void Run(string b) {}\n  void Other() { var x = 1; }\n}\n")
	d, _, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, dd := range d {
		if dd.QualifiedName != "" && dd.Change != "" && strings.Contains(dd.QualifiedName, "Run") {
			t.Errorf("unchanged overload set journaled %s for %s", dd.Change, dd.QualifiedName)
		}
	}
}

func TestDeltaFileRecreateReadsAsChangedNotRemoved(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	// Baseline: the file exists and is indexed.
	indexInto(t, e, wt, "back.go", "package p\nfunc Back() {}\n")

	// Branch-switch shape: the file vanishes, then comes back at the same path.
	n := e.clk.Now().UnixMilli()
	if err := e.deleteFile(ctx, wt, "back.go", stamp{at: n, seen: n, now: n}); err != nil {
		t.Fatal(err)
	}
	indexInto(t, e, wt, "back.go", "package p\nfunc Back() {}\n")

	d, _, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, dd := range d {
		got[dd.QualifiedName] = dd.Change
	}
	// The symbol existed at baseline and exists now. "removed" would be false —
	// it is sitting right there — and "added" would deny its baseline existence;
	// "changed" is the honest disposition (content may differ, we cannot tell).
	if got["Back"] != "changed" {
		t.Errorf("Back = %q, want changed (delete + recreate must not read as a removal)", got["Back"])
	}
}

func TestPruneDeltas(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() { _ = 1 }\n") // journals 'changed'
	if err := e.PruneDeltas(ctx, wt, e.clk.Now().UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
	_, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("prune left %d deltas, want 0", total)
	}
}

func TestDeltaBaselineWithCountsAPendingSession(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	clk := e.clk.(*clock.Fake)
	at := func() int64 { clk.Advance(time.Second); return clk.Now().UnixMilli() }
	write := func(id string, start int64) {
		t.Helper()
		if _, err := e.st.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at) VALUES (?, 'a', ?, ?)`, id, wt, start); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what, id string, start int64) {
		t.Helper()
		now := at()
		pb, pok, err := e.DeltaBaselineWith(ctx, wt, id, start, now)
		if err != nil {
			t.Fatal(err)
		}
		write(id, start)
		wb, wok, err := e.DeltaBaseline(ctx, wt, now)
		if err != nil || wb != pb || wok != pok || !pok {
			t.Fatalf("%s: pending %d %v, written %d %v %v", what, pb, pok, wb, wok, err)
		}
	}
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\n")
	earlier := at()
	write("earlier", earlier)
	at()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() { _ = 1 }\n")
	// The pending session is the latest start: it sets the cutoff.
	check("cutoff", "cut", at())

	// The pending session made a change and another started later: it is the baseline.
	pending := at()
	at()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() { _ = 2 }\n")
	write("later", at())
	check("candidate", "cand", pending)
}

func pruneFixture(t *testing.T, e *Engine, wt string, openB bool) {
	t.Helper()
	endedA, endedB := int64(19), int64(25)
	insert := func(id string, start int64, end *int64) {
		t.Helper()
		_, err := e.st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at, ended_at) VALUES (?, ?, ?, ?)`, id, wt, start, end)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("A", 10, &endedA)
	if openB {
		insert("B", 20, nil)
	} else {
		insert("B", 20, &endedB)
	}
	for start := int64(30); start < 40; start++ {
		end := start + 1
		insert(fmt.Sprintf("noop-%d", start), start, &end)
	}
	end := int64(51)
	insert("latest", 50, &end)
	for i, at := range []int64{5, 15, 27} {
		if _, err := e.st.DB.Exec(`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
			VALUES (?, ?, 'a.go', ?, 'function', 'changed', ?)`, fmt.Sprintf("d-%d", i), wt, fmt.Sprintf("D%d", i), at); err != nil {
			t.Fatal(err)
		}
	}
}

func deltaTimes(t *testing.T, e *Engine, wt string) []int64 {
	t.Helper()
	rows, err := e.st.DB.Query(`SELECT at FROM graph_deltas WHERE worktree=? ORDER BY at`, wt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close delta rows: %v", err)
		}
	}()
	var out []int64
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPruneUnreachableDeltasDecidesInItsTransaction(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	e := newTestEngine(t)
	pruneFixture(t, e, wt, true)
	if err := e.PruneUnreachableDeltas(ctx, wt, []Session{{ID: "current", StartedAt: 100}}, 110, func(store.Conn) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if got := deltaTimes(t, e, wt); len(got) != 3 {
		t.Fatalf("held deltas = %v, want all three", got)
	}
	before, ok, err := e.DeltaBaselineWith(ctx, wt, "current", 100, 110)
	if err != nil || !ok || before != 20 {
		t.Fatalf("open baseline = %d %v %v, want 20 true", before, ok, err)
	}
	var holdCalls int
	if err := e.PruneUnreachableDeltas(ctx, wt, []Session{{ID: "current", StartedAt: 100}}, 110, func(c store.Conn) (bool, error) {
		holdCalls++
		if holdCalls == 2 {
			_, err := c.ExecContext(ctx, `UPDATE sessions SET ended_at=25 WHERE id='B'`)
			return false, err
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if holdCalls != 2 {
		t.Fatalf("hold calls = %d, want preflight and transaction", holdCalls)
	}
	if got := deltaTimes(t, e, wt); len(got) != 2 || got[0] != 15 || got[1] != 27 {
		t.Fatalf("pruned deltas = %v, want [15 27] (the earlier baseline must remain)", got)
	}
	baseline, ok, err := e.DeltaBaseline(ctx, wt, 110)
	if err != nil || !ok || baseline != 10 {
		t.Fatalf("final baseline = %d %v %v, want A at 10", baseline, ok, err)
	}

	ref := newTestEngine(t)
	pruneFixture(t, ref, wt, false)
	tenth, err := tenthSessionStart(ctx, ref.st.DB, wt)
	if err != nil {
		t.Fatal(err)
	}
	baseline, ok, err = deltaBaselineWith(ctx, ref.st.DB, wt, "current", 100, 110)
	if err != nil {
		t.Fatal(err)
	}
	if ok && baseline < tenth {
		tenth = baseline
	}
	if err := ref.PruneDeltas(ctx, wt, tenth); err != nil {
		t.Fatal(err)
	}
	if got, want := deltaTimes(t, e, wt), deltaTimes(t, ref, wt); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unheld pruning = %v, master cutoff leaves %v", got, want)
	}
}

func TestPruneUnreachableDeltasTakesNoLockWhenMasterTookNone(t *testing.T) {
	ctx := store.WithWriteDeadline(context.Background(), time.Time{})
	lock := func(t *testing.T, e *Engine) func() {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+e.st.Path()+"?_pragma=busy_timeout(1)")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		return func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = db.Close()
		}
	}
	for _, tc := range []struct {
		name  string
		count int
		hold  bool
	}{
		{name: "fewer than ten", count: 9},
		{name: "held", count: 10, hold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			wt := t.TempDir()
			for i := range tc.count {
				if _, err := e.st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at) VALUES (?, ?, ?)`, fmt.Sprintf("s%d", i), wt, i+1); err != nil {
					t.Fatal(err)
				}
			}
			release := lock(t, e)
			defer release()
			if err := e.PruneUnreachableDeltas(ctx, wt, nil, 100, func(store.Conn) (bool, error) { return tc.hold, nil }); err != nil {
				t.Fatalf("did not skip the writer lock: %v", err)
			}
		})
	}
	e := newTestEngine(t)
	wt := t.TempDir()
	for i := range 10 {
		if _, err := e.st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at) VALUES (?, ?, ?)`, fmt.Sprintf("s%d", i), wt, i+1); err != nil {
			t.Fatal(err)
		}
	}
	release := lock(t, e)
	defer release()
	err := e.PruneUnreachableDeltas(ctx, wt, nil, 100, func(store.Conn) (bool, error) { return false, nil })
	if !store.IsBusy(err) {
		t.Fatalf("with ten sessions and no hold, error = %v, want busy", err)
	}
}

func TestPruneKeepsEveryReadersBaseline(t *testing.T) {
	t.Run("the rows give a baseline", testPruneWithRowsBaseline)
	t.Run("the rows give none", testPruneWithListBaseline)
}

func testPruneWithRowsBaseline(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wt := t.TempDir()
	pruneFixture(t, e, wt, false)
	pending := []Session{{ID: "unbackfilled-folder-session", StartedAt: 26}}
	before, ok, err := e.RecentRange(ctx, wt, pending, 110, 0)
	if err != nil || !ok || before.Since != 26 {
		t.Fatalf("folder range before the prune = %+v, %v, %v; want Since 26", before, ok, err)
	}
	if b, ok, err := e.DeltaBaseline(ctx, wt, 110); err != nil || !ok || b != 10 {
		t.Fatalf("worktree baseline before the prune = %d, %v, %v; want 10", b, ok, err)
	}
	if err := e.PruneUnreachableDeltas(ctx, wt, pending, 110, func(store.Conn) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if got, want := deltaTimes(t, e, wt), []int64{15, 27}; !slices.Equal(got, want) {
		t.Errorf("kept journal timestamps = %v; want %v", got, want)
	}
	if b, ok, err := e.DeltaBaseline(ctx, wt, 110); err != nil || !ok || b != 10 {
		t.Errorf("worktree baseline after the folder prune = %d, %v, %v; want 10", b, ok, err)
	}
	after, ok, err := e.RecentRange(ctx, wt, pending, 110, 0)
	if err != nil || !ok || after.Since != before.Since || !mapsEqual(rangeNames(after), rangeNames(before)) {
		t.Errorf("folder range after the prune = %+v, %v, %v; want Since %d and %v", after, ok, err, before.Since, rangeNames(before))
	}
}

func testPruneWithListBaseline(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wt := t.TempDir()
	for i := int64(1); i <= 10; i++ {
		start, end := i*10, i*10+1
		if _, err := e.st.DB.Exec(
			`INSERT INTO sessions(id, worktree, started_at, ended_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("s%d", start), wt, start, end); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.DB.Exec(
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('x', ?, 'a.go', 'X', 'function', 'changed', 5)`, wt); err != nil {
		t.Fatal(err)
	}
	pending := []Session{{ID: "unbackfilled-folder-session", StartedAt: 1}}
	if _, ok, err := e.DeltaBaseline(ctx, wt, 200); err != nil || ok {
		t.Fatalf("worktree baseline = %v, %v; want none", ok, err)
	}
	before, ok, err := e.RecentRange(ctx, wt, pending, 200, 0)
	if err != nil || !ok || before.Since != 1 || !mapsHave(rangeNames(before), "X") {
		t.Fatalf("folder range before the prune = %+v, %v, %v; want Since 1 with X", before, ok, err)
	}
	if err := e.PruneUnreachableDeltas(ctx, wt, pending, 200, func(store.Conn) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if got, want := deltaTimes(t, e, wt), []int64{5}; !slices.Equal(got, want) {
		t.Errorf("kept journal timestamps = %v; want %v", got, want)
	}
	after, ok, err := e.RecentRange(ctx, wt, pending, 200, 0)
	if err != nil || !ok || after.Since != 1 || !mapsEqual(rangeNames(after), rangeNames(before)) {
		t.Errorf("folder range after the prune = %+v, %v, %v; want Since 1 and %v", after, ok, err, rangeNames(before))
	}
}

// rangeNames returns a ChangeRange's changes as name → change.
func rangeNames(r ChangeRange) map[string]string {
	out := map[string]string{}
	for _, d := range r.Deltas {
		out[d.QualifiedName] = d.Change
	}
	return out
}

// --- Recent range: a change indexed after a later session started ---

// startSession writes a session of wt whose mark is the journal's mark now.
func startSession(t *testing.T, e *Engine, wt, id string, start int64) {
	t.Helper()
	mark, err := JournalMark(context.Background(), e.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB.Exec(
		`INSERT INTO sessions(id, worktree, started_at, delta_mark) VALUES (?,?,?,?)`,
		id, wt, start, mark); err != nil {
		t.Fatal(err)
	}
}

// endSession ends a session written by startSession or addSession.
func endSession(t *testing.T, e *Engine, id string, end int64) {
	t.Helper()
	if _, err := e.st.DB.Exec(`UPDATE sessions SET ended_at=? WHERE id=?`, end, id); err != nil {
		t.Fatal(err)
	}
}

// journalDeltasAt journals added rows for names at at, as a credited write does.
func journalDeltasAt(t *testing.T, e *Engine, wt, path string, at int64, names ...string) {
	t.Helper()
	err := e.st.WriteTx(context.Background(), func(c store.Conn) error {
		rows := make([]deltaRow, len(names))
		for i, n := range names {
			rows[i] = deltaRow{qn: n, kind: "function", change: "added"}
		}
		return insertDeltas(context.Background(), c, wt, path, rows, stamp{at: at, seen: at, now: at})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rangeFixture drives one worktree's sessions and journal with explicit times.
type rangeFixture struct {
	t   *testing.T
	e   *Engine
	wt  string
	now int64
}

func newRangeFixture(t *testing.T) *rangeFixture {
	t.Helper()
	return &rangeFixture{t: t, e: newTestEngine(t), wt: t.TempDir(), now: 1_000}
}

func (f *rangeFixture) tick() int64 {
	f.now += 1_000
	return f.now
}

// start writes session id starting now and returns its start.
func (f *rangeFixture) start(id string) int64 {
	at := f.tick()
	startSession(f.t, f.e, f.wt, id, at)
	return at
}

// end ends session id now.
func (f *rangeFixture) end(id string) {
	endSession(f.t, f.e, id, f.tick())
}

func (f *rangeFixture) journal(path string, at int64, names ...string) {
	journalDeltasAt(f.t, f.e, f.wt, path, at, names...)
}

func (f *rangeFixture) mark() sql.NullInt64 {
	mark, err := JournalMark(context.Background(), f.e.st.DB)
	if err != nil {
		f.t.Fatal(err)
	}
	return sql.NullInt64{Int64: mark, Valid: true}
}

// block returns the range of a block built now as name → change.
func (f *rangeFixture) block(pending ...Session) map[string]string {
	_, got := f.rng(pending...)
	return got
}

// rng returns the range and its names → changes.
func (f *rangeFixture) rng(pending ...Session) (ChangeRange, map[string]string) {
	f.t.Helper()
	r, _, err := f.e.RecentRange(context.Background(), f.wt, pending, f.e.clk.Now().UnixMilli(), 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return r, rangeNames(r)
}

// recordRead raises session id's mark to read, as a block's own write does.
func (f *rangeFixture) recordRead(id string, read int64) {
	f.t.Helper()
	if _, err := f.e.st.DB.Exec(
		`UPDATE sessions SET delta_mark = MAX(COALESCE(delta_mark, -1), ?) WHERE id = ?`, read, id); err != nil {
		f.t.Fatal(err)
	}
}

func TestJournalSeqOnlyGrows(t *testing.T) {
	e := newTestEngine(t)
	wt := t.TempDir()
	ctx := context.Background()
	journalDeltasAt(t, e, wt, "a.go", 1000, "First")
	m, err := JournalMark(ctx, e.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.PruneDeltas(ctx, wt, 1<<62); err != nil {
		t.Fatal(err)
	}
	journalDeltasAt(t, e, wt, "a.go", 2000, "Second")
	var seq int64
	if err := e.st.DB.QueryRow(
		`SELECT seq FROM graph_deltas WHERE worktree=? ORDER BY seq DESC LIMIT 1`, wt).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	after, err := JournalMark(ctx, e.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if seq <= m || after <= m {
		t.Errorf("after pruning and a new write: seq = %d, mark = %d; want both > %d", seq, after, m)
	}
}

func TestALateChangeIsInOneRange(t *testing.T) {
	t.Run("late after S2's start", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		f.journal("x.go", s1, "X")
		s2 := f.tick()
		f.journal("y.go", s2, "Y")
		f.end("s2")
		f.start("s3")
		if got, want := f.block(), map[string]string{"X": "added", "Y": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's range = %v; want %v", got, want)
		}
		s3 := f.now
		f.journal("z.go", s3, "Z")
		f.end("s3")
		f.start("s4")
		if got, want := f.block(), map[string]string{"Z": "added"}; !mapsEqual(got, want) {
			t.Errorf("S4's range = %v; want %v", got, want)
		}
	})
	t.Run("indexed before S2's start", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.journal("x.go", s1, "X")
		f.end("s1")
		f.start("s2")
		s2 := f.tick()
		f.journal("y.go", s2, "Y")
		f.end("s2")
		f.start("s3")
		if got, want := f.block(), map[string]string{"Y": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's range = %v; want %v", got, want)
		}
	})
	t.Run("S2 changes nothing", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		f.journal("x.go", s1, "X")
		f.end("s2")
		f.start("s3")
		if got := f.block(); !mapsHave(got, "X") {
			t.Errorf("S3's range = %v; want X", got)
		}
	})
	t.Run("overlapping starts", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		s2 := f.start("s2")
		f.journal("x.go", s1, "X")
		f.journal("y.go", s2, "Y")
		f.end("s2")
		f.start("s3")
		f.start("s4")
		r3, b3 := f.rng()
		r4, b4 := f.rng()
		if !mapsHave(b3, "X") || !mapsHave(b4, "X") {
			t.Fatalf("a block omitted X: S3 = %v; S4 = %v", b3, b4)
		}
		f.recordRead("s3", r3.Read)
		f.recordRead("s4", r4.Read)
		if got := f.block(); !mapsHave(got, "X") {
			t.Errorf("after both reads were recorded, the range = %v; want X", got)
		}
	})
	t.Run("a start that never read its range", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		s2 := f.start("s2")
		f.journal("x.go", s1, "X")
		f.journal("y.go", s2, "Y")
		f.end("s2")
		f.start("s3") // its row saves a mark that covers X; it never reads
		f.start("s4")
		if got := f.block(); !mapsHave(got, "X") {
			t.Errorf("S4's range = %v; want X", got)
		}
	})
	t.Run("the baseline is its own block's session", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		s2 := f.start("s2")
		f.journal("y.go", s2, "Y")
		f.end("s2")
		f.start("s3")
		f.end("s3")
		f.journal("x.go", s1, "X")
		r, got := f.rng()
		if r.Base != "s2" || !mapsHave(got, "X") || !mapsHave(got, "Y") {
			t.Fatalf("range = %+v, %v; want Base s2 with X and Y", r, got)
		}
		f.recordRead("s2", r.Read)
		if got := f.block(); mapsHave(got, "X") || !mapsHave(got, "Y") {
			t.Errorf("after raising the baseline's own mark, the range = %v; want Y only", got)
		}
	})
	t.Run("a session pending in a record", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		f.end("s2")
		f.journal("a.go", s1, "Extra")
		s3 := f.start("s3")
		f.end("s3")
		if _, err := f.e.st.DB.Exec(`DELETE FROM sessions WHERE id='s3'`); err != nil {
			t.Fatal(err)
		}
		pending := Session{ID: "s3", StartedAt: s3, EndedAt: sql.NullInt64{Int64: f.now, Valid: true}, Mark: f.mark()}
		f.journal("b.go", s3, "Z")
		f.start("s4")
		if got, want := f.block(pending), map[string]string{"Z": "added"}; !mapsEqual(got, want) {
			t.Errorf("S4's range = %v; want %v", got, want)
		}
	})
	t.Run("a pending end over an open row with a lower mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		s2 := f.start("s2")
		f.journal("a.go", s2, "Y")
		f.end("s2")
		s3 := f.start("s3")
		f.journal("x.go", s1, "X")
		s3span := f.tick()
		f.journal("z.go", s3span, "Z")
		f.start("s4")
		pending := Session{ID: "s3", StartedAt: s3, EndedAt: sql.NullInt64{Int64: f.now, Valid: true}, Mark: f.mark()}
		if got, want := f.block(pending), map[string]string{"Z": "added"}; !mapsEqual(got, want) {
			t.Errorf("with the pending end, S4's range = %v; want %v", got, want)
		}
		if got := f.block(); !mapsHave(got, "X") || !mapsHave(got, "Z") {
			t.Errorf("without the merge, S4's range = %v; want X and Z", got)
		}
		if _, err := f.e.st.DB.Exec(
			`UPDATE sessions SET ended_at=?, delta_mark=MAX(COALESCE(delta_mark,-1),?) WHERE id='s3'`,
			pending.EndedAt.Int64, pending.Mark.Int64); err != nil {
			t.Fatal(err)
		}
		if got, want := f.block(), map[string]string{"Z": "added"}; !mapsEqual(got, want) {
			t.Errorf("after recovery, S4's range = %v; want %v", got, want)
		}
	})
	t.Run("an ended row takes an entry's EndedMark, not its Mark", testEndedRowTakesEndedMark)
	t.Run("an entry over an ended row leaves its end", testEntryLeavesEndedRowsEnd)
	t.Run("an entry whose row is in another worktree is not counted", testOtherWorktreeEntry)
	t.Run("a baseline session without a mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.tick()
		addSession(t, f.e, f.wt, "s1", s1, f.tick())
		s2 := f.tick()
		addSession(t, f.e, f.wt, "s2", s2, f.tick())
		f.journal("y.go", s2, "Y")
		f.journal("x.go", s1, "X")
		f.start("s3")
		r, got := f.rng()
		want := changesSince(t, f.e, f.wt, r.Since)
		if !mapsEqual(got, want) || mapsHave(got, "X") || !mapsHave(got, "Y") {
			t.Errorf("S3's range = %v; want DeltasSince(%d) = %v", got, r.Since, want)
		}
	})
}

func testEndedRowTakesEndedMark(t *testing.T) {
	f := newRangeFixture(t)
	s1 := f.start("s1")
	f.end("s1")
	s2 := f.start("s2")
	f.journal("y.go", s2, "Y")
	f.end("s2")
	f.journal("x.go", s1, "X")
	f.start("s3")
	m := f.mark()
	if got := f.block(Session{ID: "s2", StartedAt: s2, Mark: m}); !mapsHave(got, "X") || !mapsHave(got, "Y") {
		t.Errorf("an entry's Mark reached an ended row: %v; want X and Y", got)
	}
	if got := f.block(Session{ID: "s2", StartedAt: s2, EndedMark: m}); mapsHave(got, "X") || !mapsHave(got, "Y") {
		t.Errorf("an entry's EndedMark did not reach an ended row: %v; want Y only", got)
	}
}

func testEntryLeavesEndedRowsEnd(t *testing.T) {
	f := newRangeFixture(t)
	s1 := f.start("s1")
	f.journal("a.go", s1, "A")
	f.end("s1")
	s2 := f.start("s2")
	f.end("s2")
	f.journal("w.go", f.tick(), "W") // after S2's end, in no session
	f.start("s3")
	entry := Session{ID: "s2", StartedAt: s2, EndedAt: sql.NullInt64{Int64: f.now, Valid: true}}
	r, got := f.rng(entry)
	if r.Since != s1 || !mapsHave(got, "A") || !mapsHave(got, "W") {
		t.Errorf("range = %+v, %v; want Since S1's start with A and W", r, got)
	}
}

func testOtherWorktreeEntry(t *testing.T) {
	f := newRangeFixture(t)
	s1 := f.start("s1")
	f.journal("a.go", s1, "A")
	f.end("s1")
	s2 := f.start("s2")
	f.journal("b.go", s2, "B")
	f.end("s2")
	s9 := f.tick()
	startSession(t, f.e, t.TempDir(), "s9", s9)
	f.journal("c.go", f.tick(), "C")
	f.start("s3")
	rWith, gotWith := f.rng(Session{ID: "s9", StartedAt: s9})
	rWithout, gotWithout := f.rng()
	if rWith.Since != rWithout.Since || !mapsEqual(gotWith, gotWithout) {
		t.Errorf("a row in another worktree changed the range: %+v %v vs %+v %v", rWith, gotWith, rWithout, gotWithout)
	}
	if rWithout.Since != s2 || !mapsHave(gotWithout, "B") || !mapsHave(gotWithout, "C") {
		t.Errorf("range = %+v, %v; want Since S2's start with B and C", rWithout, gotWithout)
	}
	rNoRow, gotNoRow := f.rng(Session{ID: "s10", StartedAt: s9})
	if rNoRow.Since != s9 || !mapsEqual(gotNoRow, map[string]string{"C": "added"}) {
		t.Errorf("a no-row entry at %d gave %+v, %v; want Since it with C", s9, rNoRow, gotNoRow)
	}
}

func TestRangeIsReadInOneSnapshot(t *testing.T) {
	f := newRangeFixture(t)
	s1 := f.start("s1")
	f.journal("old.go", s1, "A")
	f.end("s1")
	s2 := f.start("s2")
	f.journal("x.go", s2, "Y")
	f.end("s2")
	f.start("s3")
	_, before := f.rng()
	if want := map[string]string{"Y": "added"}; !mapsEqual(before, want) {
		t.Fatalf("before the commit = %v; want %v", before, want)
	}
	m := f.mark()
	old := afterRangeBaseline
	afterRangeBaseline = func() {
		ctx := context.Background()
		now := f.tick()
		if err := f.e.st.WriteTx(ctx, func(c store.Conn) error {
			return insertDeltas(ctx, c, f.wt, "x.go", []deltaRow{{qn: "X", kind: "function", change: "added"}},
				stamp{at: s1, seen: now, now: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterRangeBaseline = old })
	r, got := f.rng()
	if !mapsEqual(got, before) || r.Read != m.Int64 {
		t.Errorf("range = %v (Read %d); want the same snapshot %v (Read %d)", got, r.Read, before, m.Int64)
	}
	afterRangeBaseline = old
	if after := f.block(); !mapsEqual(after, map[string]string{"A": "added", "X": "added", "Y": "added"}) {
		t.Errorf("after the commit = %v; want A, X and Y", after)
	}
}

// mapsEqual reports whether a and b hold the same names and changes.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// mapsHave reports whether m holds name.
func mapsHave(m map[string]string, name string) bool {
	_, ok := m[name]
	return ok
}
