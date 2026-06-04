package graph

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestMergeHybridReservesSlotsForVector pins the fix for the lexical crowd-out
// bug: when the lexical tier returns enough unique names to fill the whole page
// (a query like "options"/"state" that hits many incidental symbols), the vector
// tier must still get its reserved tail so concept matches survive. Before the
// reserve, append+dedup gave the vector tier zero slots and semantic recall
// collapsed to 0% at corpus scale.
func TestMergeHybridReservesSlotsForVector(t *testing.T) {
	limit := 10
	var lex []SearchResult
	for i := range 20 {
		lex = append(lex, SearchResult{Name: fmt.Sprintf("lex%02d", i), Score: float64(100 - i)})
	}
	vec := []SearchResult{{Name: "VecTarget"}, {Name: "VecCousin1"}, {Name: "VecCousin2"}}

	got := mergeHybrid(lex, vec, limit)
	if len(got) != limit {
		t.Fatalf("want %d results, got %d", limit, len(got))
	}
	if got[0].Name != "lex00" {
		t.Fatalf("top lexical hit must keep rank 1 (no keyword regression), got %q", got[0].Name)
	}
	names := map[string]bool{}
	for _, r := range got {
		names[r.Name] = true
	}
	if !names["VecTarget"] {
		t.Fatalf("reserved-slot fusion must surface the vector hit even when lexical fills the page; got %v", resultNames(got))
	}
	// 2/5 reserve of 10 => up to 4 vector slots; all 3 vector candidates fit.
	if !names["VecCousin1"] || !names["VecCousin2"] {
		t.Fatalf("all vector candidates within the reserve should appear; got %v", resultNames(got))
	}
}

// TestMergeHybridBackfillsWhenVectorShort proves the reserve is a ceiling, not a
// floor: an unused reserve is backfilled with lexical so the page is never short.
func TestMergeHybridBackfillsWhenVectorShort(t *testing.T) {
	var lex []SearchResult
	for i := range 20 {
		lex = append(lex, SearchResult{Name: fmt.Sprintf("lex%02d", i)})
	}
	got := mergeHybrid(lex, []SearchResult{{Name: "V"}}, 10)
	if len(got) != 10 {
		t.Fatalf("want a full page of 10, got %d", len(got))
	}
	var vecCount int
	for _, r := range got {
		if r.Name == "V" {
			vecCount++
		}
	}
	if vecCount != 1 {
		t.Fatalf("the single vector hit should appear exactly once; got %v", resultNames(got))
	}
}

// TestMergeHybridNoVectorIsPlainLexical: with no vector candidates the hybrid
// degrades exactly to a lexical dedup (the no-embedder path).
func TestMergeHybridNoVectorIsPlainLexical(t *testing.T) {
	lex := []SearchResult{{Name: "a"}, {Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := mergeHybrid(lex, nil, 10)
	want := dedupByName(lex, 10)
	if len(got) != len(want) {
		t.Fatalf("no-vector merge must equal lexical dedup: got %v want %v", resultNames(got), resultNames(want))
	}
	for i := range got {
		if got[i].Name != want[i].Name {
			t.Fatalf("no-vector merge must equal lexical dedup: got %v want %v", resultNames(got), resultNames(want))
		}
	}
}

// conceptEmbedder is a deterministic stand-in for a real embedding model: it
// places text into a tiny hand-built "concept space" where chosen words share an
// axis (e.g. "drive" and "travel" both load axis 0). That lets a test prove the
// semantic tier finds a symbol whose name shares NO token with the query —
// exactly what a real embedder buys and what lexical search cannot do — without
// pulling in an ONNX runtime.
type conceptEmbedder struct {
	axes map[string]int
	dim  int
}

func (c conceptEmbedder) Dim() int { return c.dim }

func (c conceptEmbedder) embed(t string) []float32 {
	v := make([]float32, c.dim)
	lt := strings.ToLower(t)
	for word, ax := range c.axes {
		if strings.Contains(lt, word) {
			v[ax] = 1
		}
	}
	return v
}

func (c conceptEmbedder) EmbedDocs(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = c.embed(t)
	}
	return out, nil
}

func (c conceptEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return c.embed(text), nil
}

func TestSearchSemanticFindsZeroOverlapMatch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	e.SetEmbedder(conceptEmbedder{
		dim:  3,
		axes: map[string]int{"drive": 0, "travel": 0, "route": 0, "auth": 1, "permission": 1, "store": 2, "persist": 2},
	})

	src := []byte("namespace App;\nclass TravelTimeService {}\nclass PermissionChecker {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.embedWorktree(ctx, "/wt"); err != nil {
		t.Fatalf("embedWorktree: %v", err)
	}

	// "drive" shares no token with "TravelTimeService"; only the semantic tier
	// (drive ~ travel) can surface it.
	got, err := e.Search(ctx, "/wt", "how long to drive somewhere", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range got {
		if r.Name == "TravelTimeService" {
			found = true
		}
	}
	if !found {
		t.Fatalf("semantic search for 'drive' should surface TravelTimeService, got %v", resultNames(got))
	}
}

func TestSearchSemanticDoesNotBreakLexical(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	e.SetEmbedder(conceptEmbedder{dim: 3, axes: map[string]int{"drive": 0, "travel": 0}})

	src := []byte("namespace App;\nclass AuthenticationService {}\nclass TravelTimeService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.embedWorktree(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	// An exact keyword query must still return its exact match first under hybrid
	// fusion (lexical keeps the edge).
	got, err := e.Search(ctx, "/wt", "AuthenticationService", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "AuthenticationService" {
		t.Fatalf("exact query should rank its exact match first under hybrid, got %v", resultNames(got))
	}
}

func TestEmbedWorktreeSkipsNonEmbeddableKinds(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	e.SetEmbedder(conceptEmbedder{dim: 2, axes: map[string]int{"travel": 0}})
	// A property is not an embeddable kind; only the class should get a vector.
	src := []byte("namespace App;\nclass TravelTimeService { public int Minutes { get; set; } }")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.embedWorktree(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var classVecs, propVecs int
	if err := e.st.DB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM node_vectors v JOIN nodes n ON n.id=v.node_id WHERE n.kind='class'`).Scan(&classVecs); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM node_vectors v JOIN nodes n ON n.id=v.node_id WHERE n.kind='property'`).Scan(&propVecs); err != nil {
		t.Fatal(err)
	}
	if classVecs != 1 || propVecs != 0 {
		t.Fatalf("want 1 class vector and 0 property vectors, got class=%d property=%d", classVecs, propVecs)
	}
}
