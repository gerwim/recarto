# Recarto — Phase 3: Compaction & Decay — Design Spec

**Date:** 2026-06-03
**Status:** Approved design, pre-implementation
**Phase:** 3 of 3 (Multi-agent + Merge + Compaction). Phase 1 (Memory Spine) and Phase 2 (Code Graph) are **complete and merged**.
**Scope:** This is the **first slice of Phase 3**. Phase 3 is four largely independent subsystems — **compaction/decay** (this spec), **promote-on-merge**, **team snapshot export/import**, and **OpenCode + Copilot CLI installers** — each getting its own spec → plan → build cycle, exactly as Phase 1 shipped (1A/1B/1C-1/1C-2). The legacy OpenWolf "dashboard" and "reframe/designqc" items are **dropped from Phase 3 scope** (outside the recarto binary's mission).
**Implementation:** one implementation plan, built in the ordered sequence of §12.

---

## 1. Context & Vision

Phase 1 delivered the **Memory Spine**: a concurrency-safe per-repo SQLite store, a unified memory model (`preference`/`learning`/`decision`/`do_not_repeat`/`bug`/`action`), lexical+structural token-budgeted recall, MinHash dedup, a token ledger, on-demand markdown export, an MCP server, a stdlib-`flag` CLI, and Claude Code installer/hooks. Phase 2 added the **Code Graph**.

As an agent works a repo over weeks, the memory store accumulates: stale `action` log entries, near-duplicate `learning`s captured across branches/sessions, and `superseded` rows left behind by reinforcement. Left unchecked the store grows, recall candidate sets bloat, and signal-to-noise drops. **Compaction/Decay** keeps the per-repo store **high-signal and bounded over time**, performing three mechanical operations — **decay + eviction**, **superseded GC**, and a **global dedup sweep** — with no daemon, no runtime LLM, and behavior that is **robust to sporadic repo usage**.

This slice reuses everything Phase 1 built — the store + concurrency discipline, `internal/app` wiring, the CLI command pattern, the ledger (incl. `EstimateTokens`), the `meta` key-value table, `internal/memory.Engine`, and the MinHash fingerprinting in `internal/memory/fingerprint.go`. It adds one file (`internal/memory/compact.go`), a schema v4 migration, a `compact` CLI command, and a `SessionStart` hook trigger.

## 2. Goals

1. A single **compaction pass** performing, in order: superseded GC → budget-triggered decay+eviction → bounded global dedup sweep → archive GC.
2. Decay measured on a **logical activity clock (epochs), never wall-clock** — robust to sporadic repo usage.
3. Eviction is **budget-triggered**, not age-triggered: a quiet/normal repo never loses memory; eviction is a safety valve against runaway growth.
4. **Eviction = archive-then-GC**: a two-stage, reversible removal leaning on the pre-existing `status='archived'` enum (recall already skips it).
5. **Tiered kind policy**: `do_not_repeat` pinned; `decision`/`bug` exempt from eviction; `preference`/`learning` standard decay; `action` fastest decay.
6. **Hook-driven + CLI** trigger, **no daemon**, exit-0 best-effort in the hook (mirrors Phase 2 lazy reconcile).
7. **Single-flight** across concurrent worktrees/sessions; concurrency proven with a multi-process test (first-class concern).
8. Built **test-first (TDD)** throughout, including a **benchmark over a 50k-record synthetic store** that resolves the synchronous-vs-detached execution question with numbers.

## 3. Non-Goals (deferred)

- **Detached background process** (forking `recarto compact` from the hook). Deferred behind the §11 benchmark; added only if measurements show the synchronous path blocks the agent UI. Synchronous + budgeted is the v1 path.
- **Structural cluster-merge** (collapsing clusters to a canonical representative beyond pairwise near-dup). Redundant with the global dedup sweep.
- **Recall-ranking's wall-clock recency** (`recall.go:105` uses `updated_at`). It has a milder version of the cadence issue, but it lives in the recall path, not compaction. Flagged as a related observation; out of scope.
- **LLM summarization / rewriting of memories** — no runtime model (locked Phase 1 decision); only mechanical operations.
- **A real fsnotify daemon / timer process** — the no-daemon stance (Phase 2 D5) holds.

## 4. Decision Log (from brainstorming)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Trigger = hook-driven + CLI, no daemon.** A bounded pass runs opportunistically in `SessionStart` (gated to once per interval), plus an explicit `recarto compact` CLI for manual/cron runs. | Preserves the locked no-daemon invariant (Phase 2 D5); fits the exit-0 hook model; mirrors lazy reconcile. A daemon reintroduces cross-platform process-lifecycle problems for no gain. |
| D2 | **Operations = decay+eviction, superseded GC, global dedup sweep.** Structural cluster-merge excluded. | The three are mechanical and feasible without an LLM/embeddings. Cluster-merge overlaps the dedup sweep. |
| D3 | **Decay measured in a logical activity clock (`compact_activity_epoch`), not wall-clock.** | Wall-clock decay is broken under sporadic usage: a "1 week" half-life archives everything in a repo opened fortnightly before the user returns. Epochs advance only when the repo is actually worked on. |
| D4 | **Eviction is budget-triggered** (live-set token budget), not age-triggered. Epoch-decay only ranks *what* to evict when over budget. | The decisive cadence safety net: a quiet/normal repo never crosses the budget, so it **never loses memory regardless of cadence**. Eviction is a safety valve, not routine behavior. |
| D5 | **Eviction = archive-then-GC.** Decay archives (`status='archived'`); a later pass hard-deletes archived rows past a grace period. | The schema already has the `archived` enum and recall already skips it (`recall.go:92`) — near-zero new read-path code, reversible/auditable, and GC still reclaims space. |
| D6 | **Tiered kind policy.** `do_not_repeat` pinned/never; `decision`+`bug` exempt from eviction (durable rationale + fix history; still dedup-able); `preference`+`learning` standard half-life; `action` shortest half-life. | Matches the real semantics of each kind. Exempt kinds leave the eviction candidate set entirely. |
| D7 | **Track recall usage.** Add `last_used_epoch` + `recall_count`; recall stamps surfaced rows in its existing tail write-phase via one batched `UPDATE`. | "Time since last *recalled*" is the true staleness signal; `updated_at` (last write) would archive frequently-recalled-but-never-rewritten memories. Recall already writes ledger counters there, so this is not a new read→write transition. |
| D8 | **Synchronous + budgeted execution; measure before considering detached spawn.** | Decay/eviction/GC are single fast SQL statements; only the dedup sweep is variable-cost and gets a per-pass work budget + cursor. The §11 benchmark decides whether detaching is ever needed. Avoids per-OS process-spawn code with no proven need. |
| D9 | **Single-flight via a `compact_lease` CAS in `meta`.** | Multiple worktrees/sessions can trigger at once; the lease prevents overlapping passes and double epoch-advance. Uses the existing `WriteTx` + `SQLITE_BUSY` retry. |
| D10 | **TDD everywhere, proof-first; one implementation plan.** | Consistency with prior phases. |

## 5. Architecture Overview

Same one-binary / three-modes shape (`serve` / `<command>` / `hook <event>`). This slice adds one file and extends existing surfaces — no new package.

```
internal/memory/compact.go   NEW. Engine.Compact(ctx, CompactOptions) (CompactReport, error);
                               epoch read/advance + meta helpers; the four pass steps.
                               Reuses fingerprint.go (MinHash/Jaccard) for the dedup sweep
                               and the existing reinforce path.
internal/memory/recall.go    + epoch-stamping batched UPDATE in the tail write-phase.
internal/store/migrate.go    + schema v4 migration (3 new memories columns).
internal/app/                + MaybeCompact(ctx) (gated/leased, for hooks) and
                               CompactNow(ctx, opts) (for the CLI).
internal/cli/compact.go      NEW. `recarto compact [--force] [--dry-run] [--budget-tokens N]`.
internal/cli/status.go       + epoch / live-tokens-vs-budget / archived-count / last-run lines.
internal/agents/claudecode/  + SessionStart calls app.MaybeCompact (best-effort, exit-0).
```

Every surface goes through `internal/app`, per the established single-wiring-point convention.

## 6. Data Model (schema v4)

One append-only migration (`schemaVersion` 3 → 4). Add to `memories`:

| Column | Type | Meaning |
|---|---|---|
| `last_used_epoch` | `INTEGER NOT NULL DEFAULT 0` | Activity-epoch at which this memory was last recalled (or created). Decay = `current_epoch − last_used_epoch`. |
| `recall_count` | `INTEGER NOT NULL DEFAULT 0` | Cumulative times surfaced by recall. Observability + future signal. |
| `archived_epoch` | `INTEGER NOT NULL DEFAULT 0` | Epoch at which the row was archived; drives archive-GC grace. |

Existing rows backfill to `0`. This is harmless: eviction is budget-triggered (D4), so old-looking rows are only ever evicted if the store is genuinely over budget — never merely because their backfilled epoch is `0`.

New `meta` keys (the `meta` table is the existing `(key, value)` store):

| Key | Meaning |
|---|---|
| `compact_activity_epoch` | Monotonic session/activity counter. Starts at `1`. |
| `compact_last_run_at` | Wall-clock ms of last actual pass — gates `minInterval`. |
| `dedup_sweep_cursor` | Last `memory_minhash` band-bucket processed — resume point for the bounded sweep. |
| `compact_lease` | Single-flight lease: `pid:expiry_ms` (or similar) CAS-set at pass start. |

The graph schema (Phase 2 `files`/`nodes`/`edges`/`nodes_fts`) is untouched.

## 7. The Activity Clock

`compact_activity_epoch` advances **+1 once per active session**, performed by the gated trigger at the start of a pass (so it ticks only when the repo is actually worked on). Because the trigger is `minInterval`-gated (D1), multiple hook/CLI invocations within one session do **not** over-tick.

**Recall integration:** recall reads the current epoch (pooled-DB read) during candidate scoring, and in its existing tail write-phase — immediately alongside the `led.Incr` calls at `recall.go:127/130` — issues **one batched statement**:

```sql
UPDATE memories SET last_used_epoch = :E, recall_count = recall_count + 1
WHERE id IN (<surfaced ids>);
```

This is a single `WriteTx` over the (few) surfaced rows, reusing the auto `SQLITE_BUSY` retry. It does not change recall's concurrency class — recall already takes the write mutex for the ledger counters at that point.

## 8. Decay Scoring

For memory *m* at current epoch *E*:

```
age        = E − m.last_used_epoch
decay      = exp(-age / halfLife[m.kind])
relevance  = decay × (1 + log1p(m.reinforcement_count)) × m.confidence
```

Lower `relevance` = more evictable. `relevance` only ranks the eviction candidate set; it never by itself triggers eviction (D4).

**Per-kind half-life (epochs) — tunable policy constants, not optimizable parameters:**

| Kind | Half-life | Eviction-eligible? |
|---|---|---|
| `action` | short (default ~5) | yes (fastest decay) |
| `preference` | medium (default ~30) | yes |
| `learning` | medium (default ~30) | yes |
| `decision` | — | **no — exempt** |
| `bug` | — | **no — exempt** |
| `do_not_repeat` | — | **no — pinned** |

Exempt kinds never enter the eviction candidate set. They can still be **deduped** by the sweep and have their **superseded** copies GC'd. Defaults are deliberately conservative and overridable (§10); they are policy choices, not values to be empirically optimized (contrast the dedup threshold, which was).

## 9. One Compaction Pass

A pass runs only if `now − compact_last_run_at ≥ minInterval` (or CLI `--force`), **and** after acquiring `compact_lease` (CAS in `meta`; if another session holds an unexpired lease, **skip silently**). The lease is released (or left to expire) at the end. Steps, in order — each its own `WriteTx`, idempotent and resumable:

1. **Advance epoch** — `compact_activity_epoch := E + 1`; record `compact_last_run_at = now`.
2. **Superseded GC** — `DELETE FROM memories WHERE status='superseded'`. CASCADE clears `memory_refs`/`memory_minhash`; the corresponding FTS rows are removed. One fast set-deletion.
3. **Decay + eviction (budget-triggered)**:
   - Compute live-set tokens: `Σ ledger.EstimateTokens(body)` over `status='active'` rows (single read pass).
   - **If `liveTokens ≤ budget` → do nothing.**
   - Else rank eviction-eligible rows (active, non-exempt kind) by `relevance` ascending and **archive** from the bottom — set `status='archived'`, `archived_epoch = E`, and **delete the row's FTS entry** (so it stops being a lexical candidate) — until `liveTokens ≤ budget`.
4. **Global dedup sweep (bounded)** — walk `memory_minhash` band-buckets starting at `dedup_sweep_cursor`, at most **N buckets per pass** (default tunable). For same-`kind`, `active` collisions whose Jaccard ≥ `0.55` (reusing `fingerprint.go`), fold the duplicate into the **highest-reinforced canonical** (tiebreak: higher confidence, then newer): bump the canonical's `reinforcement_count`, set the duplicate `status='superseded'`, `superseded_by=<canonical id>`. Advance/store the cursor; wrap to the first bucket when the end is reached. (Superseded rows are reclaimed by step 2 on the next pass.)
5. **Archive GC** — hard-delete `status='archived'` rows with `E − archived_epoch > archiveGraceEpochs` (default tunable); CASCADE + FTS cleanup. `do_not_repeat` never reaches `archived`, so it is structurally safe.

`Engine.Compact` returns a `CompactReport` (counts: superseded-GC'd, archived, deduped, archive-GC'd, liveTokens before/after, epoch). `--dry-run` runs steps 2–5 in compute-only mode and returns the same report **without writing**.

## 10. Configuration

All knobs have safe defaults; overridable via flags (on `compact`) and `RECARTO_*` env (read in `internal/app`), consistent with existing config handling:

| Knob | Default | Notes |
|---|---|---|
| budget tokens | **250,000** | Deliberately generous — comfortably larger than any single recall budget, so normal/quiet repos never evict; catches only pathological growth. The safety-valve threshold. |
| half-life epochs (`action` / `preference` / `learning`) | 5 / 30 / 30 | per-kind |
| `archiveGraceEpochs` | 30 | archive → hard-delete grace |
| dedup buckets per pass (`N`) | 1,000 | the sweep's per-pass work budget; cursor resumes across passes |
| `minInterval` | 1h | gate so epoch ≈ sessions |

These are **starting defaults** (policy, not empirically optimized); revisit budget/`N` once the §11 benchmark and real usage inform them.

## 11. Performance & the Benchmark (resolves D8)

Latency rationale: decay/eviction/GC are single SQL statements (O(rows), milliseconds at realistic per-repo sizes); only the dedup sweep is variable-cost and is capped per pass by `N` + cursor. Combined with `minInterval` gating (≈ once per session) and the `SessionStart`-only trigger (never `PostToolUse`), the synchronous path should never sit on the agent's critical path.

This is **proven, not assumed**: a `BenchmarkCompact` over a **synthetic 50k-record store** measures full-pass latency (and the dedup sweep in isolation). The recorded numbers decide whether the deferred detached-spawn path (§3) is ever needed. The result is documented in the plan and `.wolf/cerebrum.md`.

## 12. Build Sequence (TDD, ordered)

1. **Schema v4 migration** + migrate tests (new columns, backfill, idempotency).
2. **Epoch helpers** in `compact.go` (read/advance `compact_activity_epoch`; `meta` accessors) + tests.
3. **Recall epoch-stamping** (batched `UPDATE` in the tail write-phase) + tests (surfaced rows get current epoch; `recall_count` increments).
4. **Decay scoring** (`relevance`, per-kind half-life, exempt kinds) + tests (monotonic in age; exempt kinds excluded).
5. **Superseded GC** + test.
6. **Budget-triggered eviction** + tests — including the **quiet-repo invariant** (under budget ⇒ zero evictions no matter how far the epoch advances), lowest-scored-first, stop-at-budget, FTS row removed, exempt kinds never archived.
7. **Global dedup sweep** (bounded, cursor resume) + tests (near-dups merged into canonical; bound respected; cursor wraps).
8. **Archive GC** (grace) + test.
9. **`Engine.Compact` orchestration** + `CompactReport` + **`--dry-run`** parity test.
10. **Single-flight lease** + **multi-process concurrency test** (4 OS processes × worktrees sharing one store.db: no double-evict, consistent epoch, no lost updates) — mirrors the Phase 2 `multiproc_test.go`.
11. **`internal/app`** `MaybeCompact` / `CompactNow` wiring + tests.
12. **CLI** `recarto compact` (`--force`/`--dry-run`/`--budget-tokens`) + `status` extensions + tests.
13. **SessionStart hook** trigger (best-effort, exit-0) + test.
14. **`BenchmarkCompact`** over a 50k synthetic store; record latency; resolve D8.
15. Full suite + `-race` + `go vet` + `gofmt`; end-to-end binary smoke (`recarto compact --dry-run`, then real, then `status`).

## 13. Testing Strategy

- **Unit (RED→GREEN→REFACTOR)** for every step above; deterministic via the existing fake clock and an injectable epoch.
- **Quiet-repo invariant** test is the headline correctness proof for the cadence fix (D3/D4).
- **Concurrency:** multi-process + multi-goroutine compaction proving the `compact_lease` single-flight (first-class concurrency rule).
- **Benchmark:** the 50k-store `BenchmarkCompact` (D8).
- All race-clean, `gofmt`-gated, as enforced by CI.

## 14. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Backfilled `last_used_epoch=0` makes old rows look stale | Budget-triggered eviction (D4) — never evict purely on age; covered by the quiet-repo invariant test. |
| Epoch over-ticks (multiple invocations/session) | `minInterval` gate + single-flight lease; epoch advances once per gated pass. |
| Dedup sweep too slow on huge stores | Per-pass bucket budget `N` + persisted cursor; benchmark verifies. |
| Archiving leaves rows in FTS, bloating candidate sets | Archive deletes the FTS row (step 3); GC removes the row entirely. |
| Recall write-on-read contention | One batched `UPDATE` over only surfaced rows, in the write-phase recall already uses; `SQLITE_BUSY` auto-retry. |
| Losing a still-valuable memory | Two-stage archive→GC (reversible during grace); exempt kinds; budget-only trigger. |
