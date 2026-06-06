# Recarto — LLM-Facing API Improvements

**Date:** 2026-06-06
**Status:** Approved design

## Summary

Nine improvements to Recarto's public, LLM-facing surface — the MCP tools, the
hook-injected context, and the `AGENTS.md` instructions block — driven by a scan
of the API from the standpoint of the model that consumes it. The changes fall
into one near-bug (a `kind` schema that contradicts a working feature) and eight
ergonomic gaps that cost the consuming model extra round-trips, silent dead-ends,
or guessed-at inputs.

They are **nine independent changes grouped into seven work units (A–G)**. Each
unit is self-contained and can be implemented, tested, and shipped on its own.

**Decisions locked during brainstorming:**
- **Scope:** all nine items, including the docs-only rewrite.
- **Compatibility:** break the live MCP wire contract freely (pre-1.0). Stored
  data is untouched except where a migration is explicitly called out (unit D).
- **Memory mutation:** add `forget_memory` only (no in-place `update_memory`).
- **Recall metadata:** include both `matched_on`/`min_score` and
  `truncated`/`budget_used` now.

---

## Unit A — Schema hardening: `kind` + closed-set enums

**Problem.** `RecordMemoryInput.Kind` (`internal/mcp/handlers.go:33`) documents the
kinds as `"preference, learning, decision, do_not_repeat, bug, or action"` — it
**omits `todo`**, which is a valid kind (`internal/memory/record.go:14`,
`validKinds`) and is actively promoted by the tool-level description
(`internal/mcp/server.go:19`). The field doc and the tool doc disagree. Separately,
every closed-value field across the MCP surface is a free-text string with the
allowed values buried in prose — there are zero JSON-Schema `enum`s — so the model
guesses and clients cannot reject typos.

**Change.**
- Correct the `kind` field documentation to include `todo`.
- Promote closed-value fields to real JSON-Schema `enum`s:
  - `record_memory.kind` → the 7 values in `validKinds`.
  - `trace_call_path.direction` → `callers`, `callees`.
  - `query_graph.edge_direction` → `in`, `out`.
- Left as free strings (open-ended across parsers/languages): `language`, node
  `kind` filters, `edge_kind`.

**Implementation risk to verify first.** Confirm whether the go-sdk's `jsonschema`
struct-tag reflection emits an `enum` keyword. If it does, express the enum in the
tag. If it does not, set the enum on the generated `InputSchema` explicitly in the
`addTool` registration (`internal/mcp/server.go:60`). Either way the served schema
must actually carry the enum.

**Test.** Assert the served tool schema for `record_memory` contains an `enum`
listing all 7 kinds (and that it includes `todo`); same for the two graph enums.

---

## Unit B — `forget_memory` tool

**Problem.** The API can record, reinforce, supersede (`supersedes_id`), and
`complete_todo`, but there is **no way to delete or correct a memory recorded in
error**. The only path to fix a wrong memory is recording a replacement. The user's
own memory protocol calls for deleting memories that turn out to be wrong.

**Change.** Add an MCP tool:

```
forget_memory(id: string, reason?: string) -> { forgotten: bool }
```

Backed by a new `Engine.Forget(ctx, id, reason)`:
- **Soft-delete:** set the row's `status` to a new non-active value `'forgotten'`
  and remove it from the lexical FTS index via the existing `lex.Delete`
  (`internal/memory/lexical.go:39`).
- Soft (not hard) delete because recall already filters `status != "active"`
  (`internal/memory/recall.go:109`), so a forgotten memory stops surfacing with no
  new filter; the snapshot/sync path stays consistent; and the act is auditable
  (`reason` retained).
- No-op returning `forgotten:false` when the id is unknown or already inactive —
  mirrors `complete_todo`'s no-op contract (`internal/mcp/handlers.go:146`).

**Status taxonomy.** `'forgotten'` joins the existing `active`/`done`/`superseded`/
`archived` set. It is distinct from `archived` (faded via decay) and `superseded`
(replaced by a newer memory): `forgotten` means **explicitly removed as wrong**.

**Tests.** Forget round-trips (`forgotten:true` then a subsequent recall/list
excludes it; the FTS index no longer matches its text); forgetting an unknown id
returns `forgotten:false`; forgetting an already-`done` todo returns `false`.

---

## Unit C — `get_code_snippet` shape + empty-result convention

**Problem.** `get_code_snippet` returns a bare `snippet` string and turns a
not-found symbol into a Go **error** (`internal/graph/snippet.go:21`), while sibling
tools like `get_memory` return a clean `found:false`. The output also lacks the
line range and qualified name, so the model cannot cite `file:line` without
stitching in a prior search result.

**Change.**
- `Snippet` returns a struct: `{ found, start_line, end_line, snippet, qualified_name }`.
  The line range is already stored on the node (`start_line`/`end_line`,
  `internal/graph/index.go:158`); the query simply selects them.
- A symbol that is not found becomes `found:false` with empty fields — **not** an
  error. Genuine faults stay errors: stale byte range
  (`internal/graph/snippet.go` "stale byte range") and a file that vanished between
  index and read.
- **Convention (applied + documented):** *a miss is an empty / `found:false`
  result; only faults are errors.* Audit the other tools to confirm compliance —
  searches already return empty slices and `get_memory` already returns
  `found:false`, so this unit's code change is confined to `get_code_snippet`; the
  rest is a documentation pass (see unit G).

**Tests.** Existing symbol → `found:true` with correct `start_line`/`end_line` and
the same snippet text as today; unknown symbol → `found:false`, no error.

---

## Unit D — `search_graph` / `query_graph` signatures

**Problem.** `search_graph` returns name/path/line but no signature
(`internal/graph/search.go:45`, `SearchResult`). Confirming a hit, or choosing among
several, almost always needs a second `get_code_snippet` call just to see the
declaration line — every concept search is two round-trips.

**Change.**
- Add a `signature` column to `nodes`, populated **at index time** from the
  symbol's first source line (trimmed; capped at ~200 chars).
- Return `signature` by default in `search_graph` and `query_graph` results
  (add the field to `SearchResult` and the `GraphSymbol` wire shape,
  `internal/mcp/graph.go:26`).

**Why stored, not query-time.** Deriving the signature at query time means an extra
file read per result. Storing it at index time costs one more column write on a
path that is **not** the indexing bottleneck — per the project's indexing-perf
findings the cost is dominated by Resolve, not parse/write — and yields always-on
signatures with zero query-time IO.

**Migration.** Requires a schema migration adding the column and a one-time reindex
to backfill it for existing caches (acceptable pre-1.0). The migration must be
expressed through the store's existing schema-versioning mechanism; an un-backfilled
row returns an empty `signature` (the field is `omitempty`), so a stale cache
degrades gracefully rather than erroring.

**Tests.** After indexing a known file, `search_graph` results carry the expected
declaration line in `signature`; a node whose signature column is empty omits the
field rather than erroring.

---

## Unit E — Recall metadata: `matched_on`, `min_score`, `truncated`, `budget_used`

**Problem.** Recall results carry `score` and `caveat` but never explain **why** a
memory matched (which signal hit), and budgeted outputs **silently** degrade bodies
to titles and drop low-ranked entries (`internal/memory/recall.go`,
`selectWithinBudget`) — the model never learns truncation happened, so it cannot
decide to re-ask with a larger budget.

**Change.**
- **`matched_on: []string`** on `MemoryView` — the dominant signals (e.g.
  `["structural","lexical"]`) derived from the per-result `Signals` map that recall
  **already computes** (`internal/memory/recall.go:48`,
  `RecallResult.Signals`). Include signals whose weighted contribution clears a
  threshold, ordered by contribution descending. No ranking rework.
- **`min_score`** input on `recall_memory` — drop scored candidates below it before
  budgeting.
- **`truncated` + `budget_used`** on `recall_memory` output. To thread the budget
  data out, change `Engine.Recall`'s return from `[]RecallResult` to a
  `RecallResultSet{ Results []RecallResult; TokensUsed int; Truncated bool }`.
  `TokensUsed` is the `used` already returned by `selectWithinBudget`
  (`internal/memory/recall.go:153`); `Truncated` is true when candidates above
  `min_score` existed but did not fit the budget.
- Update the internal callers of `Recall` (the MCP handlers `recallMemory` /
  `searchBugs`, plus `handleSessionStart` and `handleAugment` in
  `internal/agents/handle.go`) to the new return type. Session-start formatting
  (`internal/agents/format.go`) keeps its existing behaviour; it just reads
  `.Results`.

**Tests.** `matched_on` reflects the dominant signal for a structurally-anchored vs
a lexically-matched memory; `min_score` excludes a known weak match; a recall whose
candidates exceed a tiny `token_budget` reports `truncated:true` with a positive
`budget_used`; a comfortably-fitting recall reports `truncated:false`.

---

## Unit F — `trace_call_path` suggestions-on-miss

**Problem.** `trace_call_path` resolves its `symbol` via exact name / qualified-name
match (`internal/graph/trace.go:105`, `nodeByName`). A slightly-off name yields
silent empty chains, with no hint — unlike `search_graph`, which nudges via
`weakResultHint`. (`search_graph`'s `candidate_symbols` lenient matching is the
model the rest should follow.)

**Change.**
- When `nodeByName` finds no exact match, run a lenient lookup — reuse
  `SearchGuided` with the supplied symbol as a candidate — and return the top
  candidate names in a new **`suggestions: []string`** field on
  `TraceCallPathOutput`, instead of an empty result with no guidance.
- `query_graph`'s `name_contains` is already a substring (lenient) match, so it is
  left unchanged.

**Tests.** Tracing a misspelled/partial symbol that has a close match returns empty
chains **plus** `suggestions` containing the real name; tracing an exact name still
returns chains and no suggestions.

---

## Unit G — Docs + tool descriptions

**Problem.** The `AGENTS.md` instructions block (`internal/agents/instructions.go:18`)
names only a subset of the toolset — it never mentions `get_architecture`,
`query_graph`, `list_memory`, or `get_memory`, nor `trace_call_path`'s confidence
semantics. The consuming model leans on the written rules, so it systematically
under-uses these tools.

**Change.**
- Rewrite the instructions block to:
  - Name `get_architecture` as the "orient in an unfamiliar area first" entry point.
  - Mention `query_graph`, `list_memory`, `get_memory`, and the new `forget_memory`
    (record wrong → forget it).
  - Note `trace_call_path`'s confidence semantics (trust high/typed/self; verify
    low/name hops with `get_code_snippet`).
  - State the miss-vs-error convention from unit C.
- Update the affected tool descriptions in `internal/mcp/server.go` and the
  `jsonschema` field docs in the handlers for every new field/tool introduced by
  units A–F.

**Tests.** Extend the existing instructions/install test style
(`internal/agents/install_test.go`) to assert the rewritten block names the
previously-omitted tools and `forget_memory`.

---

## Out of scope

- In-place `update_memory` (forget + re-record / `supersedes_id` covers correction).
- Lenient matching on `query_graph` (its `name_contains` is already substring).
- Pagination / cursors on list/search (a separate concern from this pass).
- Any change to recall's ranking weights or the embedding tier.

## Testing approach

TDD per unit against its handler/engine seam. The MCP handlers are deliberately
SDK-free and unit-testable without a transport (`internal/mcp/handlers.go` package
doc), and the agents hook logic is exercised through `Handle`/the adapters — both
patterns already established in the existing test suites
(`internal/mcp/*_test.go`, `internal/agents/*_test.go`). Each unit lists its
specific assertions above.
