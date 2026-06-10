package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQueryOutputsStableAcrossIndexOrder pins that search, impls, query and
// trace return IDENTICAL output regardless of the order files were indexed in.
// Node/edge ids are UUIDv7 — effectively insertion-ordered — and on a real repo
// the parallel parse workers race, so any read path that orders or tiebreaks on
// a raw id (or relies on implicit FTS/rowid order among equal scores) returns
// permuted results after every reindex. For an LLM consumer that reads as
// changed results for an unchanged repo; stable keys (path, position) must
// decide instead.
//
// The fixture indexes the same worktree into two stores, once in sorted order
// and once reversed, flipping id assignment order; every rendered output must
// match. The same-named class in two files (a C# partial class) is the
// representative-pick case that flips most visibly.
func TestQueryOutputsStableAcrossIndexOrder(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	files := map[string]string{
		"ib.cs":   "interface IBar { }\n",
		"one.cs":  "class Foo : IBar { public void M() { Helper(); } }\n",
		"two.cs":  "class Foo : IBar { public void N() { Helper(); } }\n",
		"help.cs": "class Zed { public void Helper() { } }\n",
	}
	var order []string
	for p, c := range files {
		writeFile(t, wt, p, c)
		order = append(order, p)
	}

	render := func(e *Engine) []string {
		var out []string
		res, err := e.Search(ctx, wt, "Foo", "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			out = append(out, fmt.Sprintf("search %s %s:%d", r.QualifiedName, r.Path, r.Line))
		}
		impls, err := e.Implementations(ctx, wt, "IBar", "down", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range impls {
			out = append(out, fmt.Sprintf("impls %s %s:%d", r.QualifiedName, r.Path, r.Line))
		}
		qres, err := e.Query(ctx, wt, QuerySpec{Kind: "class"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range qres {
			out = append(out, fmt.Sprintf("query %s %s:%d", r.QualifiedName, r.Path, r.Line))
		}
		chains, err := e.TraceCallPath(ctx, wt, "Helper", "callers", 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range chains {
			var hops []string
			for _, n := range ch {
				hops = append(hops, fmt.Sprintf("%s(%s:%d)", n.Name, n.Path, n.Line))
			}
			out = append(out, "trace "+strings.Join(hops, "->"))
		}
		return out
	}

	index := func(paths []string) []string {
		e := newTestEngine(t)
		for _, p := range paths {
			content, err := os.ReadFile(filepath.Join(wt, p))
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(wt, p))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.IndexFile(ctx, wt, p, content, info.ModTime().UnixMilli(), info.Size()); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Resolve(ctx, wt); err != nil {
			t.Fatal(err)
		}
		return render(e)
	}

	sortedOrder := append([]string{}, order...)
	for i, j := 0, len(sortedOrder)-1; i < j; i, j = i+1, j-1 {
		sortedOrder[i], sortedOrder[j] = sortedOrder[j], sortedOrder[i]
	}
	a := index(order)
	b := index(sortedOrder)

	if len(a) != len(b) {
		t.Fatalf("output lengths differ: %d vs %d\nA:\n%s\nB:\n%s",
			len(a), len(b), strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("line %d differs across index order:\n  A: %s\n  B: %s", i, a[i], b[i])
		}
	}
}
