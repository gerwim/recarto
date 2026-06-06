package graph

import (
	"context"
	"testing"
)

func TestIsGeneratedPath(t *testing.T) {
	gen := []string{
		"frontend/src/graphql/generated/graphql.ts",
		"src/__generated__/types.ts",
		"api/pb/service.pb.go",
		"proto/foo_pb2.py",
		"lib/models.g.dart",
		"Api/Schema.generated.cs",
		"web/vendor.min.js",
		"web/site.min.css",
		"Forms/Main.Designer.cs",
		"pkg/apis/zz_generated.deepcopy.go",
	}
	for _, p := range gen {
		if !isGeneratedPath(p) {
			t.Errorf("isGeneratedPath(%q) = false, want true", p)
		}
	}
	// Hand-written paths must NOT be flagged — a false positive demotes real code.
	human := []string{
		"frontend/src/app/system/actions.ts",
		"api/internal/generator.go",     // "generator" is not the "generated" segment
		"src/codegeneration/builder.ts", // substring, not a path segment
		"Domain/Entities/Organization.cs",
		"web/main.js",
		"pkg/svc/handler.go",
	}
	for _, p := range human {
		if isGeneratedPath(p) {
			t.Errorf("isGeneratedPath(%q) = true, want false", p)
		}
	}
}

func TestDemoteGeneratedStablePartition(t *testing.T) {
	in := []SearchResult{
		{Name: "A", Path: "src/a.ts"},
		{Name: "B", Path: "src/generated/g.ts"},
		{Name: "C", Path: "src/c.ts"},
		{Name: "D", Path: "src/generated/h.ts"},
	}
	got := demoteGenerated(in, nil)
	want := []string{"A", "C", "B", "D"} // hand-written first, each group stable
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("demoteGenerated order = %v, want %v", names(got), want)
		}
	}
	// Nothing is dropped.
	if len(got) != len(in) {
		t.Fatalf("demoteGenerated dropped results: %d != %d", len(got), len(in))
	}
}

func TestDemoteGeneratedNoGeneratedIsIdentity(t *testing.T) {
	in := []SearchResult{{Name: "A", Path: "a.ts"}, {Name: "B", Path: "b.ts"}}
	got := demoteGenerated(in, nil)
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Fatalf("demoteGenerated should be a no-op without generated results, got %v", names(got))
	}
}

func TestDemoteGeneratedProtectsExactCandidate(t *testing.T) {
	in := []SearchResult{
		{Name: "Other", Path: "src/a.ts"},
		{Name: "CreateProductInput", Path: "src/generated/g.ts"}, // generated but explicitly requested
	}
	protect := map[string]bool{"createproductinput": true}
	got := demoteGenerated(in, protect)
	// The protected generated symbol keeps its position (not sunk).
	if got[1].Name != "CreateProductInput" {
		t.Fatalf("protected candidate was reordered: %v", names(got))
	}
}

func names(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// TestSearchDemotesGeneratedBelowHandwritten is the end-to-end guarantee: when a
// hand-written and a generated symbol both match a query, the hand-written one
// ranks above the generated one — but the generated one is still returned.
func TestSearchDemotesGeneratedBelowHandwritten(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	human := []byte("export function createProduct() {}\n")
	if err := e.IndexFile(ctx, "/wt", "src/app/actions.ts", human, 1, int64(len(human))); err != nil {
		t.Fatal(err)
	}
	gen := []byte("export type Product = { id: string };\nexport type CreateProductInput = { name: string };\n")
	if err := e.IndexFile(ctx, "/wt", "src/graphql/generated/graphql.ts", gen, 1, int64(len(gen))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "product", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("search returned nothing")
	}
	// First result must be hand-written.
	if isGeneratedPath(got[0].Path) {
		t.Fatalf("generated result ranked first: %v", resultPaths(got))
	}
	// Generated results are still present (no recall loss).
	var sawGen bool
	for _, r := range got {
		if isGeneratedPath(r.Path) {
			sawGen = true
		}
	}
	if !sawGen {
		t.Fatalf("generated results were dropped entirely: %v", resultPaths(got))
	}
}

// TestSearchGuidedExactCandidateInGeneratedNotDemoted guards the contract that an
// agent's exact candidate guess wins even when it lives in a generated file.
func TestSearchGuidedExactCandidateInGeneratedNotDemoted(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	human := []byte("export function createProduct() {}\n")
	if err := e.IndexFile(ctx, "/wt", "src/app/actions.ts", human, 1, int64(len(human))); err != nil {
		t.Fatal(err)
	}
	gen := []byte("export type CreateProductInput = { name: string };\n")
	if err := e.IndexFile(ctx, "/wt", "src/graphql/generated/graphql.ts", gen, 1, int64(len(gen))); err != nil {
		t.Fatal(err)
	}
	got, err := e.SearchGuided(ctx, "/wt", "product mutation",
		[]string{"CreateProductInput"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "CreateProductInput" {
		t.Fatalf("exact candidate in generated file should rank first, got %v", resultNames(got))
	}
}

func resultPaths(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}
