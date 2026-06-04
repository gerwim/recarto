# Recarto — E2E Code-Graph Validation Campaign (Ordia) — Design Spec

> Status: **Approved** 2026-06-04. Topic: validate the Recarto code graph for
> correctness and LLM-queryability against a real dual-language codebase (Ordia),
> fix every bug found via TDD, distill findings into committed regression tests,
> and decide the embeddings question with evidence.

## 1. Motivation

Recarto's value for large codebases is: **save tokens, get precise answers**. That only
holds if (a) the graph is *correct* — symbols and edges match the real source — and
(b) an LLM can *query it correctly* through the actual tool surface. This branch
(`fix/codegraph-language-coverage`) just fixed a cluster of language-coverage bugs found
by indexing the Rollout .NET repo (bug-069 C# records/enums/file-scoped namespaces,
bug-070 camelCase FTS, bug-071 cache invalidation, bug-073 the TS/JS family). This
campaign extends that proven method to **Ordia**, which is *dual-language* and therefore
exercises both fix families at once.

**Target codebase — Ordia** (`~/Projects/ordia`): 162 C# files (.NET "Aspire"-style
layered solution: `Domain`/`Application`/`Infrastructure`/`Api`/`AppHost`/`ServiceDefaults`),
44 `.ts` + 54 `.tsx` (Next.js frontend with GraphQL codegen), ~52k LOC across cs+ts+tsx.
(~744 `.js` files exist but most are generated/vendored — see §7.) The clean layered .NET
solution gives a *hard, independent* ground truth for module dependency edges: the
`<ProjectReference>` graph in the `.csproj` files.

## 2. Goals & success criteria (definition of "correct")

| Dimension | Bar | Oracle |
|---|---|---|
| **Completeness** | recarto node count ≈ disk count for every well-defined construct; **100% of top-level declarations** captured per construct. Any systematic miss = bug. | `ripgrep` per-construct counts over the `git ls-files` set |
| **Accuracy** | Stratified sample (~40/language): correct **kind + qualified name + file:line span**; no phantom nodes, no duplicates, correct worktree tag. | serena LSP (Roslyn/tsserver) + source |
| **Edges** | Sampled `contains`/`calls`/`extends`/`implements` resolve to the **correct target node**; cross-file `calls` resolution **rate** measured. | serena LSP references/implementations |
| **Architecture** | `get_architecture` inter-module deps match the **`.csproj <ProjectReference>` graph**. | `.csproj` parse (independent, exact) |
| **LLM grading** | ≥ **90% correct** on factual lookups across the 5 MCP tools; every miss root-caused. Plus **tokens-to-answer (graph) vs reading whole files**. | hand-verified answer key |
| **Embeddings** | FTS recall@k on *semantic* (no lexical overlap) queries. **If recall@10 < ~65%**, trigger the vector spike. | hand-labeled relevant-symbol set |

Thresholds (90% LLM accuracy, ~65% semantic-recall trigger) are the agreed defaults and
may be revised once baseline numbers are in.

## 3. Validation architecture — three independent oracles + recarto

- **Completeness oracle** — `ripgrep` per-construct regexes counted over `git ls-files`
  (mirrors recarto's `GitSource`, which honors `.gitignore`, so the file sets are
  comparable; generated/vendored files excluded explicitly — see §7).
- **Accuracy / edge oracle** — **serena MCP (LSP)**: Roslyn for C# (after
  `dotnet restore/build`), typescript-language-server for TS/TSX. Genuinely independent
  of recarto's tree-sitter, so it cannot share recarto's blind spots.
- **recarto side** — `recarto index` then **direct SQLite reads** of the per-repo cache
  (`~/.cache/recarto/<repo-id>/`) for exhaustive, noise-free node/edge dumps; plus the
  CLI / **MCP** surface for the LLM layer.
- **Comparison engine** — a Python script joins recarto's dump against the oracle
  counts/samples and emits a markdown+JSON discrepancy report into a scratch dir.

**Artifact policy:** campaign scripts and run artifacts live in a **gitignored scratch
dir** (throwaway — Ordia is an external discovery target). Only the distilled fixtures,
Go tests, migrations, and the final report are committed (§5/§8).

## 4. Clean-room LLM grading protocol (Phase 3) — validity guard

**Principle: no agent both knows the ground truth and produces the answers.** Otherwise a
grader that already scanned the codebase answers from memory and gives recarto false
credit — it would measure the agent's recall, not recarto's queryability.

1. **Answer-key construction (full-knowledge side).** The orchestrator + serena/grep/source
   author a fixed question set **Q** (~30 Qs spanning `search_graph` / `get_code_snippet`
   / `trace_call_path` (callers+callees) / `get_architecture` / `query_graph`, plus
   realistic agent tasks) **and** the verified answer key **K**. Questions require
   repo-specific facts (exact symbol names, files, call relationships) so training-data
   priors cannot shortcut them.
2. **Cold grader (recarto-only side).** A **freshly spawned subagent** — cold context, no
   prior turns — whose **only** codebase-access tool is **recarto's MCP server** (pointed
   at Ordia). Explicitly denied Read / Grep / Glob / serena / web. Receives **Q** but not
   **K**.
3. **Anti-contamination audit.** For each question the grader must return: the answer, the
   **exact recarto tool calls it made**, and the **tool output it relied on** (evidence).
   Any answer not grounded in a recarto tool call is **disqualified** (answered from
   priors) — the direct defense against contamination. Evidence also yields the
   token-to-answer measurement.
4. **Grading.** Orchestrator compares grader answers → K: correct / partial / wrong /
   tool-failed. Token accounting: tokens of the grader's recarto tool calls+outputs to
   reach the answer vs the baseline of reading the file(s) that contain the answer.
5. **Root-cause.** Each wrong/partial is classified: **graph bug** (→ Phase 5),
   **tool-ergonomics gap** (description/schema/output shape — only surfaces by driving the
   real MCP), or **genuine recall gap** (→ feeds Phase 4 embeddings).

**Implementation note.** Drive the **real MCP** (the tool descriptions/schemas the agent
sees are part of "can an LLM query correctly"). Register recarto's MCP against the Ordia
working dir. If the harness cannot expose an arbitrary MCP server to a spawned subagent,
fall back to the `recarto graph/recall` CLI as the sole tool (same `Engine.Search`
underneath) — prefer MCP and note the difference in the report.

## 5. Build sequence (test → validate → implement loop)

- **Phase 0 — Setup & baseline.** Build recarto from source (this branch's indexer);
  `recarto version` / `recarto doctor`. `dotnet restore/build` Ordia + activate serena;
  confirm frontend `node_modules`. `recarto index` Ordia; capture index time + raw counts;
  locate the cache DB. *Fallback:* if Roslyn restore fails (restricted network), degrade
  C# accuracy to grep + spot-checks; the TS oracle is unaffected.
- **Phase 1 — Completeness.** Per-construct disk counts vs recarto counts, per language
  (C# / TS / TSX / JS). Investigate every gap → log bugs.
- **Phase 2 — Accuracy & edges.** Stratified symbol sample vs LSP+disk; sampled
  `contains` / `calls` (cross-file resolution rate) / `extends` / `implements`; validate
  `get_architecture` vs the `<ProjectReference>` graph → log bugs.
- **Phase 3 — LLM grading + token efficiency.** Run the §4 clean-room protocol → grade,
  measure token savings → log bugs / tool-ergonomics gaps.
- **Phase 4 — Embeddings eval (+ gated spike).** Semantic query set; measure FTS recall@k.
  If below threshold → build an **offline, lightweight-local** vector spike *first*
  (char-ngram / hashing / TF-IDF — preserves the zero-dependency static-binary
  architecture), re-measure, compare head-to-head. See §6.
- **Phase 5 — Fix loop (TDD) + regression distillation.** Per logged bug: write a failing
  Go test backed by a **minimal fixture under `internal/parse/testdata` (NOT depending on
  Ordia)** → fix → **cache-wipe migration if the indexer/FTS content changed** (bump
  `schemaVersion`; the `migrate_test::TestLatestGraphMigrationRebuildsIndex` invariant
  enforces this) → green → re-run the relevant phase on Ordia to confirm the real
  discrepancy closed. Update `.wolf/buglog.json` and `.wolf/cerebrum.md`.
- **Phase 6 — Report & recommendation.** Commit the campaign report: per-phase metrics,
  bugs found/fixed, perf + token-savings numbers, and the evidence-based embeddings
  recommendation.

## 6. The embeddings decision — the architectural crux

Recarto is a **zero-dependency static binary**. True semantic embeddings imply one of:
(a) an external embedding API — breaks offline / zero-dependency; (b) a bundled local
model — heavy for a static binary; (c) a **lightweight local approximation**
(char-n-gram / feature-hashing / TF-IDF+SVD) — improves fuzzy / morphology / typo / partial
recall but **not** deep semantics. The spike evaluates (c) **first** because it preserves
the architecture. The recommendation will state explicitly which class of query each option
does and does not solve, backed by the recall@k numbers — so the "do we need embeddings?"
answer is evidence-based, not assumed.

## 7. Risks & mitigations

- **File-set mismatch** (recarto's `git ls-files` vs the oracle vs the ~744 `.js`): the
  `.js` are likely generated/vendored (Next build output, codegen). Normalize **both**
  sides to `git ls-files` and explicitly exclude generated paths (`next-env.d.ts`, GraphQL
  codegen output, `.next/`, `dist/`). Confirm the `.js` provenance in Phase 0.
- **Roslyn restore failure** (restricted network): Phase 0 fallback to grep + spot-checks
  for C# accuracy; TS oracle unaffected.
- **Construct-scope ambiguity** ("bug" vs tree-sitter grammar limitation vs intentional
  scope — e.g. local functions, lambdas, anonymous types): decide in/out scope **per
  construct up front** in Phase 1 and record it; only *unintended* misses are bugs.
- **LLM grading nondeterminism**: the *tool outputs* are deterministic even if phrasing
  isn't; fixed Q + hand-verified K + the evidence-grounding requirement (§4.3) keep grading
  objective.
- **Scale**: Ordia is medium (~52k LOC). The token/perf claims target "very large"
  codebases, so include an **optional** index/query-timing cross-check on the larger
  Rollout repo (828 `.cs`) as a scale sanity check — not a gating requirement.

## 8. Deliverables

- **Committed code:** new Go fixtures + tests (`internal/parse`, `internal/graph`)
  reproducing each real bug; any `schemaVersion` migration bump; updated
  `.wolf/buglog.json` and `.wolf/cerebrum.md`.
- **Committed report:** `docs/validation/2026-06-04-ordia-e2e-report.md` — per-phase
  metrics, bugs found/fixed, perf + token-savings numbers, residual known limitations, and
  the embeddings recommendation (with spike results if triggered).

## 9. Out of scope (YAGNI)

Languages beyond Go / C# / TS / JS; a reusable external-codebase harness (chose
committed-tests-only); memory-spine (non-graph) validation; shipping a full embedding model
(only a gated spike + recommendation — productionization is a separate follow-up unless the
spike clearly wins and is cheap).
