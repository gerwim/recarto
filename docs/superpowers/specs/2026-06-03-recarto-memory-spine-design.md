# Recarto — Phase 1: Memory Spine — Design Spec

**Date:** 2026-06-03
**Status:** Approved design, pre-implementation
**Phase:** 1 of 3 (Memory Spine). Phases 2 (Code Graph) and 3 (Multi-agent + Merge + Compaction) are specced separately.

---

## 1. Context & Vision

Recarto is a single Go tool that helps LLM coding agents build and navigate a codebase efficiently. It fuses two existing tools:

- **OpenWolf** (TypeScript): a `.wolf/` directory + Claude Code lifecycle hooks providing a project map, a learning memory (cerebrum), a bug log, and a token ledger — the "second brain" that makes the agent read less and repeat fewer mistakes.
- **codebase-memory-mcp / cbm** (C): an MCP server that builds a tree-sitter AST → SQLite knowledge graph (158 languages) and answers structural queries in <1ms, cutting code-exploration tokens by ~99%.

Recarto = OpenWolf's memory/consistency discipline + cbm's structural code intelligence, in one zero-dependency static Go binary, with first-class git-worktree support and multi-agent reach (Claude Code, OpenCode, GitHub Copilot CLI).

The whole product is decomposed into three phases. **This spec covers Phase 1 only: the Memory Spine** — everything except tree-sitter code intelligence (Phase 2) and cross-worktree merge + multi-agent + background compaction (Phase 3). Phase 1 is deliberately "OpenWolf-in-Go with a structured, concurrency-safe store," because it is almost pure, highly testable logic and delivers a usable vertical slice on its own.

## 2. Goals (Phase 1)

1. A `recarto` binary that runs as **MCP server**, **CLI**, and **hook entrypoint** from one executable.
2. A **concurrency-safe** SQLite store living **outside the working tree**, shared by all worktrees of a repo.
3. A **unified memory model** (preference / learning / decision / do_not_repeat / bug / action) replacing OpenWolf's cerebrum + buglog + memory.md + ADRs.
4. **Lexical + structural recall** with **token-budgeted** selection and **identifier-split + synonym expansion** for vocabulary bridging.
5. **MinHash near-duplicate dedup** that reinforces existing memories instead of duplicating them (the consistency property).
6. A **token ledger** with atomic, lost-update-free counters.
7. **On-demand markdown export** (no persistent files to drift).
8. **Claude Code integration**: MCP tools + session-start recall injection + repeated-read detection + session finalize.
9. Built **test-first (TDD)** throughout, with an explicit multi-process concurrency test as a deliverable.

## 3. Non-Goals (deferred)

- **Phase 2:** tree-sitter parsing, knowledge graph, `search_graph` / `trace_call_path` / `get_architecture` / `query_graph` / `get_code_snippet`, file-watcher auto-sync, code-query token-savings.
- **Phase 3:** cross-worktree promote-on-merge + git hooks, OpenCode + Copilot CLI installers, background compaction/decay daemon, committable team snapshot export/import, dashboard, reframe/designqc.
- **Embeddings of any kind** (see Decision D7). Lexical + structural only in Phase 1, and the memory layer is decided independently from the future code-search layer.

## 4. Decision Log (from brainstorming)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Go 1.26.4** (`go` + `toolchain` pinned in go.mod) | Single static binary, instant hook cold-start, mature tooling, trivial cross-compile. |
| D2 | **SQLite is the single source of truth**; markdown is on-demand export only | Enables pruning, token-budgeted recall, and structured merge. A render-only markdown has one writer → cannot drift. |
| D3 | **Pure-Go SQLite (`modernc.org/sqlite`, no CGO)** | Preserves the static-binary + cross-compile goal; still supports WAL. |
| D4 | **One store per repo in a global cache** (`$RECARTO_CACHE_DIR` or `~/.cache/recarto/<repo-id>/store.db`), shared across worktrees | Nothing committed → zero file-merge conflicts; new worktrees inherit memory instantly. |
| D5 | **Every record is branch/worktree-tagged**; recall is branch-aware | Branch isolation where it matters; "merge" becomes a structured promote (Phase 3), never a text merge. |
| D6 | **Layered pruning**: recall-time token budgeting (primary, lossless) + background compaction (Phase 3); **do_not_repeat is pinned, never decayed/evicted** | Keep context tiny without losing correctness-critical memory. |
| D7 | **No neural embeddings in the memory layer (v1)**; lexical + structural + MinHash, plus identifier-split + synonym expansion | Research-backed: cbm itself uses a *static precomputed table* (no runtime inference) and has *no* memory recall; hook cold-start is incompatible with runtime model loading; small short-record identifier-heavy corpora favor BM25; peers (Cody deprecated embeddings; Cursor/Claude/Cline rules are file-based). If ever added: daemon-only, opt-in, hybrid, static-table-first. |
| D8 | **Concurrency-safety is a primary constraint** (multiple worktrees → one DB) | Proven by an explicit multi-process test. See §7. |
| D9 | **TDD everywhere, proof-first** (RED→GREEN→REFACTOR); clock/fs/git are injected interfaces | Determinism + testability; drives clean module boundaries. |
| D10 | **Hooks are Advisory + Augment + one opt-in bounded Stop gate** — never hard-block Read/Grep | Mirrors cbm (non-blocking) and OpenWolf (reminders, ~85–90% compliance); hard-blocking breaks invariants and risks loops. Structural recall is phrasing-independent, so advisory injection is already effective; the opt-in Stop gate raises compliance without deadlock risk. |

## 5. Architecture Overview

One binary, three modes:

- `recarto serve` — **MCP server** over stdio (JSON-RPC 2.0). Universal surface for agents. Long-lived process.
- `recarto <command>` — **CLI** twin of every capability (scriptable, debuggable).
- `recarto hook <event>` — **hook entrypoints** invoked by the agent harness; short-lived, must cold-start in single-digit ms.

**Data location:** `resolveStore()` →
1. `$RECARTO_CACHE_DIR` if set, else OS cache dir (`~/.cache/recarto` on Linux, `~/Library/Caches/recarto` on macOS, `%LocalAppData%\recarto` on Windows).
2. `<cache>/<repo-id>/store.db` where `repo-id` is derived in §6.

**Module layout** (each independently testable):

```
cmd/recarto/            main; wires modes
internal/gitctx/        git/branch/worktree/repo-id resolution (interface + real + fake)
internal/store/         SQLite open, PRAGMAs, migrations, concurrency primitives, repos (DAOs)
internal/memory/        record model, write+dedup, recall ranking, budgeter, expansion
internal/ledger/        token estimation + atomic counters
internal/mcp/           JSON-RPC stdio server + Phase-1 tool handlers
internal/agents/claudecode/  installer + hook adapters
internal/export/        store → markdown renderer
internal/cli/           cobra commands
internal/clock/         injected clock (real + fake)
```

## 6. Repo / Worktree / Branch Identity (`internal/gitctx`)

Resolved by shelling to git through an injected `GitRunner` interface (real impl + fake for tests):

- **repo-id (store key):** `git rev-parse --git-common-dir` → absolute, canonicalized path. This is **identical across all worktrees** of a repo (worktrees share the common git dir). `repo-id = hex(sha256(canonicalCommonDir))[:16]`. The human-readable repo name (basename of toplevel or first remote) is stored as metadata for display.
- **root-commit:** `git rev-list --max-parents=0 HEAD` stored as metadata (used by Phase 3 team-snapshot matching across clones; unused for keying in Phase 1).
- **branch:** `git symbolic-ref --short -q HEAD`; on detached HEAD fall back to `detached@<short-sha>`.
- **worktree:** `git rev-parse --show-toplevel` (distinct per worktree). Stored as `worktree` tag (path) on records.
- **Non-git directory:** fall back to a stable hash of the absolute cwd; branch=`(none)`, worktree=cwd. Recarto still works without git.

All four are resolved once per process and cached.

## 7. Concurrency Design (the hard requirement)

Multiple processes hit the same `store.db` simultaneously: hooks firing from different worktrees, a running `recarto serve`, and ad-hoc CLI commands. Rules:

1. **WAL mode**: `PRAGMA journal_mode=WAL` (set once at store creation; persists). Concurrent readers + a single serialized writer.
2. **`PRAGMA busy_timeout=5000`** on every connection → writers wait up to 5s instead of erroring `SQLITE_BUSY`.
3. **`PRAGMA synchronous=NORMAL`** (safe under WAL, faster).
4. **All writes use `BEGIN IMMEDIATE`** and are kept short → acquire the write lock up front, no read→write upgrade deadlocks.
5. **Client-generated UUIDv7 ids** for records → no sequence/autoincrement contention; time-ordered for index locality.
6. **Atomic counters only**: ledger updates are `INSERT … ON CONFLICT(scope,metric) DO UPDATE SET value = value + excluded.value` inside the tx — never read-modify-write in Go (no lost updates across processes).
7. **Guarded migrations**: on open, `BEGIN IMMEDIATE; read PRAGMA user_version; apply pending migrations; set user_version; COMMIT`. The IMMEDIATE lock serializes concurrent first-run processes so migrations apply exactly once.
8. **Single writer connection per process**: the store keeps `db` with `SetMaxOpenConns` tuned; a dedicated serialized write path (mutex-guarded `*sql.Conn` or `MaxOpenConns=1` for the writer pool) avoids intra-process write contention while readers use a separate read pool.
9. **Idempotent, append-mostly writes**: dedup-on-write (§9) means concurrent identical records converge by reinforcing one row rather than racing to insert two.

**Proof (deliverable test):** `TestConcurrentWriters` spawns N goroutines *and* M separate `recarto` OS subprocesses that record memories and bump counters against one temp DB; asserts (a) `PRAGMA integrity_check` = ok, (b) no lost counter increments (sum equals expected), (c) every distinct record persisted, (d) no `SQLITE_BUSY` surfaced to the caller. Windows WAL locking behavior is covered by running the same test in CI on all three OSes.

## 8. Data Model (SQLite schema)

`PRAGMA user_version` holds the schema version; migrations are ordered Go funcs.

**`memories`**
| column | type | notes |
|--------|------|-------|
| id | TEXT PK | UUIDv7 |
| kind | TEXT | CHECK in (preference, learning, decision, do_not_repeat, bug, action) |
| title | TEXT | short summary |
| body | TEXT | full content |
| search_text | TEXT | derived: identifier-split + synonym-expanded title+body+refs (feeds FTS) |
| created_at | INTEGER | unix ms |
| updated_at | INTEGER | unix ms |
| branch | TEXT | nullable → repo-global |
| worktree | TEXT | path |
| author | TEXT | agent/user/source |
| source | TEXT | mcp \| hook \| cli |
| confidence | REAL | default 1.0 |
| reinforcement_count | INTEGER | default 0 |
| pinned | INTEGER | 1 for do_not_repeat (and any explicitly pinned) |
| status | TEXT | active \| superseded \| archived (default active) |
| superseded_by | TEXT | nullable id |
| simhash | INTEGER | 64-bit body fingerprint |
| extra | TEXT (JSON) | kind-specific fields (bug: error_message, root_cause, fix, file; decision: alternatives, rationale) |

**`memory_refs`** (reverse-lookup index for structural overlap)
`memory_id TEXT, ref_type TEXT (file|symbol|tag), ref_value TEXT, PRIMARY KEY(memory_id, ref_type, ref_value)`; index on `(ref_type, ref_value)`.

**`memory_minhash`** `memory_id TEXT, band INTEGER, hash INTEGER` — LSH banding for near-dup candidate lookup; index on `(band, hash)`.

**`memories_fts`** — FTS5 virtual table over `search_text` (default unicode61 tokenizer; we pre-split identifiers at write time, so no custom tokenizer needed — keeps it pure-Go). *Risk-gated: see §15.*

**`counters`** `scope TEXT (lifetime | session:<id>), metric TEXT, value INTEGER, PRIMARY KEY(scope, metric)`.

**`sessions`** `id TEXT PK, agent TEXT, branch TEXT, worktree TEXT, started_at INTEGER, ended_at INTEGER`.

**`meta`** `key TEXT PK, value TEXT` (repo name, root-commit, recarto version, etc.).

## 9. Memory Subsystem (`internal/memory`)

### 9.1 Write + dedup (the consistency property)
On `record`:
1. Normalize body; compute `search_text` (identifier split + synonym expansion, §9.3) and `simhash` + MinHash signature.
2. **Near-dup check** via MinHash LSH bands → candidates → exact Jaccard on shingles. If Jaccard ≥ `dedupThreshold` against an active record of the same `kind`, **reinforce** instead of insert: `reinforcement_count += 1`, `updated_at = now`, union refs/tags, bump confidence toward 1.0, optionally append a note. This is how Recarto "keeps memory consistent" — repeated learnings converge to one row. **Metric/threshold (implementation-refined, evidence-backed):** the initial 0.9 over **3-word** shingles proved wrong for short, identifier-heavy bodies — a heavy paraphrase scores *below* a topically-related distinct fact (negative separation). Refined to **2-word shingles with `dedupThreshold = 0.55`** (mid-valley between the measured distinct-fact ceiling ~0.29 and the realistic re-statement floor ~0.60), and LSH widened to **32 bands × 4 rows** so the S-curve knee (~0.42) sits below the threshold (16×8's ~0.71 knee gave only ~24% candidate recall at Jaccard 0.60). Full paraphrase remains out of scope for lexical dedup (consistent with D7, no embeddings in v1).
3. **Contradiction handling (lightweight, Phase 1):** a new `do_not_repeat`/`decision` may set `superseded_by` on an explicitly referenced prior id (via tool arg); no automatic semantic contradiction detection in Phase 1.
4. Else insert a new row + refs + minhash + FTS.

### 9.2 Recall (lexical + structural, token-budgeted)
Input context: `{ task_hint?, files[]?, symbols[]?, kind?, branch(resolved), limit?, token_budget? }`.

Per candidate score (weights configurable, golden-tested defaults):
- `lexical` — BM25 from FTS over (`task_hint` + file basenames + symbol names), min-max normalized.
- `structural` — overlap between candidate `memory_refs` and query `files`/`symbols` (weighted Jaccard). **Highest weight** — exact path/symbol match is the strongest relevance signal.
- `recency` — `exp(-age / halflife)`.
- `reinforcement` — `log(1 + reinforcement_count)`.
- `branch` — boost if `branch == currentBranch` or `branch IS NULL` (repo-global).
- multiply by `confidence`; drop `status != active`.

**Selection:** always include active `do_not_repeat` (pinned) first under a dedicated sub-budget; then greedily add top-ranked records until `token_budget` (estimated per-record tokens, §10) is exhausted. Default session-start budget ≈ 1500 tokens (configurable). Output is structured (id, kind, title, body, why-surfaced signals).

**Recall semantics & the honest limit:** the **structural signal is the workhorse and is phrasing-independent** — a memory tagged to `src/auth/session.go` resurfaces whenever the agent touches `src/auth/session.go`, regardless of how the memory was worded. The **lexical (BM25)** signal adds word-level matching (error codes, symbol/lib names, identifiers) but does **not** bridge synonyms; the synonym map (§9.3) only covers curated pairs, not arbitrary paraphrase. The residual gap is therefore a memory that shares **neither words nor file/symbol refs** with the current task — a pure paraphrase miss. We do **not** solve that in v1; instead we **measure** it: every `recall_memory` that returns nothing above threshold increments `recall_misses` (§10), giving the data to justify (or not) the deferred daemon-only embeddings path before paying its cost.

### 9.3 Identifier split + synonym expansion
- Split camelCase / snake_case / kebab / dotted into component words; keep both original and split forms in `search_text`.
- Apply a small **curated** synonym/abbreviation map (e.g. `cfg`↔`config`, `auth`↔`authentication`, `db`↔`database`, `repo`↔`repository`) at write time (expand `search_text`) and optionally at query time. Map ships as an embedded, user-overridable data file. **Scope is deliberately narrow**: it bridges known abbreviations and a hand-picked synonym set, *not* open-ended domain paraphrase (`refresh`↔`renewal`, `race condition`↔`concurrency bug` are out of scope and left to the structural signal / future embeddings).

## 10. Token Ledger (`internal/ledger`)
- **Estimator:** char/token heuristic (~3.8 chars/token, tunable), good to ~15% (OpenWolf parity). Pure function, golden-tested.
- **Counters** (atomic, §7.6): per `session:<id>` and `lifetime` — `reads`, `repeated_reads_blocked`, `writes`, `memories_recorded`, `memories_recalled`, `recall_tokens_injected`, `recall_misses` (recall calls returning nothing above threshold — measures the paraphrase gap, §9.2), `sessions`.
- Phase 1 "savings" = tokens avoided by repeated-read detection + recall reuse. (Graph-query savings arrive in Phase 2.)

## 11. MCP Server + Tools (`internal/mcp`)
JSON-RPC 2.0 over stdio. Phase 1 tools:

| Tool | Purpose |
|------|---------|
| `record_memory` | Insert/reinforce a memory (`kind`, `title`, `body`, `refs?`, `pinned?`). |
| `record_bug` | Convenience wrapper → `kind=bug` with `error_message/root_cause/fix/file/tags`. |
| `recall_memory` | Token-budgeted, branch-aware ranked recall (`task_hint?`, `files?`, `symbols?`, `kind?`, `limit?`, `token_budget?`). |
| `search_bugs` | OpenWolf "bug search" parity — query the bug subset before fixing. |
| `get_memory` / `list_memory` | Fetch by id / list with filters. |

Each tool has a CLI twin (§12). Tools are thin handlers over `internal/memory` + `internal/ledger`; the heavy logic is unit-tested below the MCP layer, with a round-trip integration test at the MCP layer.

## 12. CLI (`internal/cli`, cobra)
`init` (set up Claude Code integration for this repo), `serve`, `hook <event>`, `remember <kind> …`, `bug add …`, `bug search <term>`, `recall [query] [--files …]`, `memory show` (markdown export to stdout), `status` (repo-id, branch, worktree, db path, counts, ledger), `doctor` (db reachable, WAL on, `integrity_check`, git resolution, migration version).

## 13. Claude Code Integration (`internal/agents/claudecode`)

**Enforcement model: Advisory + Augment + one opt-in bounded Stop gate.** Hooks inject context and remind; they do **not** hard-block `Read`/`Grep` (gating those breaks the read-before-edit invariant). The only hard lever is an **opt-in** Stop gate (below). "Augment" (running a graph query alongside `Grep`/`Glob` and injecting the structured answer, non-blocking) is a **Phase 2** behavior since it needs the code graph; Phase 1 ships Advisory + the opt-in Stop gate.

`recarto init` (idempotent):
- Registers the MCP server (project `.mcp.json` or user `~/.claude` per flag).
- Installs hooks into `.claude/settings.json`:
  - **SessionStart** → `recarto hook session-start`: opens/creates session row, emits the token-budgeted recall slice (pinned do_not_repeat + top-ranked) as `additionalContext`, plus a one-line "record learnings/bugs" nudge. **Always exits 0** (non-blocking).
  - **PostToolUse(Read)** → `recarto hook post-read`: logs the read to the ledger, estimates tokens, warns on repeated reads this session. Exit 0.
  - **Stop** → `recarto hook stop`: finalizes session counters, reminds to record unrecorded learnings/bugs. Exit 0 (advisory).
  - **(opt-in) Stop gate** → when enabled (`recarto config set stop_gate true`), the Stop hook **blocks once** (`decision: block` with a reason) if it heuristically detects an unrecorded bug-fix or decision this session (e.g. edits to a previously-error-associated file, or test-fix churn) and no matching `record_bug`/`record_memory` call was made. Bounded: blocks at most once per session, trivially satisfied by recording, and self-clears so it can never loop or deadlock.
  - **(stretch)** **PreToolUse(Write|Edit)** → `recarto hook pre-write`: inject any `do_not_repeat` whose refs match the target file. Non-blocking.
- Writes a short `RECARTO.md` instruction snippet (or appends to `CLAUDE.md`) describing the record/recall discipline.
- All hooks are **structurally non-blocking** (exit 0 on every path) and cold-start in single-digit ms (lexical-only, no model load).

## 14. Testing Strategy (TDD, proof-first)
- **Unit (RED→GREEN→REFACTOR):** identity resolution (fake GitRunner), dedup (near-dup pairs reinforce, distinct insert), recall ranking (golden fixtures with known orderings), budgeter (respects token cap, pins do_not_repeat), token estimator, identifier-split + synonym expansion, markdown render (golden).
- **Concurrency:** `TestConcurrentWriters` (§7) — multi-goroutine + multi-process, integrity + no-lost-updates, run on Linux/macOS/Windows CI.
- **Store:** migration idempotency under concurrent open; WAL asserted; busy_timeout honored.
- **MCP integration:** record → recall round-trip over the JSON-RPC layer.
- **Determinism:** injected clock, fs, and GitRunner everywhere; no wall-clock or real-git dependence in unit tests.
- Coverage gate enforced in CI; every new behavior starts with a failing test.

## 15. Risks & Mitigations
- **`modernc.org/sqlite` FTS5 support/behavior** — *primary risk.* Mitigation: a day-1 spike test confirming FTS5 availability + ranking. Fallback if absent/poor: a Go-side inverted index (`terms` table) with a BM25 implementation over our pre-split tokens — same scoring contract, no FTS dependency. The recall interface is designed so the lexical backend is swappable.
- **Go 1.26.4 not in this environment (1.24.4 present)** — `toolchain go1.26.4` triggers auto-download (`GOTOOLCHAIN=auto`), or install manually; confirm at implementation start.
- **Windows WAL cross-process locking** — covered by the concurrency test in CI on Windows; `busy_timeout` + IMMEDIATE writes are the standard mitigation.
- **Hook latency** — keep hook paths lexical/SQL-only, no model load; budget a soft deadline and exit 0 on overrun (cbm's discipline).
- **Recall quality without embeddings (vocabulary bridging)** — mitigated by identifier-split + synonym map; revisit (daemon-only, opt-in, hybrid) only if measured paraphrase misses justify it (Phase 3+).

## 16. Phase Exit Criteria
Phase 1 is done when: a fresh repo can `recarto init` for Claude Code; session-start injects a budgeted recall slice; the agent can `record_memory`/`record_bug`/`recall_memory`/`search_bugs` via MCP and CLI; dedup reinforces instead of duplicating; the ledger tracks reads/recalls atomically; `memory show` renders markdown on demand; all of the above is shared correctly and safely across two simultaneously-open worktrees of the same repo (proven by the concurrency test); and the full suite is green on Linux/macOS/Windows.
