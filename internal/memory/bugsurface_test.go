package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestBugsForCodeMatchesByFileRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "nil deref", Body: "nil deref in parser — fix: guard",
		Refs:   []Ref{{Type: RefFile, Value: "internal/parse/parse.go"}},
		Branch: "main", Source: "cli", Extra: map[string]any{"fix": "guard"},
	}); err != nil {
		t.Fatal(err)
	}
	// unrelated bug on another file must not match
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "other", Body: "an unrelated bug elsewhere",
		Refs: []Ref{{Type: RefFile, Value: "internal/store/store.go"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	// a learning on the SAME file must not be returned (bugs only)
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "note", Body: "a note about the parser file",
		Refs: []Ref{{Type: RefFile, Value: "internal/parse/parse.go"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.BugsForCode(ctx, "main", []string{"internal/parse/parse.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "nil deref" {
		t.Fatalf("want exactly 1 bug 'nil deref', got %d: %+v", len(bugs), bugs)
	}
}

func TestBugsForCodeMatchesBySymbolRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "off by one", Body: "off by one in Tokenize loop",
		Refs: []Ref{{Type: RefSymbol, Value: "Tokenize"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "off by one" {
		t.Fatalf("want 1 bug 'off by one', got %d: %+v", len(bugs), bugs)
	}
}

func TestRecentBugsOrderedByRecency(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "older", Body: "older bug body alpha", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "newer", Body: "newer bug body beta", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.RecentBugs(ctx, "main", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 2 || bugs[0].Title != "newer" {
		t.Fatalf("want 2 bugs, newer first; got %d: %+v", len(bugs), bugs)
	}
}
