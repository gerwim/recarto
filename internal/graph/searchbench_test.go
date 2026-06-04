package graph

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// benchEngine returns an engine over a persistent store at RECARTO_BENCH_STORE
// (so repeated benchmark runs reuse the index via incremental reconcile), or a
// throwaway temp store when that env is unset.
func benchEngine(t *testing.T) *Engine {
	t.Helper()
	path := os.Getenv("RECARTO_BENCH_STORE")
	if path == "" {
		return newTestEngine(t)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e, err := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// benchQuery is one labelled query: a natural-language or keyword string and the
// set of symbol names that SHOULD surface for it. type is "semantic" (zero
// identifier-token overlap with the targets — only meaning bridges them) or
// "lexical-control" (shares tokens, must work with plain FTS).
type benchQuery struct {
	Q    string   `json:"q"`
	Rel  []string `json:"rel"`
	Type string   `json:"type"`
	Cand []string `json:"cand,omitempty"` // LLM-supplied candidate_symbols (semantic queries)
}

// TestSearchQualityBenchmark indexes a real worktree and measures how well
// Search surfaces the expected symbols for a labelled query set. It is the
// instrument used to compare baseline (lexical FTS) against Tier A (stemming/
// expansion) and Tier B (hybrid vector) — run it before and after each change.
//
// It is gated on RECARTO_BENCH_REPO (a path to a checkout, e.g. Ordia) because
// it needs a real, sizeable codebase; without it the test skips. Set
// RECARTO_BENCH_K to change the cutoff (default 10).
//
//	RECARTO_BENCH_REPO=/path/to/ordia go test ./internal/graph -run SearchQualityBenchmark -v
func TestSearchQualityBenchmark(t *testing.T) {
	repo := os.Getenv("RECARTO_BENCH_REPO")
	if repo == "" {
		t.Skip("set RECARTO_BENCH_REPO=<checkout> to run the search-quality benchmark")
	}
	k := 10
	if v := os.Getenv("RECARTO_BENCH_K"); v != "" {
		if n, err := parsePositiveInt(v); err == nil {
			k = n
		}
	}

	raw, err := os.ReadFile("testdata/searchbench.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []benchQuery
	if err := json.Unmarshal(raw, &queries); err != nil {
		t.Fatal(err)
	}

	// Reuse a persistent store across runs when RECARTO_BENCH_STORE is set so
	// tuning iterations skip the (slow) full index: Reconcile is incremental, so a
	// warm store reconciles cheaply. Delete the file after any indexer change
	// (parser/FTS), since the graph is only rebuilt by the schema migration.
	e := benchEngine(t)
	ctx := context.Background()
	if emb := benchEmbedder(t); emb != nil {
		e.SetEmbedder(emb) // hybrid (semantic) tier; -tags ORT only
	}
	// RECARTO_BENCH_NORECONCILE skips the (slow) index/embed pass to re-measure
	// fusion against an already-warm store — set it only when the store at
	// RECARTO_BENCH_STORE was just built by a full run.
	if os.Getenv("RECARTO_BENCH_NORECONCILE") == "" {
		if err := e.Reconcile(ctx, repo, FSSource{Root: repo}); err != nil {
			t.Fatalf("reconcile %s: %v", repo, err)
		}
	}

	// Per-type accumulators.
	type acc struct {
		n        int
		recall   float64 // mean fraction of rel found in top-k
		mrr      float64 // mean reciprocal rank of the first rel hit
		zeroHits int     // queries that surfaced none of their rel symbols
	}
	byType := map[string]*acc{}
	t.Logf("indexed %s; k=%d; %d queries", repo, k, len(queries))
	t.Logf("%-42s %-15s recall@k  rank1", "query", "type")

	for _, q := range queries {
		res, err := e.SearchGuided(ctx, repo, q.Q, q.Cand, "", "", k)
		if err != nil {
			t.Fatalf("search %q: %v", q.Q, err)
		}
		rank := map[string]int{}
		for i, r := range res {
			if _, ok := rank[r.Name]; !ok {
				rank[r.Name] = i + 1 // 1-based; first occurrence wins
			}
		}
		found := 0
		best := 0 // best (lowest) rank among rel hits, 0 = none
		for _, name := range q.Rel {
			if rr, ok := rank[name]; ok {
				found++
				if best == 0 || rr < best {
					best = rr
				}
			}
		}
		var missed []string
		for _, name := range q.Rel {
			if _, ok := rank[name]; !ok {
				missed = append(missed, name)
			}
		}
		recall := float64(found) / float64(len(q.Rel))
		mrr := 0.0
		if best > 0 {
			mrr = 1.0 / float64(best)
		}
		a := byType[q.Type]
		if a == nil {
			a = &acc{}
			byType[q.Type] = a
		}
		a.n++
		a.recall += recall
		a.mrr += mrr
		if found == 0 {
			a.zeroHits++
		}
		rank1 := "-"
		if best > 0 {
			rank1 = itoa(best)
		}
		miss := ""
		if len(missed) > 0 {
			miss = "  MISS:" + strings.Join(missed, ",")
		}
		top := make([]string, 0, 5)
		for i, r := range res {
			if i >= 5 {
				break
			}
			top = append(top, r.Name)
		}
		t.Logf("%-42s %-15s %6.0f%%   %5s%s", truncate(q.Q, 42), q.Type, recall*100, rank1, miss)
		t.Logf("        top5: %s", strings.Join(top, ", "))
	}

	types := make([]string, 0, len(byType))
	for ty := range byType {
		types = append(types, ty)
	}
	sort.Strings(types)
	t.Logf("---- summary (k=%d) ----", k)
	for _, ty := range types {
		a := byType[ty]
		t.Logf("%-15s n=%-3d recall@k=%5.1f%%  MRR=%.3f  zero-hit=%d/%d",
			ty, a.n, a.recall/float64(a.n)*100, a.mrr/float64(a.n), a.zeroHits, a.n)
	}
}

// TestVectorOnlyBenchmark measures the vector tier in isolation against an
// already-embedded persistent store (RECARTO_BENCH_STORE), bypassing lexical
// fusion and reconcile/re-embed. It calls vectorPass directly so we can see
// whether the semantic tier itself finds the targets at full corpus scale, vs
// being crowded out by the lexical-first hybrid. Gated on RECARTO_BENCH_VECONLY.
//
//	RECARTO_BENCH_VECONLY=1 RECARTO_BENCH_REPO=/path/to/ordia RECARTO_BENCH_STORE=... \
//	  go test -tags ORT -run VectorOnlyBenchmark -v ./internal/graph
func TestVectorOnlyBenchmark(t *testing.T) {
	if os.Getenv("RECARTO_BENCH_VECONLY") == "" {
		t.Skip("set RECARTO_BENCH_VECONLY=1 to run the vector-only probe")
	}
	repo := os.Getenv("RECARTO_BENCH_REPO")
	if repo == "" {
		t.Skip("set RECARTO_BENCH_REPO=<checkout> (= the worktree the vectors were embedded under)")
	}
	k := 10
	if v := os.Getenv("RECARTO_BENCH_K"); v != "" {
		if n, err := parsePositiveInt(v); err == nil {
			k = n
		}
	}
	raw, err := os.ReadFile("testdata/searchbench.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []benchQuery
	if err := json.Unmarshal(raw, &queries); err != nil {
		t.Fatal(err)
	}
	e := benchEngine(t)
	ctx := context.Background()
	emb := benchEmbedder(t)
	if emb == nil {
		t.Skip("vector-only probe needs an embedder (-tags ORT + RECARTO_BENCH_EMBED_MODEL)")
	}
	e.SetEmbedder(emb)

	type acc struct {
		n        int
		recall   float64
		mrr      float64
		zeroHits int
	}
	byType := map[string]*acc{}
	t.Logf("vector-only probe over %s; k=%d; %d queries", repo, k, len(queries))
	for _, q := range queries {
		res, err := e.vectorPass(ctx, repo, q.Q, "", "", k)
		if err != nil {
			t.Fatalf("vectorPass %q: %v", q.Q, err)
		}
		rank := map[string]int{}
		for i, r := range res {
			if _, ok := rank[r.Name]; !ok {
				rank[r.Name] = i + 1
			}
		}
		found, best := 0, 0
		for _, name := range q.Rel {
			if rr, ok := rank[name]; ok {
				found++
				if best == 0 || rr < best {
					best = rr
				}
			}
		}
		var missed []string
		for _, name := range q.Rel {
			if _, ok := rank[name]; !ok {
				missed = append(missed, name)
			}
		}
		recall := float64(found) / float64(len(q.Rel))
		mrr := 0.0
		if best > 0 {
			mrr = 1.0 / float64(best)
		}
		a := byType[q.Type]
		if a == nil {
			a = &acc{}
			byType[q.Type] = a
		}
		a.n++
		a.recall += recall
		a.mrr += mrr
		if found == 0 {
			a.zeroHits++
		}
		rank1 := "-"
		if best > 0 {
			rank1 = itoa(best)
		}
		miss := ""
		if len(missed) > 0 {
			miss = "  MISS:" + strings.Join(missed, ",")
		}
		top := make([]string, 0, 5)
		for i, r := range res {
			if i >= 5 {
				break
			}
			top = append(top, r.Name)
		}
		t.Logf("%-42s %-15s %6.0f%%   %5s%s", truncate(q.Q, 42), q.Type, recall*100, rank1, miss)
		t.Logf("        top5: %s", strings.Join(top, ", "))
	}
	types := make([]string, 0, len(byType))
	for ty := range byType {
		types = append(types, ty)
	}
	sort.Strings(types)
	t.Logf("---- vector-only summary (k=%d) ----", k)
	for _, ty := range types {
		a := byType[ty]
		t.Logf("%-15s n=%-3d recall@k=%5.1f%%  MRR=%.3f  zero-hit=%d/%d",
			ty, a.n, a.recall/float64(a.n)*100, a.mrr/float64(a.n), a.zeroHits, a.n)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, os.ErrInvalid
	}
	return n, nil
}
