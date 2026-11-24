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
