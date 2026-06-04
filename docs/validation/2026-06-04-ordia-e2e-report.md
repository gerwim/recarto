# Recarto Code-Graph E2E Validation — Ordia — Report

> Date: 2026-06-04. Branch: `fix/codegraph-language-coverage`. Method:
> `docs/superpowers/specs/2026-06-04-ordia-e2e-graph-validation-design.md`,
> plan `docs/superpowers/plans/2026-06-04-ordia-e2e-graph-validation.md`.
> Target: **Ordia** — a dual-language codebase (162 C# files in a 7-project .NET
> "Aspire" solution + a 94-file Next.js/TypeScript frontend), ~52k LOC.

## TL;DR

- **Symbol extraction is correct and complete** on Ordia: recarto's nodes match
  two *independent* oracles (Roslyn for C#, the TypeScript compiler API for TS/JS)
  **exactly** across all in-scope constructs (0 missing, 0 phantom, 0 duplicate, 0
  line mismatches). The earlier C#/TS coverage fixes (bug-069/073) hold up on a
  second real codebase.
- **The bugs were all in EDGES, not symbols.** Three were found and fixed (TDD +
  cache-wipe migration v7), one documented as an inherent limitation.
- After the fixes, a **cold, recarto-only LLM grader answered 20/20 questions
  correctly** across all five MCP tools, using **14×–227× fewer tokens** than
  reading the underlying files.
- **Embeddings: not needed yet.** Lexical FTS is perfect for keyword search and
  useless for zero-overlap semantic queries; the cheaper fix is relaxing FTS
  AND-matching, not shipping a model.

## 1. Completeness (Phase 1) — PASS, no bugs

Independent oracle vs recarto, over the canonical `git ls-files` set (generated/
vendored excluded; the 2 `.js` files are config `.mjs`, no app JS):

| bucket | result |
|---|---|
| C# (9 kinds: class/interface/struct/record/enum/enum_member/delegate/method/constructor/property/namespace) | **all deltas 0, set-diff 0** across 162 files |
| TS/TSX (function/method/class/interface/enum/type/namespace) | match; only a `function` -19 explained by TS overload signatures (recarto stores 1 node/name; oracle counts each overload AST node) — unique-key diff 0 |

Out-of-scope constructs (C# event/operator/indexer, TS get/set accessors) have **0
occurrences in Ordia**, so coverage matches Ordia's full surface. (Those kinds are a
documented residual for *other* codebases, not a finding here.)

## 2. Accuracy & edges (Phase 2)

Symbol accuracy: **2158/2159 line-checked, 0 line mismatches, 0 duplicate nodes.**
The edge layer is where the bugs were:

| # | Finding | Disposition |
|---|---|---|
| **A** | `Resolve()` matched `dst_name` against *any* node in the worktree (`LIMIT 1`, no language scoping). **45 C#→`tsx` "calls" all bound to a React `Select` component** (every C# LINQ `.Select`). Polluted `trace_call_path` and produced a bogus `api-gateway→frontend` architecture edge. | **FIXED** (bug-074) |
| **B** | `qualified_name == name` for 100% of nodes (never qualified) → **43.6% of resolved calls had an ambiguous target**; first-match picked arbitrarily. | **FIXED** (bug-075) |
| **C** | C# interface-implementation emitted as `extends`, never `implements` (tree-sitter `base_list` is just identifiers; separating base class from interface needs semantics). | **DOCUMENTED limitation** |
| **D** | `get_architecture` grouped by first path segment → all 7 nested C# projects collapsed into one `api-gateway` bucket; the layered architecture was invisible. | **FIXED** (bug-076) |

Non-bugs confirmed (expected behavior): `imports` 0% resolved (import targets are
package/path strings, not symbol nodes); file-scope/callback calls have `NULL` src;
`contains` edges carry an empty `dst_name` by design.

## 3. Fixes implemented (TDD) — Phase 5

Commits `59b707b`, `5cc7b30`, `374fee7` (this branch). Full suite + `-race` + `vet`
+ `gofmt` green; new regression tests in `internal/graph/{index,resolve,architecture}_test.go`.

- **A — language-family-scoped resolution** (`resolve.go`): resolution restricted
  to the source file's language family (`{csharp}`/`{go}`/`{typescript,tsx,javascript}`),
  preferring same-file then same-top-level-dir then a stable id order.
- **B — real `qualified_name`** (`index.go`): built at index time from the
  structural context (`namespace.class.member`), feeding the node FTS. **C#
  file-scoped namespaces** are handled specially — verified via a tree-sitter
  s-expression probe that their span does *not* contain the following types
  (unlike block namespaces), so they are applied as a preceding-namespace prefix.
- **D — project-aware architecture** (`architecture.go`): modules group by the
  nearest `.csproj`/`package.json`/`go.mod` directory (basename, disambiguated to
  the full relative path on collision), falling back to the first path segment.
- **Migration v7** wipes the rebuildable graph so existing caches rebuild on upgrade
  (the standing convention, enforced by `migrate_test.go::TestLatestGraphMigrationRebuildsIndex`).

**Validation after re-indexing Ordia:** cross-language resolved calls **45 → 0**;
`qualified_name == name` **100% → 38%** (the remainder are genuinely top-level
symbols/namespaces); `get_architecture` now shows the real 7 projects + frontend
with the correct `Api→Application→Domain` / `Infrastructure→Application→Domain`
layering and **no** backend→frontend edge.

## 4. LLM query grading (Phase 3) — 20/20 correct

A **fresh, codebase-blind subagent restricted to the recarto CLI** (no Read/Grep/
serena/web; every answer required to cite the recarto tool output — the
anti-contamination control) answered **20/20** questions across `search_graph`,
`query_graph`, `get_architecture`, `get_code_snippet`, and `trace_call_path`.
Several answers (`backend depends on frontend? → No`; the 7-project layering;
project-scoped `query record Ordia.Domain`; correct callers/callees) **would have
been wrong before the Phase 5 fixes** — end-to-end validation through the real tool
surface.

**Token efficiency** (recarto vs reading the underlying file(s)):

| task | recarto | whole-file | savings |
|---|---:|---:|---:|
| locate a symbol | 46 tok | 828 tok | **18×** |
| list a project's interfaces | 388 tok | 5,384 tok | **14×** |
| architecture overview | 204 tok | 46,544 tok | **227×** |
| callees of a function | 19 tok | 2,491 tok | **131×** |
| code snippet (tiny file) | 299 tok | 344 tok | 1.2× |

This is the "save tokens, get precise answers" value prop, quantified — the payoff
grows with codebase size.

## 5. Embeddings decision (Phase 4) — not needed yet

On 10 natural-language queries with **verified zero identifier-token overlap** with
their targets, and 4 lexical-control queries:

| index | semantic recall@10 | lexical-control recall@10 |
|---|---:|---:|
| recarto FTS (bm25, AND-matched) | **0%** | 100% |
| offline char-trigram + TF-IDF spike | **0%** | 100% |

FTS is perfect for keyword search and returns "(no matches)" for multi-word NL
queries (it ANDs every term). The lightweight-local spike never returns empty and
ranks partial/morphological matches (it even surfaced relevant symbols the test key
under-listed), but on true synonyms (`street`→`Address`, `drive`→`Travel`) it also
scores 0% — char/TF-IDF cannot bridge meaning.

**Recommendation:**
1. **Relax FTS from AND to OR + bm25 ranking** (and reuse the memory layer's existing
   `synonyms.txt`). Cheapest, highest-value, no new dependency, preserves the
   zero-dependency static binary. Do this first.
2. **Skip local vectorization** — it only adds fuzzy matching over OR-FTS and does
   nothing for synonyms.
3. **True semantic embeddings only if NL/synonym search becomes a hard requirement** —
   only a real model bridges zero-overlap synonyms, and it costs the static-binary
   property (bundled model or external API). Defer to an opt-in follow-up; if shipped,
   make it an *optional* index, not the default.

## 6. Bugs found & fixed

| id | summary | file | status |
|---|---|---|---|
| bug-074 | cross-language/family edge mis-resolution (45 C#→React) | `internal/graph/resolve.go` | fixed `59b707b` |
| bug-075 | `qualified_name` never populated → ambiguous resolution | `internal/graph/index.go` | fixed `59b707b` |
| bug-076 | `get_architecture` module granularity too coarse | `internal/graph/architecture.go` | fixed `5cc7b30` |

## 7. Residual known limitations

- **C# `extends`/`implements` collapse** (finding C): all C# base types emit
  `extends`; separating interfaces needs a semantic model. A `trace`/`query` for "what
  implements `IFoo`" won't find C# implementers via an `implements` edge.
- **Within-monorepo same-name ambiguity** (finding B tail): identically-named methods
  across C# projects can still mis-resolve when neither is in the caller's file or
  top-level directory (all share the `api-gateway/` segment). Mitigated by the
  same-file/same-dir preference and the now-visible `qualified_name` context; a deeper
  same-project preference was deferred (would need project roots threaded into the
  resolve SQL). Observable as a few backward `Infrastructure→Api` arch edges.
- **FTS AND-matching** (finding from Phase 4): multi-word queries that lack a full
  match return nothing — see the §5 recommendation.
- **Scale**: Ordia is medium (~52k LOC); `recarto index` ran in ~2.4s. A larger-repo
  timing cross-check (the optional Rollout run) was not performed.
