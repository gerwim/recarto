package graph

import (
	"context"
	"strings"
	"testing"
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
	if err := e.deleteFile(ctx, wt, "gone.go"); err != nil {
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
