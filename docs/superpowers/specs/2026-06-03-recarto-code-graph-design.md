# Recarto — Phase 2: Code Graph — Design Spec

**Date:** 2026-06-03
**Status:** Approved design, pre-implementation
**Phase:** 2 of 3 (Code Graph). Phase 1 (Memory Spine) is **complete and merged**; Phase 3 (Multi-agent + Merge + Compaction) is specced separately.
**Implementation:** delivered as **one implementation plan** (not decomposed into sub-plans), built in the ordered sequence of §17.

---

## 1. Context & Vision

Phase 1 delivered the **Memory Spine**: a concurrency-safe per-repo SQLite store, a unified memory model, lexical+structural token-budgeted recall, MinHash dedup, a token ledger, on-demand markdown export, an MCP server (official Go SDK), a stdlib-`flag` CLI, and Claude Code installer/hooks — all as a CGO-free static Go 1.26.4 binary.

Phase 2 adds the other half of the original vision: **cbm's structural code intelligence**, reimplemented in Go. It parses the working tree with tree-sitter into a **knowledge graph** (symbols + relationships) stored in the *same* per-repo SQLite database, and exposes structural queries (`search_graph`, `trace_call_path`, `get_architecture`, `query_graph`, `get_code_snippet`) over the MCP, CLI, and hook surfaces. The payoff is the same as cbm's: an agent answers "where is X / who calls Y / what's the shape of this module" structurally instead of reading and grepping whole files, cutting code-exploration tokens dramatically.

This phase reuses everything Phase 1 built — the store, concurrency discipline, `internal/app` wiring, the MCP `addTool` adapter, the CLI command pattern, the ledger, and `gitctx` — and adds two new packages (`internal/parse`, `internal/graph`) plus extensions to the existing surfaces.

## 2. Goals (Phase 2)

1. **Parse** C#, Go, TypeScript (incl. `tsx`), and JavaScript (incl. JSX) with tree-sitter into a normalized symbol/edge graph. (React is covered for free via JSX/`tsx`.)
2. A **worktree-aware** knowledge graph in schema v2 of the existing per-repo store — each worktree's checkout is graphed independently, sharing one DB.
3. Five structural query tools — **`search_graph`, `trace_call_path`, `get_architecture`, `query_graph`, `get_code_snippet`** — over MCP **and** CLI, all worktree-scoped.
4. **Lazy reconcile + hook-incremental sync**: a git+mtime staleness check re-indexes only changed files (at query time and `SessionStart`); `PostToolUse` re-indexes agent edits immediately. No daemon.
5. The **Augment** hook behavior: on `PostToolUse(Grep|Glob)`, run a matching graph query and inject a structured answer as advisory context.
6. **Code-query token-savings** recorded in the existing ledger.
7. A **single static binary preserved** across the three ship targets via CGO + `zig cc` cross-compilation and static linking.
8. Built **test-first (TDD)** throughout, including a multi-process concurrency test and per-language golden fixtures.

## 3. Non-Goals (deferred)

- **Blazor / Razor (`.razor`, `.cshtml`)** — no official tree-sitter grammar; an embedded C#+HTML language needing C# injection or a community grammar. Deferred until the core pipeline is proven (revisit in a later iteration / Phase 3).
- **Broad symbol-only language tier** — supporting every grammar that ships `tags.scm` at the symbol level. The v1 set is the four deep-tier languages only; the architecture stays pluggable so this is a cheap later addition.
- **A real OS file-watcher (fsnotify) daemon** — the sync intent is met by lazy reconcile + hooks (§11). A live watcher is a Phase 3 option if sync-without-an-agent is ever needed.
- **Raw-SQL `query_graph`** — see D6; v1 ships a constrained structured query only.
- **Cross-worktree promote/merge of the graph, background compaction, OpenCode/Copilot installers** — Phase 3.
- **Neural embeddings / semantic code search** — out of scope here as in Phase 1 (decided independently per the Phase 1 D7 rationale).

## 4. Decision Log (from brainstorming)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Tree-sitter via the official CGO Go binding** (`github.com/tree-sitter/go-tree-sitter` + per-language grammar modules) — **deliberately reverses Phase 1's no-CGO decision** (`CGO_ENABLED=1`, build needs a C compiler) | The CGO path's only real cost is build/distribution (C toolchain, cross-compile, static linking) — **not** runtime or hook cold-start (native + fast; parsing is never on the hook hot path). WASM-via-wazero (pure-Go) was considered and set aside: more glue/maturity risk for less benefit given the small target matrix. Mature, well-supported, mirrors cbm. |
| D2 | **Ship targets are exactly three**: `linux/amd64`, `darwin/arm64`, `windows/amd64`. Cross-compile + static link via **`zig cc`** (`CC="zig cc -target …"`) | A small matrix keeps CGO cross-compilation tractable; `zig cc` gives painless cross-compile + static musl linking with one build-time dependency (not shipped to users). `modernc.org/sqlite` retained regardless. |
| D3 | **v1 language set: C#, Go, TypeScript (incl. `tsx`), JavaScript (incl. JSX)** — all **deep tier** (symbol search *and* call-path/architecture), fully tested | These four cover the user's stack; React = JSX/`tsx` needs no extra grammar. Deep tier is what makes `trace_call_path`/`get_architecture` useful. Grammars are pluggable for cheap future additions. |
| D4 | **The graph is worktree-tagged**; every query is worktree-scoped | Multiple worktrees share one DB but have genuinely different code on disk. Worktree is part of file identity; nodes/edges inherit it via `file_id`. Mirrors and extends Phase 1's branch/worktree tagging (Phase 1 D5). |
| D5 | **Sync = lazy reconcile + hook incremental, no daemon** | A git-status+mtime staleness check re-indexes only changed files at query time / `SessionStart`; `PostToolUse` re-indexes agent edits. Satisfies "auto-sync" intent without fsnotify's cross-platform baggage; fits the pure-CLI exit-0 hook model; reuses `gitctx`. |
| D6 | **`query_graph` is a constrained *structured* query, not raw SQL** | Stable contract for the LLM, no injection/runaway-query footgun, and it decouples the tool surface from the physical schema. |
| D7 | **The graph is a throwaway per-repo cache** — always rebuildable; a schema/cache-version bump invalidates cleanly | Same property as the Phase 1 store and the MinHash bands; no committed artifacts, no migration of stale graph data needed. |
| D8 | **Unresolved reference edges are kept by `dst_name`** (textual target), never dropped | `trace_call_path` still works by name-matching where deep resolution is partial; resolution can be sharpened later *without* reparsing. |
| D9 | **All CGO is confined to `internal/parse`** behind a safe wrapper; the **must-Close discipline** (`defer Close()` on every C-allocating tree-sitter object) is enforced there | Mirrors how the MCP SDK is confined to one file in Phase 1. `runtime.SetFinalizer`+CGO is buggy, so `Parser`/`Tree`/`TreeCursor`/`CQuery`/`QueryCursor`/`LookaheadIterator` **must** be explicitly closed or C memory leaks. Nothing else in the codebase touches a raw C object. |
| D10 | **TDD everywhere, proof-first**; one implementation plan (not decomposed) | Consistency with Phase 1; the build is ordered (§17) but ships as a single plan per user direction. |

## 5. Architecture Overview

Same one-binary / three-modes shape as Phase 1 (`serve` / `<command>` / `hook <event>`). Phase 2 adds two packages and extends existing ones.

**New / extended module layout:**

```
internal/parse/         tree-sitter integration — ALL CGO confined here:
                          grammar registry (lang → *sitter.Language),
                          per-language extraction (tags.scm + node-type→kind),
                          safe must-Close wrapper, ParseFile(...) → (symbols, edges)
internal/graph/         graph engine (mirrors internal/memory.Engine):
                          Index / Reconcile (write), Search / TraceCallPath /
                          Architecture / Query / Snippet (read), name resolution
internal/store/         + schema v2 migration (files / nodes / edges / nodes_fts)
internal/mcp/           + 5 graph tools via the existing addTool adapter
internal/cli/           + `index` and the `graph` subcommand group
internal/agents/claudecode/  + reconcile (SessionStart), incremental reindex
                          (PostToolUse Edit|Write), Augment (PostToolUse Grep|Glob)
internal/ledger/        + graph-query savings counters
internal/app/           wires the graph engine alongside the memory engine
```

`internal/app.Open(...)` remains the **single runtime wiring point**: it builds the graph engine (over store + ledger + clock + parse) next to the existing memory engine. Like the memory engine, the graph engine is **git-agnostic** — callers pass `worktree`/`branch`, injected from `gitctx` at the CLI/MCP/hook edges.

## 6. Build, Toolchain & Distribution (the CGO consequence)

Phase 2 flips the module to `CGO_ENABLED=1`. The grammars are generated C compiled into the binary.

- **C confined to `internal/parse`** (D9). The rest of the tree (and all tests outside `parse`) stays pure Go.
- **Cross-compilation & static linking via `zig cc`**: release builds set `CC="zig cc -target <triple>"` to produce statically-linked binaries for the three D2 targets from a single host. `zig` is a build-time dependency only.
- **CI** installs the C/zig toolchain and runs build + vet + test + `gofmt` (Phase 1's gate) on the three targets, plus a cross-compile smoke build of each release artifact and a `-race` run (Linux).
- **`modernc.org/sqlite` is retained** — it works, it's proven, and there's no reason to churn it even though CGO is now permitted.

## 7. Worktree / Identity Reuse

No new identity logic. The graph reuses `internal/gitctx` (Phase 1 §6): the **store key** is still the repo-id from `git rev-parse --git-common-dir` (identical across worktrees → one shared DB), and the **worktree** tag is `git rev-parse --show-toplevel` (distinct per worktree). The graph stamps `worktree` onto every `files` row, so two worktrees of the same repo maintain independent graphs in the shared store, and every query is scoped to the caller's worktree. Non-git directories fall back to Phase 1's stable-cwd-hash behavior.

## 8. Concurrency

Reuses Phase 1's proven discipline verbatim — WAL, `busy_timeout=5000`, `synchronous=NORMAL`, all writes via `BEGIN IMMEDIATE` on the serialized writer connection, atomic UPSERT counters, guarded migrations. Indexing/reconcile is the new high-volume writer, so:

1. Per-file index writes (delete old nodes/edges for the file, insert new) run inside **one `WriteTx`** so a file's graph is replaced atomically.
2. `ledger.Incr` for savings is called **outside** the enclosing `WriteTx` (the Phase 1B non-reentrant-mutex deadlock gotcha).
3. Reconcile of N files commits per-file (short transactions) so a long initial index never holds the write lock for the whole run.

**Proof (deliverable test):** a multi-process reconcile test — N goroutines + M `recarto` subprocesses indexing/reconciling against one temp DB in distinct worktrees — asserting `integrity_check = ok`, no lost savings counters, correct per-worktree node/edge counts, and no `SQLITE_BUSY` surfaced. Run on all three OSes in CI (mirrors Phase 1's `TestConcurrentWriters`).

## 9. Data Model (schema v2)

`PRAGMA user_version` advances to 2; the migration is an ordered Go func appended to `internal/store/migrate.go`. A cache/schema-version bump invalidates any stale graph (D7).

**`files`** — index state per source file (the reconcile diff target)
| column | type | notes |
|--------|------|-------|
| id | TEXT PK | UUIDv7 |
| worktree | TEXT | part of identity (indexed) |
| path | TEXT | repo-relative |
| language | TEXT | `csharp` \| `go` \| `typescript` \| `tsx` \| `javascript` \| `jsx` |
| content_hash | TEXT | for change detection |
| mtime | INTEGER | unix ms, fast pre-check |
| size | INTEGER | bytes |
| last_indexed_at | INTEGER | unix ms |

`UNIQUE(worktree, path)`; index on `(worktree)`.

**`nodes`** — symbols
| column | type | notes |
|--------|------|-------|
| id | TEXT PK | UUIDv7 |
| file_id | TEXT | FK → files (carries worktree) |
| kind | TEXT | function \| method \| class \| interface \| struct \| type \| field \| enum \| namespace \| module \| … |
| name | TEXT | simple name |
| qualified_name | TEXT | e.g. `Namespace.Class.Method` |
| signature | TEXT | nullable, display |
| start_byte / end_byte | INTEGER | for `get_code_snippet` |
| start_line / end_line | INTEGER | display |

Index on `(file_id)`, `(name)`, `(qualified_name)`.

**`edges`** — relationships
| column | type | notes |
|--------|------|-------|
| id | TEXT PK | UUIDv7 |
| src_node_id | TEXT | FK → nodes |
| dst_node_id | TEXT | **nullable** (resolved target) |
| dst_name | TEXT | textual target — kept even when unresolved (D8) |
| kind | TEXT | contains \| calls \| imports \| references \| extends \| implements |
| file_id | TEXT | FK → files (carries worktree) |
| line | INTEGER | call/reference site |

Index on `(src_node_id, kind)`, `(dst_node_id)`, `(dst_name)`.

**`nodes_fts`** — FTS5 over `name` + `qualified_name` for `search_graph`. Reuses Phase 1's bm25 pattern and its **negative-score gotcha** (`ORDER BY bm25 ASC`, flip sign before normalizing). Same pure-Go FTS5 already proven available in `modernc.org/sqlite`.

All graph rows are deleted/rewritten per file during reconcile; worktree scoping is enforced by joining through `files`.

## 10. Subsystems

### 10.1 `internal/parse` — tree-sitter integration (CGO-confined)
- **Grammar registry**: maps a language id → `*sitter.Language`, and a file extension → language id (`.cs`→csharp, `.go`→go, `.ts`→typescript, `.tsx`→tsx, `.js`/`.jsx`→javascript/jsx). Unknown extensions are skipped (not an error).
- **Extraction**: per language, a tree-sitter `tags.scm` query (defs + references — the symbol layer) plus a node-type→`kind` mapping, run over the parse tree to emit `(symbols, edges)`. Call/import/reference edges come from language-specific queries.
- **Safe wrapper (D9)**: a small type owns the `Parser`/`Tree`/`QueryCursor`/etc. and guarantees `defer Close()` on all of them; callers receive only plain Go structs (`Symbol`, `Edge`). No raw C-allocating object escapes the package. A test-only tracking wrapper asserts balanced open/close.
- `ParseFile(path, lang, src) → ([]Symbol, []Edge, error)`; a malformed file yields a best-effort partial result and never panics.

### 10.2 `internal/graph` — engine (mirrors `internal/memory.Engine`)
- **`Index(worktree, path, src)`**: parse → within one `WriteTx`, replace the file's `nodes`/`edges` and upsert its `files` row; update `nodes_fts`.
- **`Reconcile(worktree)`**: enumerate the worktree's tracked files (git, honoring `.gitignore`), diff `content_hash`/`mtime` against `files`, (re)index changed/new, delete removed (cascading their nodes/edges). Cheap when nothing changed: a `git status`/HEAD pre-check gates the stat pass.
- **Name resolution (deep tier)**: after symbols are written, resolve `calls`/`imports`/`extends`/`implements` `dst_name`s to `dst_node_id` within the worktree (per-language scoping: namespaces/packages, imports, receiver types). Unresolved targets remain name-only (D8). Resolution is a separate pass so it can be improved without reparsing.

## 11. Sync Model (lazy reconcile + hook incremental)

- **Query-time**: each query first ensures freshness via `Reconcile` (gated by the cheap staleness pre-check) so answers reflect the current checkout.
- **`SessionStart` hook**: runs `Reconcile` so the graph is warm at session start.
- **`PostToolUse(Edit|Write)` hook**: incrementally `Index` the specific edited file.
- **No daemon, no fsnotify.** Worktree-scoped throughout. External edits (IDE, `git pull`, branch switch) are caught by the next reconcile.

## 12. Query Tools (MCP + CLI, worktree-scoped)

| Tool | Purpose | Key args |
|------|---------|----------|
| `search_graph` | Symbol search via `nodes_fts` (bm25) + filters | `query`, `kind?`, `lang?`, `limit?` |
| `trace_call_path` | BFS over `calls` edges (resolved `dst_node_id`, falling back to `dst_name`) | `symbol`, `direction=callers\|callees`, `depth?` |
| `get_architecture` | Bird's-eye structural map: modules/namespaces, files, import/dependency edges, counts | `scope?` |
| `query_graph` | **Constrained structured query** (D6): node filters + optional edge traversal (kinds/direction/depth), read-only | `spec` (JSON) |
| `get_code_snippet` | Exact source for a node via stored byte range (reads the worktree's file slice) | `file`, `symbol`\|`range` |

Each MCP tool has a CLI twin under a `graph` subcommand group (`graph search`/`callers`/`callees`/`arch`/`query`/`snippet`) plus `recarto index`. CLI commands follow Phase 1C's pattern: `func Name(ctx, *app.App, io.Writer, args) error` + the shared `parseFlags` helper; the command is validated by `known()` before `app.Open`. MCP tools are added via the existing generic `addTool` adapter (In/Out structs with `json` + `jsonschema` tags; `,omitempty` ⇒ optional), worktree injected from `gitctx` at the server edge. Tool logic is thin over `internal/graph`; the heavy logic is unit-tested below the surface, with a round-trip test at the MCP layer.

## 13. Token Savings

Graph queries record tokens saved vs. the file-reads/greps they replace, into the existing ledger (Phase 1 §10 earmarked graph-query savings for Phase 2). Examples: `get_code_snippet` saves `whole-file − snippet`; `search_graph` saves vs. reading the candidate files it points at. New counters: `graph_queries`, `graph_tokens_saved` (per `session:<id>` and `lifetime`), atomic UPSERT like all Phase 1 counters.

## 14. Claude Code Integration

Extends `internal/agents/claudecode` (all hooks exit-0, never block — Phase 1C-2 contract):

- **`SessionStart`** → also `Reconcile` the worktree (warm graph); may inject a one-line architecture summary as `additionalContext`.
- **`PostToolUse(Edit|Write)`** → incrementally `Index` the edited file.
- **`PostToolUse(Grep|Glob)`** → **Augment**: run a matching `search_graph` on the pattern and inject the structured result as advisory `additionalContext` (non-blocking) — the spec's Phase 2 "Augment" behavior, now realizable because the graph exists.
- `recarto init` registers no new MCP server (the graph tools live in the existing `serve`); it adds the new hook wirings idempotently to `.claude/settings.json`.

## 15. Testing Strategy (TDD, proof-first)

- **Per-language golden fixtures**: small source files for C#, Go, TS, `tsx`, JS, JSX → expected `(nodes, edges)`. RED→GREEN per language.
- **Query tests** over a known fixture graph: `search_graph` ranking, `trace_call_path` chains (resolved + name-fallback), `get_architecture` shape, `query_graph` traversal, `get_code_snippet` byte-exactness.
- **Reconcile/incremental**: change one file → only it reparses; delete → its nodes/edges removed; external edit detected via hash/mtime; nothing-changed path is stat-only.
- **Concurrency**: the multi-process reconcile test (§8) on Linux/macOS/Windows.
- **CGO safety**: the must-Close tracking wrapper asserts balanced open/close; `-race` clean.
- **Build**: CGO build green on all three targets in CI; cross-compile smoke via `zig cc`; `gofmt` gate retained.
- **Determinism**: injected clock/git/fs; no wall-clock or real-git dependence in unit tests.

## 16. Risks & Mitigations

- **Grammar availability + `tags.scm` coverage** (*primary risk*) — confirmed by a **research spike at the start of implementation** (§17.0): verify the exact grammar modules for C#/Go/TS/JS, which ship `tags.scm`, and that they build under `zig cc` for the three targets. If a grammar lacks `tags.scm`, hand-write the queries.
- **CGO cross-compilation / static linking** — `zig cc` is the mitigation; the CI cross-compile smoke catches regressions early. Windows + CGO is the usual trouble spot → exercised in CI.
- **Binary size / build time** — only four grammars in v1 keeps this modest; revisit if/when the broad tier is added.
- **must-Close C memory leaks** — D9 wrapper + tracking test.
- **Deep-tier name resolution accuracy** — full resolution (overloads, generics) is hard; v1 resolves the common cases and keeps `dst_name` for the rest (D8), so partial resolution degrades gracefully rather than failing.
- **Reconcile latency on large repos** — gated staleness pre-check (git + mtime) keeps the no-change path cheap; per-file short transactions avoid long lock holds.

## 17. Implementation (single plan, ordered build sequence)

Delivered as **one implementation plan**. Recommended internal order (each step TDD, each builds on the last):

0. **Research spike** — confirm grammar modules + `tags.scm` + `zig cc` build for the three targets (§16).
1. **Toolchain & schema** — flip to `CGO_ENABLED=1`, set up the `zig cc` cross-compile + CI; schema v2 migration (`files`/`nodes`/`edges`/`nodes_fts`).
2. **`internal/parse`** — grammar registry, safe must-Close wrapper, per-language extraction (`tags.scm` + node mapping), `ParseFile`, golden fixtures per language.
3. **`internal/graph` write side** — `Index` (atomic per-file replace) + `Reconcile` (git+mtime diff) + name resolution; concurrency test.
4. **`internal/graph` read side** — the five query methods + token-savings accounting.
5. **Surfaces** — `internal/app` wiring; CLI `index` + `graph` commands; MCP tools via `addTool`.
6. **Sync & Augment** — `SessionStart` reconcile, `PostToolUse(Edit|Write)` incremental reindex, `PostToolUse(Grep|Glob)` Augment.

## 18. Phase Exit Criteria

Phase 2 is done when: `recarto index` builds a worktree-scoped graph for a C#/Go/TS/JS repo; `search_graph`/`trace_call_path`/`get_architecture`/`query_graph`/`get_code_snippet` answer correctly over MCP and CLI; the graph stays current via lazy reconcile (git+mtime) and `PostToolUse` incremental reindex with no daemon; `PostToolUse(Grep|Glob)` Augment injects a structured answer; two simultaneously-open worktrees of the same repo maintain correct independent graphs in the shared store (proven by the concurrency test); graph-query token-savings land in the ledger; the CGO build links statically and runs on `linux/amd64`, `darwin/arm64`, `windows/amd64`; and the full suite (incl. `-race`, `gofmt`) is green on all three OSes.
