package graph

import (
	"context"
	"testing"
)

func TestOutlineListsFileSymbolsInSourceOrder(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// Other() is defined before Target() in the source; outline must preserve
	// that order so an agent can map line numbers without reading the file.
	content := "package p\n\nfunc Other() {}\n\nfunc Target() int {\n\treturn 42\n}\n"
	writeFile(t, wt, "t.go", content)
	src, _ := FSSource{Root: wt}.Read("t.go")
	if err := e.IndexFile(ctx, wt, "t.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	syms, err := e.Outline(ctx, wt, "t.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) < 2 {
		t.Fatalf("outline returned %d symbols, want >= 2: %+v", len(syms), syms)
	}
	var names []string
	for _, s := range syms {
		names = append(names, s.Name)
		if s.StartLine == 0 {
			t.Errorf("symbol %q missing start line", s.Name)
		}
	}
	// Source order: Other (line 3) precedes Target (line 5).
	iOther, iTarget := indexOf(names, "Other"), indexOf(names, "Target")
	if iOther < 0 || iTarget < 0 {
		t.Fatalf("expected both Other and Target in outline, got %v", names)
	}
	if iOther > iTarget {
		t.Errorf("outline not in source order: %v", names)
	}
}

func TestOutlineEmptyForUnknownFile(t *testing.T) {
	e := newTestEngine(t)
	syms, err := e.Outline(context.Background(), t.TempDir(), "nope.go")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("expected no symbols for unknown file, got %+v", syms)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
