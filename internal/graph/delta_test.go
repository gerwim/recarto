package graph

import (
	"context"
	"database/sql"
	"fmt"
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
	if err := e.PruneUnreachableDeltas(ctx, wt, "current", 100, 110, func(store.Conn) (bool, error) { return true, nil }); err != nil {
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
	if err := e.PruneUnreachableDeltas(ctx, wt, "current", 100, 110, func(c store.Conn) (bool, error) {
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
			if err := e.PruneUnreachableDeltas(ctx, wt, "", 0, 100, func(store.Conn) (bool, error) { return tc.hold, nil }); err != nil {
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
	err := e.PruneUnreachableDeltas(ctx, wt, "", 0, 100, func(store.Conn) (bool, error) { return false, nil })
	if !store.IsBusy(err) {
		t.Fatalf("with ten sessions and no hold, error = %v, want busy", err)
	}
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

// inRange returns RecentRange + DeltasIn as name → change.
func inRange(t *testing.T, e *Engine, wt string, of RangeOf) map[string]string {
	t.Helper()
	r, ok, err := e.RecentRange(context.Background(), wt, of, e.clk.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if !ok {
		return out
	}
	ds, _, err := e.DeltasIn(context.Background(), wt, r, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		out[d.QualifiedName] = d.Change
	}
	return out
}

// journalMark reads e's journal mark as a block start would.
func journalMark(t *testing.T, e *Engine) sql.NullInt64 {
	t.Helper()
	mark, err := JournalMark(context.Background(), e.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	return sql.NullInt64{Int64: mark, Valid: true}
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

func (f *rangeFixture) mark() sql.NullInt64 { return journalMark(f.t, f.e) }

func (f *rangeFixture) block(upto sql.NullInt64, pending ...Session) map[string]string {
	return inRange(f.t, f.e, f.wt, RangeOf{Pending: pending, Upto: upto})
}

func (f *rangeFixture) live(recorded map[string]int64) map[string]string {
	return inRange(f.t, f.e, f.wt, RangeOf{Recorded: recorded})
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
		f.journal("a.go", s1, "Extra")
		s2 := f.tick() // S2's own window
		f.journal("b.go", s2, "Other")
		f.end("s2")
		f.start("s3")
		if got, want := f.block(f.mark(), Session{ID: "s3", StartedAt: f.now}), map[string]string{
			"Extra": "added", "Other": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's block = %v; want %v", got, want)
		}
		s3 := f.now
		f.journal("c.go", s3, "Later")
		f.end("s3")
		f.start("s4")
		s4 := f.now
		f.journal("d.go", s4, "S4s")
		f.end("s4")
		f.start("s5")
		if got := f.block(f.mark()); mapsHave(got, "Extra") {
			t.Errorf("S5's block = %v; want no Extra", got)
		}
	})
	t.Run("indexed before S2's start", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.journal("a.go", s1, "Extra")
		f.end("s1")
		f.start("s2")
		s2 := f.tick()
		f.journal("b.go", s2, "Other")
		f.end("s2")
		f.start("s3")
		if got, want := f.block(f.mark()), map[string]string{"Other": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's block = %v; want %v", got, want)
		}
	})
	t.Run("written while S3 builds its block", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.journal("a.go", s1, "A")
		f.end("s1")
		f.start("s2")
		f.end("s2")
		s3 := f.start("s3")
		upto := f.mark()
		f.journal("b.go", s1, "Extra")
		f.journal("c.go", s3, "InS3")
		if got := f.block(upto); mapsHave(got, "Extra") {
			t.Errorf("S3's block as of its mark = %v; want no Extra", got)
		}
		if got := f.live(nil); !mapsHave(got, "Extra") {
			t.Errorf("S3's live range = %v; want Extra", got)
		}
		f.end("s3")
		s4 := f.start("s4")
		if got := f.block(f.mark()); !mapsHave(got, "Extra") {
			t.Errorf("S4's block = %v; want Extra", got)
		}
		f.journal("d.go", s4, "InS4")
		f.end("s4")
		f.start("s5")
		if got := f.block(f.mark()); mapsHave(got, "Extra") {
			t.Errorf("S5's block = %v; want no Extra", got)
		}
	})
	t.Run("S2's change recorded while S3 builds its block", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.journal("a.go", s1, "A")
		f.end("s1")
		s2 := f.start("s2")
		f.end("s2")
		s3 := f.start("s3")
		upto := f.mark()
		f.journal("b.go", s2, "B")
		if got, want := f.block(upto), map[string]string{"A": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's block as of its mark = %v; want %v", got, want)
		}
		if got, want := f.live(nil), map[string]string{"B": "added"}; !mapsEqual(got, want) {
			t.Errorf("S3's live range = %v; want %v", got, want)
		}
		f.journal("c.go", s3, "C")
		f.end("s3")
		f.start("s4")
		if got, want := f.block(f.mark()), map[string]string{"B": "added", "C": "added"}; !mapsEqual(got, want) {
			t.Errorf("S4's block = %v; want %v", got, want)
		}
	})
	t.Run("an older session's new mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		s2 := f.tick()
		f.journal("a.go", s2, "Other")
		f.end("s2")
		f.start("s3")
		f.end("s3")
		f.journal("b.go", s1, "Extra")
		raised := f.mark()
		if _, err := f.e.st.DB.Exec(`UPDATE sessions SET delta_mark=? WHERE id='s2'`, raised.Int64); err != nil {
			t.Fatal(err)
		}
		if got := f.block(raised); !mapsHave(got, "Extra") {
			t.Errorf("S2's block with the raised mark = %v; want Extra", got)
		}
		if got := f.live(nil); !mapsHave(got, "Extra") {
			t.Errorf("the live range = %v; want Extra", got)
		}
		f.start("s4")
		if got := f.block(f.mark()); mapsHave(got, "Extra") {
			t.Errorf("S4's block = %v; want no Extra", got)
		}
	})
	t.Run("the last block before its raise lands", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		s2 := f.tick()
		f.journal("a.go", s2, "Other")
		f.end("s2")
		f.start("s3")
		f.end("s3")
		f.journal("b.go", s1, "Extra")
		if got := f.block(f.mark()); !mapsHave(got, "Extra") {
			t.Errorf("S2's block = %v; want Extra", got)
		}
		if got := f.live(nil); !mapsHave(got, "Extra") {
			t.Errorf("the live range = %v; want Extra", got)
		}
	})
	t.Run("a recorded mark over a lower row mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		s2 := f.tick()
		f.journal("a.go", s1, "X")
		f.journal("b.go", s2, "Y")
		f.start("s3")
		s3 := f.tick()
		f.journal("c.go", s3, "Z")
		f.end("s3")
		recorded := map[string]int64{"s2": 3}
		want := map[string]string{"Y": "added", "Z": "added"}
		if got := inRange(t, f.e, f.wt, RangeOf{Recorded: recorded, Upto: sql.NullInt64{Int64: 3, Valid: true}}); !mapsEqual(got, want) {
			t.Errorf("S2's block with a low row = %v; want %v", got, want)
		}
		if got := f.block(f.mark()); !mapsHave(got, "X") {
			t.Errorf("the range without the recorded mark = %v; want X", got)
		}
		if got := f.live(recorded); !mapsEqual(got, want) {
			t.Errorf("the live range with the recorded mark = %v; want %v", got, want)
		}
		if _, err := f.e.st.DB.Exec(`UPDATE sessions SET delta_mark=3 WHERE id='s2'`); err != nil {
			t.Fatal(err)
		}
		if got := inRange(t, f.e, f.wt, RangeOf{Recorded: recorded, Upto: sql.NullInt64{Int64: 3, Valid: true}}); !mapsEqual(got, want) {
			t.Errorf("S2's block with a raised row = %v; want %v", got, want)
		}
		if got := f.live(recorded); !mapsEqual(got, want) {
			t.Errorf("the live range after the raise = %v; want %v", got, want)
		}
	})
	t.Run("a session pending in a record", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		f.start("s2")
		f.end("s2")
		f.journal("a.go", s1, "Extra")
		s3 := f.start("s3") // no row: its end waits in a record
		f.end("s3")
		if _, err := f.e.st.DB.Exec(`DELETE FROM sessions WHERE id='s3'`); err != nil {
			t.Fatal(err)
		}
		pending := Session{ID: "s3", StartedAt: s3, EndedAt: sql.NullInt64{Int64: f.now, Valid: true}, Mark: f.mark()}
		f.journal("b.go", s3, "Z")
		f.start("s4")
		if got, want := f.block(f.mark(), pending), map[string]string{"Z": "added"}; !mapsEqual(got, want) {
			t.Errorf("S4's block = %v; want %v", got, want)
		}
	})
	t.Run("S2 without a mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.start("s1")
		f.end("s1")
		s2 := f.tick()
		addSession(t, f.e, f.wt, "s2", s2, f.tick())
		f.journal("a.go", s1, "Extra")
		f.start("s3")
		if got := f.block(f.mark()); !mapsHave(got, "Extra") {
			t.Errorf("S3's block = %v; want Extra", got)
		}
	})
	t.Run("no earlier session has a mark", func(t *testing.T) {
		f := newRangeFixture(t)
		s1 := f.tick()
		addSession(t, f.e, f.wt, "s1", s1, f.tick())
		s2 := f.tick()
		addSession(t, f.e, f.wt, "s2", s2, f.tick())
		f.journal("b.go", s2, "Normal")
		f.journal("a.go", s1, "Extra")
		f.start("s3")
		upto := f.mark()
		r, ok, err := f.e.RecentRange(context.Background(), f.wt, RangeOf{Upto: upto}, f.e.clk.Now().UnixMilli())
		if err != nil || !ok {
			t.Fatalf("RecentRange = %v, %v", ok, err)
		}
		got := f.block(upto)
		want := changesSince(t, f.e, f.wt, r.Since)
		if !mapsEqual(got, want) || mapsHave(got, "Extra") {
			t.Errorf("S3's range = %v; want DeltasSince(%d) = %v without Extra", got, r.Since, want)
		}
	})
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
