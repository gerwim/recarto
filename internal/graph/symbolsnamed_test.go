package graph

import (
	"context"
	"fmt"
	"testing"
)

func TestIsFixturePath(t *testing.T) {
	for p, want := range map[string]bool{
		"testdata/x.go":         true,
		"a/fixtures/b.ts":       true,
		"testing/x.go":          false,
		"src/fixture_core/a.py": false,
	} {
		if got := isFixturePath(p); got != want {
			t.Errorf("isFixturePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func indexGo(t *testing.T, e *Engine, path, src string) {
	t.Helper()
	b := []byte(src)
	if err := e.IndexFile(context.Background(), "/wt", path, b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolsNamed(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	indexGo(t, e, "x.pb.go", "package p\nfunc Run() {}\n")
	indexGo(t, e, "testdata/f.go", "package p\nfunc Run() {}\n")
	indexGo(t, e, "a_test.go", "package p\nfunc Run() {}\n")
	indexGo(t, e, "a.go", "package p\ntype T struct{}\nfunc Run() {}\nfunc RunAll() {}\nfunc run() {}\nfunc (T) Run() {}\n")

	got, err := e.SymbolsNamed(ctx, "/wt", []string{"Run"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, r := range got {
		seq = append(seq, r.QualifiedName+"@"+r.Path)
	}
	want := []string{"Run@a.go", "T.Run@a.go", "Run@a_test.go", "Run@testdata/f.go", "Run@x.pb.go"}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Fatalf("SymbolsNamed(Run) = %v, want %v", seq, want)
	}

	got, err = e.SymbolsNamed(ctx, "/wt", []string{"T.Run"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].QualifiedName != "T.Run" {
		t.Fatalf("SymbolsNamed(T.Run) = %+v, want only the method", got)
	}

	if got, _ = e.SymbolsNamed(ctx, "/wt", []string{"Run"}, 2); len(got) != 2 {
		t.Fatalf("limit 2 gave %d rows", len(got))
	}
}

// More test and fixture rows sort before the product row by path than any
// plausible SQL row cap, so ranking must happen over every match.
func TestSymbolsNamedProductFirstWhateverThePath(t *testing.T) {
	e := newTestEngine(t)
	for i := range 1000 {
		indexGo(t, e, fmt.Sprintf("aaa/testdata/f%04d.go", i), "package p\nfunc Run() {}\n")
	}
	indexGo(t, e, "aaa/x_test.go", "package p\nfunc Run() {}\n")
	indexGo(t, e, "zzz/product.go", "package p\nfunc Run() {}\n")
	got, err := e.SymbolsNamed(context.Background(), "/wt", []string{"Run"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "zzz/product.go" {
		t.Fatalf("SymbolsNamed(Run, 1) = %+v, want zzz/product.go", got)
	}
}
