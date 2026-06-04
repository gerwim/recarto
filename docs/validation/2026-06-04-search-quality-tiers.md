# Recarto Graph Search Quality — Tier A (lexical) + Tier B (semantic) — Report

> Date: 2026-06-04. Branch: `fix/codegraph-language-coverage`. Target: **Ordia**
> (162 C# + 94 TS/Next.js files, ~60.7k indexed symbols). Instrument:
> `internal/graph/searchbench_test.go` (`RECARTO_BENCH_REPO`-gated), query set
> `internal/graph/testdata/searchbench.json` — 10 zero-token-overlap *semantic*
> queries + 4 *lexical-control* queries, scored recall@10 / MRR / zero-hit.

> **SUPERSEDED (2026-06-04):** The Tier B recommendation below is retired. Faster
> models collapse at 60k scale and document enrichment does not help; the win is
> query-side — LLM-supplied `candidate_symbols` lift the dependency-free lexical
> tier from 0% to a measured **57.5% semantic recall@10** (lexical-control held at
> 87.5%) on the full Ordia corpus, with zero index cost. See
> `docs/superpowers/specs/2026-06-04-query-guided-search-design.md`. The ORT/`vec0`
> code remains in the tree, dormant and unrecommended.

## TL;DR

| config | new deps | lexical-control recall@10 / MRR | semantic recall@10 |
|---|---|---|---|
| baseline (OR + bm25) | none | 75.0% / 0.65 | 0% |
| **Tier A (broadeners + name-dedup)** | **none** | **87.5% / 0.71** | 0% |
| Tier B vector-only @60k (EmbeddingGemma) | — | 62.5% / 0.44 | 19.2% (ceiling) |
| Tier B hybrid @60k, naive append fusion | hugot/ONNX + native libs | 87.5% / 0.71 | **0%** (crowd-out bug) |
| **Tier B hybrid @60k, reserved-slot fusion (opt-in)** | hugot/ONNX + native libs | **87.5% / 0.71** | **9.2%** |

> Numbers above are the **full 60.7k-symbol Ordia corpus** with EmbeddingGemma
> (the earlier 3.3% was bge-small; the 32.5% in "Model research" below is
> *vector-only on a 4,128-symbol sample* — semantic recall falls steeply with
> corpus size, so the 60k full-index figure is the realistic one).

- **Tier A is the clear, free win.** An exact unicode61 primary pass + porter /
  synonym / trigram **broadeners on underfill**, plus **dedup-by-name** on the
  result page, lift lexical-control **75% → 87.5%** with **zero new dependencies**.
  Dedup was the key move: collapsing a same-name cluster (e.g. many
  `formatTravelTime` overloads) frees page slots for the actual targets
  (`travel time` recall 50% → 100%). It never regresses (exact pass stays primary)
  and adds morphology / abbreviation / substring recall (unit-tested).
- **Tier B (semantic) adds modest measurable recall at real cost — so it is
  OPT-IN, not shipped by default.** Symbol embeddings live in a `sqlite-vec`
  `vec0` index (pure-Go, *inside the existing modernc SQLite store — no new core
  dependency*); the model runs via hugot/ONNX Runtime behind the `ORT` build tag.
  On the **full 60.7k-symbol** corpus, EmbeddingGemma's **vector tier in isolation
  scores 19.2%** semantic recall@10 (the realistic ceiling at this scale — recall
  decays with corpus size, see "Known costs"). On this **worst-case** benchmark
  (queries share *zero* identifier tokens with their targets, exact-name recall@10)
  that is the value the model can add over pure lexical's 0%.
- **A fusion bug nearly erased that value, and the fix is the interesting result.**
  The first hybrid (lexical-first `append(lex, vec…)` + dedup-by-name) scored **0%
  semantic** at 60k — *worse than vector-only*. Cause: for a zero-overlap query the
  lexical pass returns a **full page of weak token matches** (`options`, `state`,
  `pending`), leaving the vector tier **zero slots**. The fix is a **reserved-slot
  fusion** (`mergeHybrid`): lexical fills the non-reserved majority and keeps all
  top positions (so keyword queries never regress — lexical-control held at exactly
  87.5%), but a **2/5 tail is reserved for the vector tier**, so concept hits like
  `AppointmentStatus`/`AlternativeSlots`/`SaveChangesAsync` survive. That lifts
  hybrid semantic **0% → 9.2%** with **no lexical regression**. (It stays below the
  19.2% vector-only ceiling because the remaining targets are *weak* vector hits at
  KNN rank 8–10 — promoting those would require surrendering the page to vectors
  and sacrificing lexical.) Query latency ~5–7 ms. Cost: ~24 transitive module
  requires under `-tags ORT`, native `libonnxruntime` + `libtokenizers`, and a
  one-time ~50-minute embed of a 60k-symbol repo (incremental after).
- **Recommendation: ship Tier A always; keep Tier B opt-in** (`-tags ORT`). At 60k
  scale Tier B adds a real but **modest** +9.2% semantic recall on the adversarial
  benchmark (more on realistic queries that share some tokens) — valuable but not
  "tremendous", so it does not justify forcing ONNX/native libs + a ~50-min index
  on every user. The default binary stays lexical-only, lean, and statically linked.

## Model research

Vector-only recall on a 4,128-symbol Ordia sample, embedding `kind +
identifier-split name + qualified name + path words` per symbol against the
natural-language query (each model with its correct pooling + prompts):

| model | size / dim | semantic recall@10 | MRR | lexical-control | query latency | CPU-practical |
|---|---|---:|---:|---:|---:|:--:|
| **EmbeddingGemma** | 308M / 768 | **32.5%** | **0.42** | 100% | ~40 ms | ✅ **winner** |
| jina-code-embeddings-0.5b | 0.5B / 896 | 39.2% | 0.24 | 100% | ~800 ms | ❌ too slow |
| CodeRankEmbed | 137M / 768 | 26.7% | 0.25 | 100% | ~47 ms | ✅ |
| bge-small-en-v1.5 | 33M / 384 | 19.2% | 0.27 | 87.5% | ~5 ms | ✅ fastest |
| all-MiniLM-L6-v2 | 22M / 384 | 19.2% | 0.10 | 87.5% | ~7 ms | ✅ |
| jina-embeddings-v2-base-code | 137M / 768 | 13.3% | 0.20 | 62.5% | ~15 ms | ✅ |
| Qwen3-Embedding-0.6B | 0.6B | not run (0.5–0.6B class is ~800 ms) | — | — | ~800 ms | ❌ |
| CodeSage-small-v2 | 130M | untestable (remote code vs transformers 5.x) | — | — | — | — |

(EmbeddingGemma/bge/all-MiniLM/jina-base-code measured via ONNX+hugot;
EmbeddingGemma/jina-code/CodeRankEmbed via sentence-transformers — EmbeddingGemma
agreed across both harnesses at 32.5%.)

**Findings:**
1. **Code-specialized models UNDER-perform a strong general model here**
   (`jina-embeddings-v2-base-code` 13.3%, `CodeRankEmbed` 26.7%, vs EmbeddingGemma
   32.5%). Reason: recarto's "documents" are *symbol-name texts* (identifier-split
   name + qualified name + path), **not code bodies** — code-tuned models expect
   actual code; a general model matches names/concepts better. (This reverses the
   naïve "use a code model" assumption — twice.)
2. **The 0.5–0.6B models are CPU-impractical.** `jina-code-embeddings-0.5b` has the
   best quality (39.2%) but runs at **~800 ms/query / 733 ms/doc** on CPU (~12 h to
   index 60k) — disqualified for the "runs on cheap hardware" goal. Qwen3-0.6B is
   the same class.
3. **EmbeddingGemma (Gemma-3-based, 308M, on-device) wins among practical models** —
   highest semantic recall AND best MRR (ranks hits nearest the top) at ~40 ms. It
   is **prompt-asymmetric** (docs `title: none | text:`, queries `task: search
   result | query:`) and ships **external-data ONNX**; both are handled. Its int8
   QAT variant holds quality (32.5% / MRR 0.37) and is the ARM/phone deployment
   choice — though on this x86 CPU (no VNNI) fp32 is actually faster.

**Recommended default: `EmbeddingGemma` (onnx-community/embeddinggemma-300m-ONNX).**
The embedder is fully configurable (model id / onnx file / prompts / external-data
/ dims), so bge-small (fastest) or any of the above remain selectable.

Pure-Go (no ONNX Runtime) was measured at **148 ms/doc** — far too slow — so the
semantic tier uses hugot's **ORT backend** (native `libonnxruntime` +
`libtokenizers`), opt-in via `-tags ORT`.

## Architecture

- **Tier A** lives entirely in FTS5: `nodes_fts` (exact, unicode61) is the primary;
  migration **v8** adds `nodes_fts_stem` (porter) and `nodes_fts_tri` (trigram) as
  broadeners. `Search` runs the exact pass, returns it untouched when it fills the
  page, else fuses the broadeners under it (`fuse.go`, weighted RRF). Query terms
  are identifier-split + stop-worded (`expand.go`); synonyms come from a graph-local
  `synonyms.txt`.
- **Tier B** is `vec0` via `modernc.org/sqlite/vec` — **the same SQLite driver
  recarto already depends on**, so vector *storage* adds no new module dependency
  and keeps the static binary. `Embedder` is an injected interface
  (`SetEmbedder`); only embeddable kinds (class/interface/method/…) are vectorized,
  to cut distractors and index time. Embedding is best-effort: a model error falls
  back to lexical.
- **Fusion is `mergeHybrid` (reserved-slot), not RRF or naive append.** Lexical
  fills the first `limit − ⌈2·limit/5⌉` slots (keeping every precise hit and all
  top positions → no keyword regression); the vector tier then fills a reserved
  `2/5` tail with concept matches lexical missed; any reserve the vector tier
  leaves unused is backfilled with more lexical. Dedup is by name throughout. This
  replaced an earlier `dedupByName(append(lex, vec…))` that let a full page of weak
  lexical token matches starve the vector tier (semantic recall 0% at 60k); the
  reserve guarantees the semantic tier a foothold (→ 9.2%) without ever displacing
  a precise lexical match. A pure weighted-RRF fusion was rejected earlier because
  vector "cousins" displaced exact lexical hits (lexical regressed to 50%).
- The **hugot/ONNX embedder** (`embed_hugot.go`) is the *only* file importing
  hugot and is behind `//go:build ORT`. The default build is lexical-only and
  dependency-free; semantic search is `go build -tags ORT` plus the two native
  libraries.

## Regression tests committed

`vector_probe_test.go` (vec0 text-PK + worktree-metadata KNN through the modernc
store), `vector_test.go` (a deterministic concept-space fake `Embedder` proving a
zero-token-overlap query is found, that exact queries still rank first under
hybrid fusion, and that non-embeddable kinds are skipped), `search_test.go`
(stemming / synonym-fallback / trigram-substring), `expand_test.go`, and the
`searchbench_test.go` instrument above. All pass in the default build with no
ONNX/transformer dependency.

## Known costs / follow-ups

- Building `-tags ORT` adds hugot + its transitive tree (~24 module requires) and
  needs `libonnxruntime.so` + `libtokenizers.a`. The default build and the
  zig-cc cross-compile are unaffected (the tagged file is excluded).
- Semantic recall@10 falls as the distractor set grows: **42% @637 → 19.2%
  @4,128 → 19.2% @60.7k vector-only** for EmbeddingGemma (the curve flattens but the
  *exact-key* hit rate stays low at scale). Embedding only searchable kinds, the
  reserved-slot hybrid, and a future re-rank of the fused top-k mitigate this; a
  larger model would raise it but is CPU-impractical (see Model research).
- The reserved-slot hybrid recovers the **strong** vector hits (KNN rank ≤5) but
  not the weak ones (rank 8–10), which is why hybrid (9.2%) sits below vector-only
  (19.2%). A cross-encoder re-rank of the union of both tiers' top-k is the obvious
  next lever if semantic recall needs to go higher.
- One-time index embedding of a 60k-symbol repo is **~50 min on CPU** with
  EmbeddingGemma (fp32, ~59 ms/doc, incremental afterward); query latency stays
  single-digit ms. A smaller model (bge-small, ~5 ms/doc) cuts indexing ~10× at
  lower recall, and remains selectable.
