# Recarto — Receiver-Aware Call Resolution (candidate sets + confidence) — Design Spec

> Date: 2026-06-04. Branch: `master`.
> Follows the read-time caller-gating fix in `internal/graph/trace.go` (bug-110),
> which removed the worst caller-direction collision but left resolution itself
> keyed on the simple method name. This spec fixes the resolution layer.

## Problem

Recarto's code graph keys call edges by **simple method name**. A call
`x.Handle()` is stored with `dst_name = "Handle"` — the receiver `x` (and thus
the type that disambiguates which `Handle` is meant) is discarded by the parser
(`csharp.scm:13`, `go.scm:5`). `Resolve` then binds the edge to *one* same-named
node via locality heuristics (same-file → same-dir → id).

In a real .NET codebase (Rollout) this makes call tracing unreliable wherever a
method name is shared across classes — `Handle`, `Start`, etc. across every saga
and handler. `trace_call_path` "callers" was the worst case (see bug-110), but
even after that fix the underlying resolution is a name-keyed guess: it cannot
tell `MigrationSaga.Handle` from `BackupSaga.Handle`, so it silently picks one.

Go is affected on **both** sides: a call `x.Handle()` loses its receiver (as in
C#), *and* a method definition `func (s *MigrationSaga) Handle()` indexes as the
bare `Handle` — the method node is a syntactic *sibling* of `type MigrationSaga`,
not nested in it, so span-containment-based `qualifiedName` never associates the
method with its receiver type.

The goal: bind `x.Method()` to the right `Type.Method` using receiver-type
information recoverable from **syntax alone**, and represent residual ambiguity
**honestly** — as a candidate set with confidence — so the LLM consuming
`trace_call_path` gets confident facts as facts and ambiguity as labeled
ambiguity, never a silent wrong pick. Languages: **C# and Go** now.

## Foundation decision (evidence / rationale)

We keep **tree-sitter as the always-on baseline** and add a heuristic type table,
rather than adopting a precise resolver (native compilers / LSP, or GitHub
stack-graphs) now. Rationale, weighed against Recarto's invariants (offline, no
build, any-repo, fast per-edit indexing):

| Option | Resolution quality | Needs project build | Per-edit fast | New deps | Available for C#/Go |
|---|---|---|---|---|---|
| tree-sitter + heuristic table *(this spec)* | medium | no | yes | none | yes |
| GitHub stack-graphs (on tree-sitter) | high | no | yes | Rust/FFI + author `.tsg` | **no official C#/Go defs** |
| native compiler / LSP (`go/types`, Roslyn, tsc) | highest | **yes** | **no** (workspace loads) | language SDKs | yes |

Two facts drive the choice:

1. **The edit-time hook re-indexes a file on every save** (`IndexFile` via the
   PostEdit hook). A compiler/LSP workspace load is seconds-to-tens-of-seconds —
   impossible on that hot path. tree-sitter parses incrementally in ms.
2. **stack-graphs has no off-the-shelf C# or Go definition** (GitHub ships
   TS/JS, Python, Java), so we would author and maintain `.tsg` rule sets
   ourselves — and for Go it is redundant with the more precise, in-process
   `go/types`.

The **candidate-set + confidence model is the seam**: a future precise backend
(`go/types` for Go; stack-graphs for TS/JS if it becomes first-class) is just
another producer of `edge_candidates` rows at `confidence = 1.0`. Choosing the
baseline now forecloses nothing.

## Goals

- Capture call **receivers** and per-scope **type bindings** for C# and Go.
- Fix Go method **qualified names** to include the receiver type.
- Resolve `x.Method()` to the correct `Type.Method` via a multi-strategy
  resolver, with each result carrying a **confidence** and a **resolution
  method** tag.
- Represent ambiguity as a **candidate set** (`edge_candidates`), and render it
  for the LLM consumer so the trace never silently picks one target.
- Prove caller/callee accuracy against compiler-grade oracles (Roslyn for C#,
  `go/types` for Go), reported before vs after, gated by a committed benchmark.
- No new dependency on the default build; the per-edit indexing path stays cheap.

## Non-goals (YAGNI — named so they don't creep in)

- TS/JS resolution. The data model is language-agnostic so they slot in later,
  but no `.scm`/resolver work for them now.
- The **runtime** precise tier. We build only the `edge_candidates(method,
  confidence)` seam, not a `go/types`/stack-graphs backend. Whether to build a
  runtime precise-Go tier is **deferred pending evidence**: the Go oracle (below)
  is written with the same in-process `go/types` API, so it doubles as that
  tier's prototype and measures the heuristic-vs-precise gap that would justify
  building it. (It can't be the *baseline* regardless: `go/packages` needs the
  module to load/build and is too slow for the per-edit hook.)
- Return-type inference (`var x = Foo()` stays name-only).
- Go **structural-interface** implementor discovery (Go has no explicit
  `implements`; Go interface dispatch stays low-confidence / name-only).
- Inner-block local shadowing precision; generic instantiation.

## Design

### 1. Pipeline

```
parse (.scm + parse.go)  → capture receiver + type bindings + Go method receiver
index.go                 → persist them; synthesize Go method qualified names
resolve.go               → multi-strategy candidate generation w/ confidence
trace.go / mcp / query   → candidate-aware traversal, adaptive floor, honest labels
store/migrate.go         → schemaVersion bump + graph wipe (per bug-071)
recarto-e2e              → oracle precision/recall (Roslyn C#, go/types Go)
```

### 2. Data model / schema

1. **`edges.dst_recv TEXT`** — receiver expression text at a call site
   (`_saga`, `this`, `MigrationSaga`, or `""` for a bare call).
2. **`type_bindings`** (new) — `(id, file_id, name, type_name,
   scope_start_byte, scope_end_byte)`: "within this byte span, identifier `name`
   has static type `type_name`." Populated from fields/properties, method &
   constructor parameters, typed local declarations, object creation
   (`new T(...)` C#; `T{}` / `&T{}` / `new(T)` Go), and the Go method receiver
   var. Scope = enclosing class/type body for fields; enclosing method body for
   params, locals, and the Go receiver.
3. **`edge_candidates`** (new) — `(edge_id, dst_node_id, confidence REAL,
   method TEXT)`: one call edge → 0..N candidate targets, each tagged with how it
   was resolved (`self` / `typed` / `static` / `iface` / `name`). This is the
   candidate-set model **and** the future-backend seam.
4. **`edges.dst_node_id`** — kept, redefined as a denormalized cache of the
   **top** candidate (highest confidence; locality tiebreak), or NULL. Existing
   single-target readers (`search`, the `resolve` invariant tests) keep working;
   `trace` reads `edge_candidates`.
5. **Migration** — bump `schemaVersion`, add the columns/tables, and **wipe the
   graph** (DELETE graph tables + `graph_token:%` meta) so existing caches
   rebuild under the new indexer. Enforced by the existing
   `store/migrate_test.go::TestLatestGraphMigrationRebuildsIndex` (per bug-071).

### 3. Parser captures (`internal/parse`)

- **Call receiver (C# + Go):** extend the member/selector-call patterns to also
  capture the receiver node alongside the method name. `parse.Edge` gains
  `Recv string`.
- **Type bindings (C# + Go):** new captures for field/property declarations,
  parameters, typed local declarations, and object creation → emit
  `TypeBinding{Name, TypeName, StartByte, EndByte}`. `parse.Result` gains
  `Bindings []TypeBinding`.
- **Go method receiver:** capture the receiver type in `method_declaration` —
  used (a) to synthesize the method's qualified name and (b) to emit a
  `TypeBinding` for the receiver var over the method body.

C# methods already nest inside the class, so C# definition-side qualified names
need no change; only call receivers + type bindings are new for C#.

### 4. Indexer (`internal/graph/index.go`)

- Persist `dst_recv` on call edges and the `type_bindings` rows.
- **Go method qualified names:** when a method node has a captured receiver type
  `T`, synthesize `qualified_name = "T.Method"` (instead of bare `Method`).
- `Resolve` (below) writes `edge_candidates` and the denormalized top
  `dst_node_id`.

### 5. Resolver — multi-strategy candidate generation (`internal/graph/resolve.go`)

`Resolve` becomes a pass that, per `calls` edge, emits `edge_candidates`. All
strategies stay scoped to the **language family** (the existing C#-vs-web guard
is preserved). Highest confidence first:

| # | Strategy | Trigger | Candidate(s) | Confidence | Tag |
|---|----------|---------|--------------|-----------|-----|
| S1 | Implicit-self / `this` | bare `M()` / `this.M()` / Go receiver `s.M()` inside type `C` | `C.M` if it exists | 0.95 | `self` |
| S2 | Typed receiver | receiver `r` resolves via innermost containing `type_binding` to type `T` | node `kind∈{method,property,ctor}` with qualified_name `T.M` (or `….T.M`) | 0.90 | `typed` |
| S3 | Static type-name receiver | receiver token *is* a known type name `T` (`Foo.Create()`) | `T.M` | 0.90 | `static` |
| S4 | Interface/base expansion *(C# only)* | S2's `T` is `interface`/`abstract` (via existing `extends` edges) | `M` on each implementor/derived type | 0.50 (set) | `iface` |
| S5 | Name-only fallback | nothing above fired | all same-name methods in family, capped, locality-ordered | 0.20 (set) | `name` |

- **Confidence → label:** `high ≥ 0.8`, `medium 0.4–0.8`, `low < 0.4`.
- **S2 scope match:** choose the `type_binding` for `name` whose
  `[scope_start, scope_end]` contains the call's byte offset and is the
  *innermost* such span (resolves field-vs-local cleanly).
- **Type/method node match:** type names are matched by simple name within
  family (collisions across namespaces simply yield multiple candidates — the
  set model absorbs this).
- `edges.dst_node_id` = highest-confidence candidate (locality tiebreak), else
  NULL.

### 6. Trace / MCP surface — built for the LLM consumer

The consumer of `trace_call_path` is the LLM. The output encodes the honesty the
model would otherwise have to perform by hand.

- **`CallNode` gains** `Confidence float64`, a `high|medium|low` label, the
  `Resolution` tag, and — for ambiguous hops — the `Receiver` expression text.
- **Adaptive floor (never silently empty):**
  - If a node has ≥1 high/medium candidate → return those; append a one-line
    summary of the suppressed tail: `(+N name-only matches; pass min_confidence=0
    to include)`.
  - If a node has *only* low-confidence candidates → surface them, labeled
    `ambiguous by name · low confidence`, rather than returning empty.
- **Uncertainty is terminal:** the tree expands only through
  high/medium-confidence edges. A low-confidence hop is a leaf that **lists its
  candidate targets** (`name → path:line`, capped ~5) — not just a count — so the
  LLM can `get_code_snippet` them directly without a second search. (Consumer
  refinement: a bare "3 candidates" forces another round-trip.)
- **Bounded, tree-shaped output:** the result is rendered as an indented call
  **tree**, each node shown once (a node already expanded elsewhere is referenced,
  not re-expanded), under a **global node budget** (default ~40) with a
  truncation summary `(… N more; raise depth/budget or pick a more specific
  symbol)`. This is the load-bearing fix for *my* token economy: today's
  root-to-leaf `[][]CallNode` chains can enumerate up to fanout^depth paths —
  unusable in context. The Go return may stay chains internally, but the
  MCP rendering the LLM reads is the budgeted, de-duplicated tree.
- **Caller direction** finds edges whose `edge_candidates` set includes `cur`;
  the bug-110 gating is naturally subsumed (candidates carry resolution; no
  ungated name match).
- **`min_confidence`** input lets the LLM widen (see everything) or narrow
  (confident spine only). Default behaviour is the adaptive floor above.
- **Consumer coaching** (the surface is dead weight if I'm not told how to read
  it): the MCP `trace_call_path` tool description explains `min_confidence`, how
  to read `[resolution · confidence]`, and that low-confidence leaves list
  candidate targets to inspect — mirroring the `candidate_symbols` guidance
  pattern proven in the query-guided-search spec.
- **Rendering** (compact, verbalizable):
  ```
  MigrationSaga.Handle
    → TakeSnapshotOrRestoreAsync        [typed · high]
        → SendToHostAsync               [typed · high]
    → Handle  [name · low]  recv: _handler  — 3 candidates:
        · BackupSaga.Handle      sagas/backup/BackupSaga.cs:41
        · RestoreSaga.Handle     sagas/restore/RestoreSaga.cs:38
        · PruneSaga.Handle       sagas/prune/PruneSaga.cs:55
    (… 12 more nodes; raise depth/budget or pick a more specific symbol)
  ```
- `search_graph` / `get_code_snippet` are unchanged (they read the top
  `dst_node_id`).

## Components / files to modify

| File | Change |
|---|---|
| `internal/parse/queries/csharp.scm` | Capture call receiver; field/property/param/local/`new` type bindings. |
| `internal/parse/queries/go.scm` | Capture call receiver; method receiver type; param/local/composite-literal/`new` bindings. |
| `internal/parse/parse.go` | `Edge += Recv`; `Result += Bindings []TypeBinding`; new `TypeBinding` type; populate from captures. |
| `internal/graph/index.go` | Persist `dst_recv` + `type_bindings`; synthesize Go method qualified names. |
| `internal/graph/resolve.go` | Multi-strategy resolver (S1–S5) → `edge_candidates` + top `dst_node_id`. |
| `internal/graph/trace.go` | Candidate-aware callers/callees; adaptive floor; uncertainty-terminal; `CallNode` fields. |
| `internal/mcp/graph.go` | `min_confidence` input; render confidence/resolution/receiver; tail summary. |
| `internal/store/migrate.go` | Schema bump: `edges.dst_recv`, `type_bindings`, `edge_candidates`; graph wipe. |
| `internal/store/migrate_test.go` | Extend `TestLatestGraphMigrationRebuildsIndex`. |
| `recarto-e2e/oracle/cs/*` | Extend `CsDump` to emit resolved call edges (Roslyn `SemanticModel`). |
| `recarto-e2e/oracle/go/*` (new) | `go/packages` + `types.Info` resolved-call-edge dumper (in-process; **not** gopls — gopls wraps the same `go/types` behind a separately-installed binary + LSP subprocess, more dependency for identical resolution). |
| `recarto-e2e/compare/*` | Resolution precision/recall vs oracle. |
| `internal/graph/*_test.go`, `testdata/` | TDD per strategy; regression benchmark fixture. |

## Data flow

```
LLM agent
  └─ trace_call_path{ symbol, direction, depth, min_confidence? }
       └─ Engine.TraceCallPath(...)
            ├─ nodeByName(symbol)                       (name or qualified_name)
            ├─ callNeighbors(cur, direction)
            │     └─ edge_candidates JOIN nodes         (confidence-ranked, family-scoped)
            │           ├─ high/medium → expand
            │           └─ low         → leaf + count (+ receiver)
            └─ chains [+ tail summary]
       → CallNode{ name, path, line, confidence, label, resolution, receiver? }

IndexFile(file)
  └─ parse → { Symbols, Edges(+Recv), Bindings }
       └─ write nodes (Go method qn = T.M), edges(+dst_recv), type_bindings
Reconcile → Resolve
  └─ per calls edge: S1..S5 → edge_candidates(+confidence,method); top → dst_node_id
```

## Testing / validation (proof-first, per the TDD mandate)

Written before implementation:

1. **Parser** — receiver capture + type-binding capture, C# and Go; Go
   method-receiver qualified name.
2. **Resolver, one test per strategy per language** — S1 self/`this`/Go-receiver;
   S2 field/param/local/`new`-typed receiver; S3 static type-name; S4 C#
   interface expansion → candidate set; S5 name-only → capped low-confidence set.
3. **Trace surface** — adaptive floor (only-low candidates *surface*, not empty;
   high present → tail summarized); uncertainty-terminal (low hop is a leaf that
   **lists its capped candidate targets**, not a bare count); **global node budget
   enforced** (output truncates with a summary past the budget) and **tree
   de-duplication** (a node reached twice is referenced, not re-expanded);
   confidence label + receiver rendered; `TestTraceCallPathCallersRespectsResolution`
   (bug-110) stays green.
4. **Migration** — `TestLatestGraphMigrationRebuildsIndex`: old-schema store
   rebuilds the graph under the new indexer.
5. **Oracle precision/recall** — extend `CsDump` (C#) + new `go/types` Go oracle;
   `compare` scores callee precision and caller precision/recall vs ground truth,
   **before vs after**, on Rollout (C#) and Recarto itself (Go).
6. **Regression guard (CI)** — a curated fixture benchmark (à la
   `searchbench.json`) asserting resolution precision stays above a threshold.
7. **Validation report** — `docs/validation/2026-06-04-receiver-aware-resolution.md`
   with the measured numbers.

## Rollout

1. Migration (schema + graph wipe) + tests (4).
2. Parser captures (1) → indexer (Go qualified names, persistence).
3. Resolver S1–S5 + `edge_candidates` (2).
4. Trace/MCP consumer surface (3).
5. Oracles + comparison + validation report (5–7).

## Risks / mitigations

- **Wrong type binding picks the wrong overload** → it becomes one candidate in a
  set; trace shows confidence/receiver, and the oracle benchmark (6) is the
  guardrail against net regression.
- **Candidate-set explosion / token blow-up** → adaptive floor + uncertainty-
  terminal expansion + the existing fan-out cap (25) bound the output.
- **Schema/perf cost of new tables** → per-edit hook still re-indexes only one
  file; `type_bindings`/`edge_candidates` are file-scoped writes.
- **Go structural interfaces under-resolve** → explicitly a non-goal; such calls
  stay low-confidence and are honestly labeled, exactly where a future `go/types`
  tier would upgrade them.
- **Backward compatibility** → `dst_node_id` retained as top-candidate cache, so
  `search`/snippet and existing resolve tests are unaffected.
