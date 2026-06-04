# Query-Guided Code Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an LLM agent steer code search by passing the identifier names it expects (`candidate_symbols`) alongside a natural-language concept, feeding the existing dependency-free lexical engine — retiring the slow heavyweight embedding tier.

**Architecture:** Pure query-side change. `Engine.SearchGuided` runs two new high-priority lexical passes over the LLM's candidate identifiers (exact name/qualified-name equality, then identifier-split FTS) and fuses them ABOVE the current natural-language passes via the existing weighted-RRF `fuseResults` + `dedupByName`. `search_graph` (MCP) gains an optional `candidate_symbols` field and a weak-result `hint`; the model is coached to fill the field via the tool description and the installed instructions block. The dormant ORT/`vec0` tier is left in place but no longer recommended.

**Tech Stack:** Go 1.26.4, `modernc.org/sqlite` (CGO-free) FTS5, the `mcp` go-sdk, the project's existing graph/agents/cli packages.

**Spec:** `docs/superpowers/specs/2026-06-04-query-guided-search-design.md`

**Design facts the engineer needs (don't rediscover):**
- `bm25()` returns NEGATIVE scores; `ftsPass` already flips the sign so higher = better.
- FTS content is identifier-split (`ftsText`), so `baseTerms("AddressLookup")` → `["address","lookup"]` matches the stored `nodes_fts` content.
- `fuseResults(...rankedList)` keys by `NodeID`, sums `weight/(60+rank)` across lists, returns best-first. `dedupByName` then collapses same-name overload clusters and truncates to `limit`.
- Existing lexical weights live in `internal/graph/search.go`: `exactWeight = 1.0`, etc.
- Test engine helper: `newTestEngine(t)` (in `internal/graph/graph_test.go`); index source with `e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src)))`; `resultNames(rs)` returns the names slice.
- MCP/CLI test helper: `newTestApp(t, "main")`; set `a.Git.Worktree = wt`; index a worktree with `a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt})` (MCP) or `Index(ctx, a, &bytes.Buffer{}, nil)` (CLI).

---

## Task 1: Engine — `SearchGuided` with candidate exact + split passes

**Files:**
- Modify: `internal/graph/search.go` (add consts, `SearchGuided`, `mergeCandidates`, `candidateExactPass`; make `Search` delegate)
- Test: `internal/graph/search_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/graph/search_test.go`:

```go
func TestSearchGuidedCandidateBoost(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// The concept query shares NO identifier token with the target name; only the
	// LLM-supplied candidate bridges them.
	src := []byte("namespace App;\nclass PdokAddressLookupService {}\nclass InvoiceService {}\nclass ReportService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Split-token candidate: "AddressLookup" → address/lookup → PdokAddressLookupService.
	got, err := e.SearchGuided(ctx, "/wt", "convert a street into coordinates",
		[]string{"AddressLookup"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "PdokAddressLookupService" {
		t.Fatalf("split candidate should boost PdokAddressLookupService to top, got %v", resultNames(got))
	}

	// Exact-name candidate: lands the precise symbol even with an unrelated query.
	got2, err := e.SearchGuided(ctx, "/wt", "totally unrelated prose",
		[]string{"InvoiceService"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) == 0 || got2[0].Name != "InvoiceService" {
		t.Fatalf("exact candidate should rank InvoiceService first, got %v", resultNames(got2))
	}
}

func TestSearchGuidedNilCandidatesMatchesSearch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\nclass AlternativeSlotFinder {}\nclass SlotRepository {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	base, err := e.Search(ctx, "/wt", "alternative slot", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	guided, err := e.SearchGuided(ctx, "/wt", "alternative slot", nil, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	bn, gn := resultNames(base), resultNames(guided)
	if len(bn) != len(gn) {
		t.Fatalf("nil-candidate SearchGuided diverged from Search: %v vs %v", bn, gn)
	}
	for i := range bn {
		if bn[i] != gn[i] {
			t.Fatalf("nil-candidate SearchGuided diverged at %d: %v vs %v", i, bn, gn)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/graph -run 'TestSearchGuided' -v`
Expected: FAIL — `e.SearchGuided undefined`.

- [ ] **Step 3: Add the `strings` import to `search.go`**

In `internal/graph/search.go`, change the import block to:

```go
import (
	"context"
	"fmt"
	"strings"
)
```

- [ ] **Step 4: Add the candidate fusion weights**

In `internal/graph/search.go`, after the existing `exactWeight … triMinTermLen` const block, add:

```go
// Candidate-symbol fusion weights. When the LLM supplies candidate_symbols (the
// identifier names it expects), an exact name/qualified-name match is the highest
// possible signal — the model's guess landing precisely — so it outranks every
// lexical pass; an identifier-split match on the guesses ranks just above plain
// natural-language lexical hits.
const (
	candidateExactWeight = 2.0
	candidateSplitWeight = 1.2
)
```

- [ ] **Step 5: Make `Search` delegate and add `SearchGuided`**

In `internal/graph/search.go`, replace the entire existing `func (e *Engine) Search(...) {...}` body with:

```go
// Search returns symbols matching query, scoped to worktree and optionally
// filtered by kind and language. It is SearchGuided with no candidate symbols:
// purely lexical (an exact pass plus stemmed/synonym/trigram broadeners on
// underfill), with the dormant vector tier fused in only when an embedder is set.
func (e *Engine) Search(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	return e.SearchGuided(ctx, worktree, query, nil, kind, lang, limit)
}

// SearchGuided is Search with optional candidate_symbols: the identifier names an
// LLM expects to exist for the concept in query. They drive two high-priority
// lexical passes — exact name/qualified-name equality, then identifier-split FTS —
// fused ABOVE the natural-language passes, so a concept query the agent annotated
// with the right names surfaces those symbols even when the prose shares no tokens
// with them. With no candidates the behaviour is identical to the previous Search.
func (e *Engine) SearchGuided(ctx context.Context, worktree, query string, candidateSymbols []string, kind, lang string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	lex, err := e.lexicalSearch(ctx, worktree, query, kind, lang, limit)
	if err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	if len(candidateSymbols) > 0 {
		return e.mergeCandidates(ctx, worktree, candidateSymbols, lex, kind, lang, limit)
	}

	if e.emb == nil {
		return dedupByName(lex, limit), nil
	}
	// Dormant semantic tier (set only under -tags ORT). Best-effort.
	vec, verr := e.vectorPass(ctx, worktree, query, kind, lang, limit*3)
	if verr != nil {
		vec = nil
	}
	return mergeHybrid(lex, vec, limit), nil
}

// mergeCandidates fuses the candidate exact-name and identifier-split passes above
// the natural-language lexical list, then dedups by name. The exact pass leads
// (its hits are the agent's guesses landing precisely), the split pass ranks above
// plain lexical, and lexical backfills the remaining page.
func (e *Engine) mergeCandidates(ctx context.Context, worktree string, candidates []string, lex []SearchResult, kind, lang string, limit int) ([]SearchResult, error) {
	fetch := limit * 3
	exact, err := e.candidateExactPass(ctx, worktree, candidates, kind, lang, fetch)
	if err != nil {
		return nil, err
	}
	var split []SearchResult
	if terms := baseTerms(strings.Join(candidates, " ")); len(terms) > 0 {
		split, err = e.ftsPass(ctx, "nodes_fts", orMatch(terms), worktree, kind, lang, fetch)
		if err != nil {
			return nil, err
		}
	}
	fused := fuseResults(
		rankedList{weight: candidateExactWeight, results: exact},
		rankedList{weight: candidateSplitWeight, results: split},
		rankedList{weight: exactWeight, results: lex},
	)
	return dedupByName(fused, limit), nil
}

// candidateExactPass returns symbols whose name OR qualified_name equals one of
// the candidate identifiers (case-insensitive), scoped to worktree and optional
// kind/language. These are the agent's guesses landing exactly, so the caller
// fuses them at the top weight. Returns nil for an empty/blank candidate set.
func (e *Engine) candidateExactPass(ctx context.Context, worktree string, candidates []string, kind, lang string, limit int) ([]SearchResult, error) {
	seen := map[string]bool{}
	var lowered []any
	var ph []string
	for _, c := range candidates {
		lc := strings.ToLower(strings.TrimSpace(c))
		if lc == "" || seen[lc] {
			continue
		}
		seen[lc] = true
		lowered = append(lowered, lc)
		ph = append(ph, "?")
	}
	if len(lowered) == 0 {
		return nil, nil
	}
	inList := strings.Join(ph, ",")
	args := []any{worktree}
	args = append(args, lowered...) // lower(n.name) IN (...)
	args = append(args, lowered...) // lower(n.qualified_name) IN (...)
	sql := `
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line
  FROM nodes n
  JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?
   AND (lower(n.name) IN (` + inList + `) OR lower(n.qualified_name) IN (` + inList + `))`
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	sql += " ORDER BY n.name LIMIT ?"
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/graph -run 'TestSearchGuided' -v`
Expected: PASS (both tests).

- [ ] **Step 7: Run the full graph package to confirm no regression**

Run: `go test ./internal/graph`
Expected: ok (all existing lexical/fusion tests still pass; `Search` delegates unchanged for nil candidates).

- [ ] **Step 8: Commit**

```bash
git add internal/graph/search.go internal/graph/search_test.go
git commit -m "feat(graph): SearchGuided — candidate_symbols exact+split boost passes"
```

---

## Task 2: MCP — `candidate_symbols` input + weak-result `hint`

**Files:**
- Modify: `internal/mcp/graph.go` (`SearchGraphInput`, `SearchGraphOutput`, `searchGraph`)
- Test: `internal/mcp/graph_test.go` (append a test)

- [ ] **Step 1: Write the failing test**

Append to `internal/mcp/graph_test.go`:

```go
func TestSearchGraphCandidateSymbolsAndHint(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.cs"),
		[]byte("namespace App;\nclass PdokAddressLookupService {}\nclass InvoiceService {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// candidate_symbols drives a concept query to the right symbol; no hint when found.
	out, err := searchGraph(ctx, a, SearchGraphInput{
		Query:            "convert a street into coordinates",
		CandidateSymbols: []string{"AddressLookup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range out.Results {
		if r.Name == "PdokAddressLookupService" {
			found = true
		}
	}
	if !found {
		t.Errorf("candidate_symbols search missed PdokAddressLookupService: %+v", out.Results)
	}
	if out.Hint != "" {
		t.Errorf("hint should be empty when results were found, got %q", out.Hint)
	}

	// Empty result + no candidates → hint nudges the agent to supply candidate_symbols.
	miss, err := searchGraph(ctx, a, SearchGraphInput{Query: "zzzznotarealconcept"})
	if err != nil {
		t.Fatal(err)
	}
	if miss.Count != 0 || miss.Hint == "" {
		t.Errorf("expected zero results with a hint, got count=%d hint=%q", miss.Count, miss.Hint)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcp -run TestSearchGraphCandidateSymbolsAndHint -v`
Expected: FAIL — `unknown field CandidateSymbols` / `out.Hint undefined`.

- [ ] **Step 3: Update the input/output types and handler**

In `internal/mcp/graph.go`, replace the `SearchGraphInput` and `SearchGraphOutput` types and the `searchGraph` function with:

```go
type SearchGraphInput struct {
	Query            string   `json:"query" jsonschema:"the concept or symbol name to find, in plain natural language"`
	CandidateSymbols []string `json:"candidate_symbols,omitempty" jsonschema:"the identifier names you expect to exist for this concept (3-8 best guesses). Any form is accepted and matched leniently: PascalCase (AddressLookup), interface-prefixed (IAddressLookupService), snake_case (address_lookup), plain phrases (address lookup), or partial/qualified names. Provide these whenever searching by concept rather than an exact known name."`
	Kind             string   `json:"kind,omitempty" jsonschema:"restrict to a node kind (function, method, class, …)"`
	Language         string   `json:"language,omitempty" jsonschema:"restrict to a language (go, csharp, typescript, tsx, javascript)"`
	Limit            int      `json:"limit,omitempty" jsonschema:"max results"`
}
type GraphSymbol struct {
	Name          string  `json:"name"`
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	Line          int     `json:"line"`
	Score         float64 `json:"score,omitempty"`
}
type SearchGraphOutput struct {
	Count   int           `json:"count"`
	Results []GraphSymbol `json:"results"`
	Hint    string        `json:"hint,omitempty"`
}

// weakResultHint nudges the agent to use candidate_symbols when a concept query
// found nothing and supplied none. No server-side model is involved.
const weakResultHint = "No matches. Retry search_graph with candidate_symbols set to the identifier names you'd expect for this concept (e.g. AddressLookup, Geocode) — any form works (PascalCase, interface-prefixed, snake_case, or plain words)."

func searchGraph(ctx context.Context, a *app.App, in SearchGraphInput) (SearchGraphOutput, error) {
	ensureFresh(ctx, a)
	res, err := a.Graph.SearchGuided(ctx, a.Git.Worktree, in.Query, in.CandidateSymbols, in.Kind, in.Language, in.Limit)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	out := SearchGraphOutput{Count: len(res), Results: make([]GraphSymbol, 0, len(res))}
	for _, r := range res {
		out.Results = append(out.Results, GraphSymbol{
			Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
			Path: r.Path, Line: r.Line, Score: r.Score,
		})
	}
	if out.Count == 0 && len(in.CandidateSymbols) == 0 {
		out.Hint = weakResultHint
	}
	return out, nil
}
```

(The existing `GraphSymbol` type is unchanged — it is reproduced here only because it sits between the two edited types; leave a single definition, do not duplicate it.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/mcp -run TestSearchGraphCandidateSymbolsAndHint -v`
Expected: PASS.

- [ ] **Step 5: Run the mcp package to confirm no regression**

Run: `go test ./internal/mcp`
Expected: ok (the existing `TestSearchGraphHandler` still passes — it passes no candidates and finds a result, so no hint).

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/graph.go internal/mcp/graph_test.go
git commit -m "feat(mcp): search_graph candidate_symbols input + weak-result hint"
```

---

## Task 3: Coach the model — tool description + instructions block

**Files:**
- Modify: `internal/mcp/server.go:36-38` (the `search_graph` `addTool` description)
- Modify: `internal/agents/instructions.go` (the `Instructions` const)
- Test: `internal/agents/instructions_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `internal/agents/instructions_test.go`:

```go
package agents

import (
	"strings"
	"testing"
)

func TestInstructionsCoachesCandidateSymbols(t *testing.T) {
	if !strings.Contains(Instructions, "candidate_symbols") {
		t.Error("Instructions block should coach agents to pass candidate_symbols to search_graph")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agents -run TestInstructionsCoachesCandidateSymbols -v`
Expected: FAIL — `Instructions` does not contain `candidate_symbols`.

- [ ] **Step 3: Update the instructions block**

In `internal/agents/instructions.go`, replace the bullet:

```go
- Query the code graph with ` + "`search_graph` / `get_code_snippet` / `trace_call_path`" + `
  instead of reading whole files.
```

with:

```go
- Query the code graph with ` + "`search_graph` / `get_code_snippet` / `trace_call_path`" + `
  instead of reading whole files. When searching ` + "`search_graph`" + ` by concept, also
  pass ` + "`candidate_symbols`" + ` — the identifier names you expect (e.g. ` + "`AddressLookup`, `Geocode`" + `);
  the names you supply are what make concept search land.
```

- [ ] **Step 4: Update the MCP tool description**

In `internal/mcp/server.go`, replace the `search_graph` registration:

```go
	addTool(s, a, "search_graph",
		"Search the code graph for symbols by name or keywords (functions, methods, classes, types).",
		searchGraph)
```

with:

```go
	addTool(s, a, "search_graph",
		"Search the code graph for symbols (functions, methods, classes, types). Pass `query` as the concept in plain words; when searching by concept, also pass `candidate_symbols` — the identifier names you expect to exist (e.g. AddressLookup, Geocode). The names you supply are what make concept search land; any form works (PascalCase, interface-prefixed, snake_case, or plain words).",
		searchGraph)
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/agents -run TestInstructionsCoachesCandidateSymbols -v`
Expected: PASS.

- [ ] **Step 6: Confirm agents + mcp packages still build/pass**

Run: `go test ./internal/agents ./internal/mcp`
Expected: ok (instructions idempotency tests are content-agnostic; the block still round-trips).

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/server.go internal/agents/instructions.go internal/agents/instructions_test.go
git commit -m "feat: coach agents to pass search_graph candidate_symbols (tool desc + instructions)"
```

---

## Task 4: Benchmark — measure query-guided recall on the real corpus

**Files:**
- Modify: `internal/graph/searchbench_test.go` (`benchQuery` struct + the `Search` call site in `TestSearchQualityBenchmark`)
- Modify: `internal/graph/testdata/searchbench.json` (add `cand` arrays to the semantic queries)

- [ ] **Step 1: Add the `Cand` field to `benchQuery`**

In `internal/graph/searchbench_test.go`, replace the `benchQuery` struct:

```go
type benchQuery struct {
	Q    string   `json:"q"`
	Rel  []string `json:"rel"`
	Type string   `json:"type"`
}
```

with:

```go
type benchQuery struct {
	Q    string   `json:"q"`
	Rel  []string `json:"rel"`
	Type string   `json:"type"`
	Cand []string `json:"cand,omitempty"` // LLM-supplied candidate_symbols (semantic queries)
}
```

- [ ] **Step 2: Use the candidates in the benchmark search call**

In `internal/graph/searchbench_test.go`, inside `TestSearchQualityBenchmark`, replace:

```go
		res, err := e.Search(ctx, repo, q.Q, "", "", k)
```

with:

```go
		res, err := e.SearchGuided(ctx, repo, q.Q, q.Cand, "", "", k)
```

- [ ] **Step 3: Add candidate arrays to the benchmark query set**

Replace the entire contents of `internal/graph/testdata/searchbench.json` with:

```json
[
 {"q":"convert a street into map coordinates","cand":["AddressLookup","Geocode","Coordinate","GeoLocation"],"rel":["IAddressLookupService","PdokAddressLookupService","AddressLookupResult"],"type":"semantic"},
 {"q":"compute the most efficient driving order","cand":["RouteOptimization","RouteReordering","OptimizeRoute","VehicleRouting"],"rel":["IRouteOptimizationService","OrToolsRouteOptimizationService","IRouteReorderingService"],"type":"semantic"},
 {"q":"decide whether a user may perform an action","cand":["Authorization","Permission","AuthorizationHandler","AccessControl"],"rel":["IAuthorizationService","PermissionAuthorizationHandler"],"type":"semantic"},
 {"q":"persist pending edits to the database","cand":["UnitOfWork","SaveChanges","DbContext","Repository"],"rel":["IUnitOfWork","SaveChangesAsync","ApplicationDbContext"],"type":"semantic"},
 {"q":"how long to drive between two locations","cand":["TravelTime","Duration","DistanceMatrix"],"rel":["ITravelTimeService","TravelTimeService"],"type":"semantic"},
 {"q":"suggest the best date for a visit","cand":["OptimalDay","GetOptimalDays","Scheduling"],"rel":["IOptimalDayService","OptimalDayPicker","GetOptimalDays"],"type":"semantic"},
 {"q":"the current state of a booking","cand":["AppointmentStatus","BookingStatus","BookingState"],"rel":["AppointmentStatus"],"type":"semantic"},
 {"q":"propose replacement time options","cand":["AlternativeSlot","AlternativeSlots","SmartAlternative","Reschedule"],"rel":["ISmartAlternativeService","AlternativeSlots","IAlternativeSlotFindingService","FindSmartAlternativesAsync"],"type":"semantic"},
 {"q":"earlier openings at another location","cand":["SmartAlternative","FindSmartAlternatives","Alternative","FindAlternatives"],"rel":["AlternativeSlots","IAlternativeSlotFindingService","FindAlternativesAsync"],"type":"semantic"},
 {"q":"availability of a staff member","cand":["WorkingHours","SetWorkingHours","Availability","StaffSchedule"],"rel":["WorkingHoursInput","SetWorkingHours"],"type":"semantic"},
 {"q":"alternative slots","rel":["AlternativeSlots","IAlternativeSlotFindingService"],"type":"lexical-control"},
 {"q":"authorization service","rel":["IAuthorizationService"],"type":"lexical-control"},
 {"q":"travel time","rel":["ITravelTimeService","TravelTimeService"],"type":"lexical-control"},
 {"q":"optimal day","rel":["IOptimalDayService","OptimalDayPicker"],"type":"lexical-control"}
]
```

- [ ] **Step 4: Verify the benchmark still compiles and skips cleanly without a repo**

Run: `go test ./internal/graph -run SearchQualityBenchmark`
Expected: ok (SKIP — no `RECARTO_BENCH_REPO`); the package must still compile with the new field/call.

- [ ] **Step 5: Run the real measurement against the warm Ordia store**

Run:
```bash
RECARTO_BENCH_REPO=/home/claude/Projects/ordia \
RECARTO_BENCH_STORE=/home/claude/recarto-e2e/embed/bench-store.db \
RECARTO_BENCH_NORECONCILE=1 \
go test ./internal/graph -run SearchQualityBenchmark -v 2>&1 | tail -40
```
Expected: the `---- summary ----` shows **`semantic recall@k ≥ 46.7%`** (higher is better — the real Tier A broadeners + dedup should meet or beat the BM25 proxy) and **`lexical-control recall@k ≥ 87.5%`** (hard guardrail, no keyword regression). If semantic is below 46.7%, inspect the per-query `MISS:` lines and the `top5` to see which candidate tier failed before adjusting weights — do not lower the target.

- [ ] **Step 6: Commit**

```bash
git add internal/graph/searchbench_test.go internal/graph/testdata/searchbench.json
git commit -m "test(graph): query-guided candidate_symbols in search benchmark"
```

---

## Task 5: CLI — optional `--symbol` flag on `graph search`

**Files:**
- Modify: `internal/cli/graph.go` (the `case "search":` block)
- Test: `internal/cli/graph_test.go` (append a test)

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/graph_test.go`:

```go
func TestGraphSearchSymbolFlag(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.cs"),
		[]byte("namespace App;\nclass PdokAddressLookupService {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Concept words + a --symbol candidate that bridges to the target name.
	if err := Graph(context.Background(), a, &out,
		[]string{"search", "--symbol=AddressLookup", "convert", "street"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("PdokAddressLookupService")) {
		t.Errorf("graph search --symbol output = %q", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli -run TestGraphSearchSymbolFlag -v`
Expected: FAIL — the target is not surfaced (concept words alone don't match; `--symbol` is currently treated as a query word).

- [ ] **Step 3: Parse `--symbol=` and route through `SearchGuided`**

In `internal/cli/graph.go`, replace the `case "search":` block:

```go
	case "search":
		res, err := a.Graph.Search(ctx, wt, strings.Join(rest, " "), "", "", 20)
		if err != nil {
			return err
		}
```

with:

```go
	case "search":
		var syms, qwords []string
		for _, tok := range rest {
			if s, ok := strings.CutPrefix(tok, "--symbol="); ok {
				if s != "" {
					syms = append(syms, s)
				}
				continue
			}
			qwords = append(qwords, tok)
		}
		res, err := a.Graph.SearchGuided(ctx, wt, strings.Join(qwords, " "), syms, "", "", 20)
		if err != nil {
			return err
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/cli -run TestGraphSearchSymbolFlag -v`
Expected: PASS.

- [ ] **Step 5: Confirm the cli package still passes**

Run: `go test ./internal/cli`
Expected: ok (the existing `TestGraphSearch` has no `--symbol`, so all `rest` tokens remain the query).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/graph.go internal/cli/graph_test.go
git commit -m "feat(cli): graph search --symbol candidate flag"
```

---

## Task 6: Docs — deprecate the heavyweight tier, point to query-guided search

**Files:**
- Modify: `README.md` (the semantic-search / Tier B mention)
- Modify: `docs/validation/2026-06-04-search-quality-tiers.md` (top-of-file note)

- [ ] **Step 1: Add a deprecation note to the validation report**

At the top of `docs/validation/2026-06-04-search-quality-tiers.md`, immediately under the first `>` blockquote line, insert:

```markdown
> **SUPERSEDED (2026-06-04):** The Tier B recommendation below is retired. Faster
> models collapse at 60k scale and document enrichment does not help; the win is
> query-side — LLM-supplied `candidate_symbols` lift the dependency-free lexical
> tier from 0% to 46.7% with zero index cost. See
> `docs/superpowers/specs/2026-06-04-query-guided-search-design.md`. The ORT/`vec0`
> code remains in the tree, dormant and unrecommended.
```

- [ ] **Step 2: Update the README semantic-search mention**

In `README.md`, find the section describing semantic / Tier B / `-tags ORT` embedding search and replace its recommendation with a note that the default, recommended path is query-guided lexical search via `search_graph`'s `candidate_symbols`, and that the ONNX embedding tier is deprecated/dormant (kept behind `-tags ORT`, not built or recommended by default). Keep it to 2-3 sentences and link `docs/superpowers/specs/2026-06-04-query-guided-search-design.md`.

Run to locate the text: `grep -n "ORT\|semantic\|embed\|Tier B" README.md`

- [ ] **Step 3: Commit**

```bash
git add README.md docs/validation/2026-06-04-search-quality-tiers.md
git commit -m "docs: deprecate heavyweight embedding tier in favor of query-guided search"
```

---

## Task 7: Final verification — full suite + default static build

**Files:** none (verification only)

- [ ] **Step 1: Build the default (CGO-free) binary**

Run: `go build ./...`
Expected: success, no ORT/native-lib requirement (the dormant tier stays behind `-tags ORT`).

- [ ] **Step 2: Run the entire test suite with the race detector**

Run: `go test -race ./...`
Expected: ok across all packages.

- [ ] **Step 3: Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 4: Re-confirm the benchmark target (if not already captured in Task 4)**

Run:
```bash
RECARTO_BENCH_REPO=/home/claude/Projects/ordia \
RECARTO_BENCH_STORE=/home/claude/recarto-e2e/embed/bench-store.db \
RECARTO_BENCH_NORECONCILE=1 \
go test ./internal/graph -run SearchQualityBenchmark -v 2>&1 | tail -20
```
Expected: semantic recall@k ≥ 46.7%, lexical-control recall@k ≥ 87.5%. Record the numbers in the validation report (append a short "query-guided measured" line under the note from Task 6).

---

## Self-Review notes (for the implementer)

- **Spec coverage:** contract field (Task 2), 4-tier ranking (Task 1: exact-name + split candidate passes fused above the unchanged NL exact/broadener passes), guidance in three touch-points (Tasks 2 schema tag + 3 description + 3 instructions), weak-result hint (Task 2, implemented as the `count==0 && no candidates` trigger — the zero-false-positive subset of the spec's "sparse" definition), Tier B quarantine (Task 6 docs; code already dormant by default), proof-first validation (Tasks 1–5 unit tests + Task 4 benchmark), optional CLI flag (Task 5).
- **Type consistency:** `SearchGuided(ctx, worktree, query string, candidateSymbols []string, kind, lang string, limit int)` is used identically in `internal/mcp/graph.go`, `internal/cli/graph.go`, and `internal/graph/searchbench_test.go`. `candidateExactWeight`/`candidateSplitWeight` are defined once in `search.go`. `SearchGraphInput.CandidateSymbols` ↔ `in.CandidateSymbols` ↔ JSON `candidate_symbols` are consistent.
- **No placeholders:** every code step shows full code; the only prose step is Task 6 Step 2 (README wording), which is documentation copy with an exact `grep` to locate the text.
