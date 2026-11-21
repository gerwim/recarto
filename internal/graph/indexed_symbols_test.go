package graph

import (
	"context"
	"testing"
)

func TestIndexedSymbolsNamesAndWorktree(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	indexGo(t, e, "a.go", "package p\nfunc Warm() {}\ntype T struct{}\nfunc (T) Run() {}\n")
	if err := e.IndexFile(ctx, "/other", "other.go", []byte("package p\nfunc Elsewhere() {}\n"), 1, 28); err != nil {
		t.Fatal(err)
	}
	built, has, err := e.IndexedSymbols(ctx, "/wt", true, []string{"Warm", "T.Run", "NoSuch", "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if !built || len(has) != 4 || !has[0] || !has[1] || has[2] || has[3] {
		t.Fatalf("IndexedSymbols = built %v, has %v; want true [true true false false]", built, has)
	}
}

func TestIndexedSymbolsMatchesNamesLiterally(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// The namespace makes T.Method exercise the qualified-name suffix form.
	src := []byte("namespace Ns { class T { void DoXRun() {} void run() {} } }")
	if err := e.IndexFile(ctx, "/wt", "Methods.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	built, has, err := e.IndexedSymbols(ctx, "/wt", true, []string{"T.DoXRun", "T.Do_Run", "T.Run", "T.%"})
	if err != nil || !built || len(has) != 4 || !has[0] || has[1] || has[2] || has[3] {
		t.Fatalf("IndexedSymbols = built %v, has %v, err %v; want true [true false false false]", built, has, err)
	}
}

func TestIndexedSymbolsUsesIndexAsItStandsAcrossInterruptedRefresh(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := &mutMemSource{files: map[string]string{
		"a.go": "package p\nfunc Warm() {}\n",
		"b.go": "package p\nfunc Other() {}\n",
	}}
	first, cancelFirst := context.WithCancel(ctx)
	if err := e.ReconcileIfStale(first, "/wt", src, "tok-1", WithProgress(cancelOnResolvePhase{cancelFirst})); err == nil {
		t.Fatal("first index was not interrupted")
	}
	built, has, err := e.IndexedSymbols(ctx, "/wt", false, []string{"Warm", "Other", "NoSuch"})
	if err != nil || built || has != nil {
		t.Fatalf("first partial index = built %v, has %v, err %v; want false,nil", built, has, err)
	}
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	built, has, err = e.IndexedSymbols(ctx, "/wt", false, []string{"Warm", "Other", "NoSuch"})
	if err != nil || !built || len(has) != 3 || !has[0] || !has[1] || has[2] {
		t.Fatalf("finished index = built %v, has %v, err %v", built, has, err)
	}

	src.files["a.go"] = "package p\nfunc Colder() { /* longer changed source */ }\n"
	src.files["c.go"] = "package p\nfunc Fresh() {}\n"
	refresh, cancelRefresh := context.WithCancel(ctx)
	if err := e.ReconcileIfStale(refresh, "/wt", src, "tok-2", WithProgress(cancelOnResolvePhase{cancelRefresh})); err == nil {
		t.Fatal("refresh was not interrupted")
	}
	built, has, err = e.IndexedSymbols(ctx, "/wt", false, []string{"Warm", "Other", "Fresh", "NoSuch"})
	if err != nil || !built || len(has) != 4 || has[0] || !has[1] || !has[2] || has[3] {
		t.Fatalf("interrupted refresh index = built %v, has %v, err %v; want true [false true true false]", built, has, err)
	}
}
