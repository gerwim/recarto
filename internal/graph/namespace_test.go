package graph

import (
	"context"
	"testing"
)

// TestDemoteNamespacesStablePartition: namespace container nodes sink below
// concrete symbols, each group keeping its incoming order, nothing dropped.
func TestDemoteNamespacesStablePartition(t *testing.T) {
	in := []SearchResult{
		{Name: "App.Exec", Kind: "namespace"},
		{Name: "Runner", Kind: "class"},
		{Name: "App.Other", Kind: "namespace"},
		{Name: "Gate", Kind: "method"},
	}
	got := demoteNamespaces(in)
	want := []string{"Runner", "Gate", "App.Exec", "App.Other"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("demoteNamespaces order = %v, want %v", names(got), want)
		}
	}
	if len(got) != len(in) {
		t.Fatalf("demoteNamespaces dropped results: %d != %d", len(got), len(in))
	}
}

func TestDemoteNamespacesNoNamespaceIsIdentity(t *testing.T) {
	in := []SearchResult{{Name: "A", Kind: "class"}, {Name: "B", Kind: "method"}}
	got := demoteNamespaces(in)
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Fatalf("demoteNamespaces should be a no-op without namespaces, got %v", names(got))
	}
}

// TestSymbolKeyCollapsesNamespacesAcrossFiles: the same namespace declared in
// many files is one logical symbol, so its per-file declarations must share a
// dedup key (unlike concrete same-named symbols, which the path keeps distinct).
func TestSymbolKeyCollapsesNamespacesAcrossFiles(t *testing.T) {
	a := SearchResult{Name: "App.Exec", QualifiedName: "App.Exec", Kind: "namespace", Path: "src/A.cs"}
	b := SearchResult{Name: "App.Exec", QualifiedName: "App.Exec", Kind: "namespace", Path: "src/B.cs"}
	if symbolKey(a) != symbolKey(b) {
		t.Fatalf("namespace declarations in different files should share a key: %q vs %q",
			symbolKey(a), symbolKey(b))
	}
	// A concrete symbol with the same name in two files must stay distinct.
	c := SearchResult{Name: "Helper", QualifiedName: "Helper", Kind: "class", Path: "src/A.cs"}
	d := SearchResult{Name: "Helper", QualifiedName: "Helper", Kind: "class", Path: "src/B.cs"}
	if symbolKey(c) == symbolKey(d) {
		t.Fatalf("distinct concrete symbols collapsed: %q", symbolKey(c))
	}
}

// TestSearchNamespaceFloodCollapsed is the end-to-end guarantee: when a concept
// query matches both a namespace (declared in N files) and concrete symbols in
// that namespace, the concrete symbols lead and the namespace folds to a single
// trailing entry — instead of N duplicate namespace rows crowding out real code.
func TestSearchNamespaceFloodCollapsed(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	a := []byte("namespace App.Pipeline;\npublic class PipelineRunner { }\n")
	if err := e.IndexFile(ctx, "/wt", "src/A.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	b := []byte("namespace App.Pipeline;\npublic class PipelineGate { }\n")
	if err := e.IndexFile(ctx, "/wt", "src/B.cs", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "pipeline", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("search returned nothing")
	}
	// At most one namespace row survives (the per-file dupes folded).
	var nsCount int
	for _, r := range got {
		if r.Kind == "namespace" {
			nsCount++
		}
	}
	if nsCount > 1 {
		t.Fatalf("namespace flood not collapsed: %d namespace rows in %v", nsCount, resultNames(got))
	}
	// A concrete symbol leads — the namespace never takes the top slot.
	if got[0].Kind == "namespace" {
		t.Fatalf("namespace ranked first over concrete symbols: %v", resultNames(got))
	}
}
