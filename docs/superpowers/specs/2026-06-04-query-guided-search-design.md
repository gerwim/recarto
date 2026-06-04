# Recarto — Query-Guided Code Search (retire heavyweight embeddings) — Design Spec

> Date: 2026-06-04. Branch: `fix/codegraph-language-coverage`.
> Supersedes the Tier B semantic-search recommendation in
> `docs/validation/2026-06-04-search-quality-tiers.md`.

## Problem

Recarto's optional semantic tier (Tier B) embeds every symbol with
EmbeddingGemma (300M) through ONNX Runtime behind `-tags ORT`. On the Ordia
corpus (60.7k symbols) it costs **~50 minutes to index** and returns **9–19%
semantic recall@10** — slow, dependency-heavy (native `libonnxruntime` +
`libtokenizers`), and only "so so" in quality. The goal is code search that is
genuinely usable for an LLM agent, indexes in **under 5 minutes**, runs **CPU-only**,
and ideally drops the heavy native dependency entirely.

## Key finding (evidence)

All measured on the real Ordia 60k corpus with the existing
`internal/graph/testdata/searchbench.json` query set (10 zero-token-overlap
*semantic* queries + 4 *lexical-control*), recall@10 / MRR.

| Approach | Index time (60k) | Semantic recall@10 | New deps |
|---|---|---|---|
| EmbeddingGemma vector-only @60k *(current Tier B)* | ~50 min | 19.2% | ONNX + native |
| EmbeddingGemma @ type-level only (~4k docs) | ~4 min | ~32% | ONNX + native |
| Static model (potion-retrieval-32M) @60k | **~8 s** | **0%** | ~none |
| Static + enrichment (signature/comment/body) @60k | ~10–20 s | 0–3% | ~none |
| **LLM query-expansion → lexical Tier A** (BM25 proxy) | **0** | **46.7%** | **none** |
| LLM query-expansion → static vector | ~8 s | 31.7% | ~none |

Three conclusions:

1. **Swapping in a faster model is a dead end.** Static embeddings index 60k in
   8 s (~340× faster) but recall collapses to 0% at scale, and **document
   enrichment does not rescue them** — signature, leading comment, and body
   snippet variants all stayed ~0%. A context-free model cannot bridge
   concept→code amid 53k distractors regardless of what it is fed.
2. **Only a heavyweight contextual model gives real vector recall, and even then
   it is modest** (19% at 60k; ~32% if restricted to the ~4k type-level symbols).
   This speed/quality tension cannot be engineered away on the index side.
3. **The query side breaks the tension.** Rewriting the semantic queries the way
   an LLM reasoning about a codebase naturally would — the concept **plus likely
   identifiers** ("authorization permission handler", "route optimization",
   "address lookup geocode") — lifted the dependency-free lexical tier from **0%
   → 46.7%**, beating EmbeddingGemma's 19% with **zero index cost** and ~1 ms
   queries. That figure is conservative: the BM25 proxy lacks Tier A's
   porter/trigram/synonym broadeners and dedup-by-name, worth ~12 more points per
   the prior report.

The insight: **the consumer of recarto's search is an LLM we instruct via the
MCP tool contract and the hooks/prompts we own.** Rather than pay a 50-minute
heavyweight model to guess what code *means*, let the LLM — which already
understands the concept — state what to look for, and feed that to the cheap,
instant, dependency-free lexical engine.

## Goals

- Add an LLM-driven query path: `search_graph` accepts candidate identifier
  names alongside the natural-language concept.
- No new dependency on the default build; no extra indexing cost.
- Reach semantic recall@10 ≥ 46.7% (the BM25-proxy figure; higher is better) on
  the existing benchmark — far past the retired Tier B's 19.2% — with no
  regression on lexical-control (must stay ≥ 87.5%).
- Behaviour proven by a committed benchmark, not a Python estimate.

## Non-goals

- Improving or replacing the embedding model (the heavyweight tier is retired,
  not improved).
- Cross-encoder re-ranking, multilingual support, or server-side LLM calls.
- Removing the dormant Tier B code (it is quarantined, not deleted — see below).

## Design

### 1. Contract — `search_graph` gains an optional `candidate_symbols`

```
search_graph({
  query: "convert a street into map coordinates",     // concept, natural language
  candidate_symbols: ["AddressLookup", "Geocode", "Coordinate"]  // optional
})
```

`candidate_symbols` is the LLM's best guesses at the identifier names it expects
to exist. It is **optional**: omitted, `search_graph` behaves exactly as today
(full backward compatibility — no regression path).

**Consumer ergonomics (the field is only worth adding if the LLM uses it well):**

- **Clear division of labor** is the single biggest adoption lever — if it is
  fuzzy, the model treats the field as redundant with `query` and skips it. The
  description states: `query` = the concept in plain natural language;
  `candidate_symbols` = the identifier names you would bet exist in the code.
- **Lenient input, documented as such** so the model does not overthink format.
  Any surface form is accepted and identifier-split + matched leniently:
  PascalCase (`AddressLookup`), interface-prefixed (`IAddressLookupService`),
  snake_case (`address_lookup`), plain phrases (`address lookup`), and
  partial or qualified names.
- **Soft cap of ~3–8 guesses** stated in the description. Unconstrained, the
  model over-generates many weak guesses that dilute precision; the cue keeps the
  candidate set high-signal.

The structured field (rather than stuffing terms into `query`) is load-bearing
because guessed identifiers and concept prose want *different* retrieval
strategies: identifiers want exact/prefix precision, the concept wants fuzzy
broadeners. Mixing them into one BM25 bag let concept words bury the real
identifier (observed in the proxy: "address lookup" sank under 60k docs
containing "service"/"address"). Separation also makes the behaviour
deterministically testable.

### 2. Ranking tiers

`Engine.Search` gains a `candidateSymbols []string` parameter and runs, in
priority order, fused via the existing `fuseResults` (weighted RRF) and
`dedupByName`:

1. **Exact name-equality on candidates** — `n.name = ? COLLATE NOCASE` for each
   candidate symbol. A direct hit is the LLM's targeted guess landing exactly;
   it takes the top slot(s). Highest weight.
2. **Split-token FTS on candidates** — `ftsPass(nodes_fts, orMatch(candTerms))`
   where `candTerms = baseTerms(join(candidate_symbols))` (identifier-split,
   stop-worded). Catches near-misses ("AddressLookup" vs `IAddressLookupService`).
   Weight above the NL exact pass.
3. **NL concept exact pass** — today's `nodes_fts` primary over `baseTerms(query)`.
4. **NL broadeners on underfill** — today's porter / synonym / trigram passes.

Tiers 3–4 are unchanged from the current `lexicalSearch`. Tiers 1–2 are new and
only fire when `candidate_symbols` is non-empty, so keyword-only and
lexical-control queries are byte-for-byte unaffected.

### 3. Guidance — teach the LLM to fill the field

Three coordinated touch-points (the field is dead weight if nobody is told to
fill it):

- **Tool description** (`internal/mcp/server.go`): expand the `search_graph`
  blurb to instruct the model to pass `candidate_symbols` with the identifier
  names it would expect when searching by concept.
- **Field schema tag** (`internal/mcp/graph.go`): a `jsonschema` description on
  `CandidateSymbols` — the field name + description sitting in the agent's tool
  schema is the strongest, always-present teacher.
- **Instructions block** (`internal/agents/instructions.go`): one line in the
  managed `# Recarto` block written to every host's instructions file, e.g.
  "When searching by concept, pass `candidate_symbols` — the identifier names you
  expect (e.g. `AddressLookup`, `Geocode`)."

### 4. Weak-result hint (closes the agent loop, no ML)

`SearchGraphOutput` gains an optional `hint string`. When a search returns
sparse results **and** `candidate_symbols` was empty, recarto sets:

> "No strong matches. Retry with `candidate_symbols` set to the identifier names
> you'd expect for this concept."

"Sparse" is deterministic: `count == 0`, or `count < 3` with the top score below
a small constant. This nudges the agent to use the new channel without any
server-side model.

### 5. Tier B disposition — quarantine, do not delete

The heavyweight tier is no longer recommended or part of the default story, but
the research is preserved:

- The `-tags ORT` embedder (`embed_hugot.go`) and the `vec0` storage stay in the
  tree, dormant. The default build already never sets an embedder, so the default
  query path is lexical-only with no behaviour change beyond the new tiers.
- `README.md` and `docs/validation/2026-06-04-search-quality-tiers.md` get a note
  marking Tier B deprecated in favour of query-guided search, with a one-line
  pointer to this spec.
- No deletion: un-deleting validated research is expensive; deleting later is
  cheap.

## Components / files to modify

| File | Change |
|---|---|
| `internal/mcp/graph.go` | `SearchGraphInput += CandidateSymbols []string` (+ schema tag); pass to `Search`; `SearchGraphOutput += Hint string`; populate hint. |
| `internal/mcp/server.go` | Expand `search_graph` tool description. |
| `internal/graph/search.go` | `Search(... candidateSymbols []string ...)`; add candidate name-eq + split-token passes; fuse above NL tiers. |
| `internal/graph/expand.go` | Reuse `baseTerms`; add a candidate name-eq query helper if needed. |
| `internal/agents/instructions.go` | One coaching line in the `Instructions` block. |
| `internal/cli/graph.go` | Optional: `--symbol` (repeatable) flag on `graph search` for parity/manual testing. |
| `internal/graph/searchbench_test.go` + `testdata/` | Add the candidate-symbol (HyDE) query variant; assert recall targets. |
| `README.md`, `docs/validation/2026-06-04-search-quality-tiers.md` | Mark Tier B deprecated; point to this spec. |

`Search`'s signature change ripples to its callers (`internal/mcp/graph.go`,
`internal/cli/graph.go`, existing tests); they pass `nil` where they have no
candidates, preserving behaviour.

## Data flow

```
LLM agent
  └─ search_graph{ query, candidate_symbols }
       └─ Engine.Search(worktree, query, candidateSymbols, kind, lang, limit)
            ├─ tier 1: exact name-equality on candidate_symbols      (top weight)
            ├─ tier 2: split-token FTS on candidate_symbols          (high)
            ├─ tier 3: NL exact pass over query                      (today)
            └─ tier 4: NL broadeners (porter/synonym/trigram)        (today, on underfill)
       → fuseResults(...) → dedupByName → truncate(limit)
       → SearchGraphOutput{ results, hint? }
```

## Testing / validation (proof-first)

Written before implementation, per the project's TDD mandate:

1. **Unit — candidate boost:** given a corpus where symbol `Foo` exists, a search
   with `candidate_symbols:["Foo"]` and an unrelated NL `query` ranks `Foo`
   first. Deterministic, no model.
2. **Unit — backward compatibility:** `candidate_symbols` empty/nil produces
   byte-identical results to the current `Search` for the same query (guards
   against any regression on the existing lexical path).
3. **Unit — hint:** a concept query with no candidates and zero results sets
   `Hint`; a healthy result or a query that already passed candidates does not.
4. **Benchmark — the headline claim:** extend `searchbench_test.go` with a
   candidate-symbols variant of the 10 semantic queries (a `candidate_symbols`
   array per query in `testdata/searchbench.json`). Gated by `RECARTO_BENCH_REPO`,
   it must show, on the **real** Tier A engine:
   - semantic recall@10 **≥ 46.7%** (the BM25-proxy figure) — higher is better,
     and the real Tier A (porter/synonym/trigram broadeners + dedup-by-name) is
     expected to meet or exceed the proxy, so this is a floor to push past, not a
     ceiling;
   - lexical-control recall@10 **≥ 87.5%** (no keyword regression — hard
     guardrail).
5. **Validation report:** refresh `docs/validation/2026-06-04-search-quality-tiers.md`
   with the measured query-guided numbers from the real engine.

## Rollout

1. Tests (1–3) + ranking tiers + `candidate_symbols`/`hint` plumbing — default build.
2. Guidance: tool description, schema tag, instructions block.
3. Benchmark (4) on the Ordia store + validation report (5).
4. Quarantine Tier B in docs.

## Risks / mitigations

- **LLM omits `candidate_symbols`** → falls back to today's NL behaviour (no
  regression); the weak-result hint nudges a retry.
- **Hallucinated / wrong candidates** → tier-1 name-equality simply returns
  nothing; tier-2 split tokens add mild noise bounded by dedup-by-name and the
  NL tiers still running underneath.
- **Over-boosting candidates buries good NL hits** → NL tiers remain in the
  fusion; candidate tiers lead but do not replace. The benchmark's
  lexical-control floor (≥ 87.5%) is the guardrail.
```
