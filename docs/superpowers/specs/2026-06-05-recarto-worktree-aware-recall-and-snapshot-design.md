# Recarto — Worktree-Aware Recall & Shareable Memory Snapshot

- **Status:** Design approved (brainstorming) — pending spec review
- **Date:** 2026-06-05
- **Phase:** 3 (Multi-agent + Merge + Compaction) — replaces the original "promote-on-merge" and "team snapshot export/import" slices
- **Predecessors:** Phase 1B memory engine (recall), Phase 2 code graph (per-worktree `files`/`nodes`), Phase 3 Slice 1 compaction/decay (MinHash dedup)

## 1. Context & Problem

Recarto stores all memories for a repo in **one shared per-repo SQLite store** outside the working tree. Memories are tagged with `branch`/`worktree` but are **not** physically siloed per branch — a memory written on any branch is visible DB-wide, and recall is branch-*aware* (same-branch ranked higher).

The original Phase 3 plan framed the open work as "merge": promote branch-local memories to mainline when a branch merges, plus a committable team snapshot. Brainstorming on 2026-06-05 found two flaws in that framing:

1. **Promote-on-merge cannot be detected reliably.** The intended signal (git commit ancestry) is defeated by the dominant real workflow — **GitHub squash-merge** (and rebase-merge) create new commits whose history does not contain the branch's commits, so `git merge-base --is-ancestor` returns false. Ancestry is the wrong signal.

2. **Promotion solves the wrong problem.** Important memories and bugs already surface regardless of branch, so the "trapped on a dead branch" risk is minor. The *real* and more dangerous failure is the **inverse**: a memory or bug surfaces in a worktree where the code it references **does not exist** (a not-yet-merged feature, a different branch, planned-but-unwritten code). The agent then chases a phantom — opens a missing file, "fixes" a bug in absent code. Bugs are the worst case, since their entire premise is "this file was broken."

**Reframe:** the valuable signal is not "how did this branch's code reach mainline" but **"is the code a memory references present in the current worktree right now?"** This is answerable directly from the per-worktree code graph, and it makes the squash-merge problem disappear entirely — we never inspect git history, only whether the referenced symbols/files exist here.

## 2. Goals

- Recall is **worktree-aware**: memories whose code references don't resolve in the current worktree are flagged so the agent is not misled, without losing the knowledge.
- A **committable team snapshot** lets memories travel between machines/people (the store lives outside the working tree and never travels with git today).
- Reuse existing machinery: the MinHash/Jaccard dedup from compaction, the `memory_refs` join pattern from `bugsurface.go`, the lazy-at-session-start convention from Phase 2 reconcile & compaction.
- **No new persisted schema columns.** No git hooks. No daemon.

## 3. Non-Goals

- **Promote-on-merge / branch re-tagging.** Dropped. Branch tag remains pure provenance.
- **Git ancestry / squash detection.** No longer needed.
- Sharing the code graph (rebuildable from committed source; self-heals via reconcile).
- Real-time / multi-writer sync, conflict-resolution UI.
- Secret-scrubbing of memory bodies (called out as a future concern, §10).

## 4. Architecture Overview

Three components, all **memory-only** except for a single read-only dependency on the graph tables:

```
                       ┌────────────────────────────┐
recall_memory ───────► │ internal/memory (recall)   │
session-start inject   │  + RefResolver (NEW)       │──reads──► files/nodes
CLI recall             └────────────────────────────┘          (current worktree)
                                   │ uses
                                   ▼
snapshot import ─────► ┌────────────────────────────┐
                       │ internal/merge (NEW)       │  MinHash/Jaccard-0.55
                       │  mergeRecord()             │  dedup → Insert|Reinforce|Supersede
                       └────────────────────────────┘
                                   ▲ uses
snapshot export/import │ internal/snapshot (NEW)    │  .recarto/memory.jsonl
CLI + lazy session-start└───────────────────────────┘
```

## 5. Component A — Reference Resolvability in Recall

### 5.1 The three buckets

Refs are optional (`memory_refs` is a separate 0..n table; many memories — preferences, most decisions — carry none). Recall classifies each candidate:

| Memory shape | Treatment |
|---|---|
| **No code refs** | Branch-neutral, always valid. Resolver skipped. Never caveated, never demoted. |
| Code refs that **resolve** in current worktree | Fully relevant; normal ranking. |
| Code refs that **dangle** | Caveat attached; demoted **unless** pinned or a bug (see 5.3). |

### 5.2 RefResolver interface & resolution rules

Recall depends on an interface, not the graph package, to keep the memory engine independently testable and the graph optional:

```go
// Resolve reports, for the given worktree, which of refs are present in the code
// graph. tag refs are non-code and always treated as resolved (ignored).
type RefResolver interface {
    Resolve(ctx context.Context, worktree string, refs []Ref) (resolved, dangling []Ref, err error)
}
```

The graph-backed implementation lives near `internal/graph` (or `internal/app` wiring) and runs indexed lookups, mirroring the existing `bugsurface.go` join style:

- **`file` ref** → `SELECT 1 FROM files WHERE worktree=? AND path=?`. Both sides are **repo-relative forward-slash** form already — memory refs via `memory.RelFileRef`, graph paths via git ls-files / `filepath.Rel+ToSlash` (`gitsource.go`, `source.go`). No path reconciliation needed.
- **`symbol` ref** → `SELECT 1 FROM nodes n JOIN files f ON n.file_id=f.id WHERE f.worktree=? AND (n.name=? OR n.qualified_name=?)`.
- **`tag` ref** → not code; always resolved.

`app` wires the graph-backed resolver into the engine; tests inject a fake.

### 5.3 Partial-aware policy

A memory may carry several refs, some resolving and some not (a partially-landed feature).

- **Caveat** lists the *specific* dangling refs.
- **Demote** only when **no** code ref resolves (the memory is entirely about absent code). If at least one resolves, it is partially relevant — annotate the missing refs but do not demote.
- **Pinned and bug memories are never demoted** — importance wins — but they **still receive the caveat**. They keep their always-surface privilege without silently misleading.

### 5.4 Caveat format

A short, structured note attached to the recalled memory (flows through `recall_memory`, session-start injection, and CLI `recall`):

```
⚠ references code not present in this worktree (branch feat/x):
   internal/foo/bar.go, symbol BazClient — may not apply to the current branch
```

The provenance branch comes from the memory's `branch` column.

### 5.5 Graceful degradation (critical)

If the current worktree has **zero graph rows** (`SELECT count(*) FROM files WHERE worktree=?` is 0 — ungraphed language, non-git, or pre-reconcile), the resolver is **skipped entirely**: every memory falls into the "no code refs" bucket and recall behaves exactly as today. This prevents a cold/unindexed repo from caveating everything. Graph reconcile is already lazy-at-session-start, so by recall time the current worktree is normally populated; accept minor first-session staleness.

## 6. Component B — Shared Merge Core (`internal/merge`)

A single dedup primitive reused by snapshot import (and available to compaction):

```go
type Outcome int // Inserted | Reinforced | Superseded

func mergeRecord(ctx, conn, incoming *memory.NewMemory) (Outcome, id string, err error)
```

It reuses the **existing** 2-shingle MinHash-LSH banding at Jaccard ≥ 0.55 that compaction already uses — the current in-package matcher is **extracted into a small exported helper** so promote/import/compaction share one definition rather than duplicating it.

- Incoming ≈ existing (≥ 0.55) → **reinforce** existing (`reinforcement_count++`), keep the higher `confidence`/`pinned`.
- Incoming has no near-duplicate → **insert** (recompute FTS + MinHash rows locally).
- Incoming declares it supersedes a local id → mark local `superseded`.

## 7. Component C — Team Snapshot (`internal/snapshot`)

### 7.1 Format

`.recarto/memory.jsonl` at repo root — one memory per line, **sorted by `id`** for deterministic, minimal git diffs.

### 7.2 Exported fields (portable content only)

`id, kind, title, body, created_at, branch (provenance), author, source, confidence, reinforcement_count, pinned, status (active only), superseded_by, simhash, extra`, plus the memory's `refs` (file/symbol/tag).

**Deliberately excluded:**
- `last_used_epoch`, `recall_count`, `archived_epoch` — local activity signals; sharing them leaks usage patterns and causes churn.
- FTS and MinHash rows — **recomputed on import**, never shipped.
- `archived`/`superseded` memories; the code graph.

### 7.3 Export filter — generous

Export **all active + pinned** memories (minus the exclusions above). **No branch-based curation.** Rationale: branch-fit is now enforced at *recall time on every consumer* (Component A), so a teammate who imports a not-yet-relevant memory simply sees it caveated until that code lands for them. The export side does not need to be clever, which is consistent with dropping promotion.

`recarto snapshot export` writes/overwrites the file.

### 7.4 Import — via merge core

Each incoming record runs through `internal/merge.mergeRecord` (§6) → dedup + reinforce + supersede. Idempotent: re-importing an unchanged snapshot is a no-op (everything reinforces in place).

- **Manual:** `recarto snapshot import`.
- **Lazy at session-start:** when the file's content hash differs from a stored `meta` key (`snapshot_hash`), import runs automatically, so a teammate's `git pull` is absorbed without a manual step — consistent with the lazy philosophy. Hash is updated after a successful import.

## 8. Surfaces

- **CLI:** `recarto snapshot export`, `recarto snapshot import`. Recall caveats render in CLI `recall` output.
- **Hooks (`internal/agents` Handle, shared across claude/opencode/copilot):** session-start triggers lazy snapshot import (§7.4); recall caveats flow through session-start context injection.
- **MCP:** caveats are included in `recall_memory` results. **No new MCP tools** — export/import are maintenance ops, not agent-facing queries.

## 9. Data Model

- **No new `memories` columns.** (The reframe eliminated the `branch_commit` column the earlier draft required.)
- **New `meta` key:** `snapshot_hash` — content hash of the last-imported `.recarto/memory.jsonl`.
- New file artifact: `.recarto/memory.jsonl` (committed by the user).

## 10. Testing Strategy (TDD throughout)

- **RefResolver:** file resolve / file dangle / symbol resolve / symbol dangle / tag-ignored / partial (some resolve) / **no-graph degradation** (empty worktree → nothing caveated), against a seeded graph fixture.
- **Recall integration:** dangling-ref memory caveated + demoted; **pinned** and **bug** caveated but **not** demoted; ref-less memory untouched; partial-resolve memory caveated but not demoted.
- **Merge core:** dedup table tests (insert / reinforce near-dup / supersede), mirroring existing compaction dedup tests.
- **Snapshot:** export field-set assertions (excluded fields absent); round-trip export→import; **idempotent re-import**; import dedups against existing local memories; lazy import fires only on hash change.
- Suite + `-race` + `vet` + `gofmt` green; binary smoke.

## 11. Open Questions / Future

- **Symbol-name ambiguity:** a `symbol` ref matched purely by `name` could resolve against an unrelated same-named symbol. Acceptable for a coarse presence check; revisit with qualified-name-first matching if false "resolves" appear.
- **Secret-scrubbing:** memory bodies may contain sensitive strings; the committable snapshot makes that shareable. Out of scope here; flag before any team rollout.
- **Mainline-relevance export filter:** if generous export proves noisy in practice, the resolvability-against-mainline filter (considered and deferred) is the fallback.

## 12. Decision Log

- **2026-06-05 — Dropped promote-on-merge.** Ancestry detection is defeated by squash/rebase merges (the dominant workflow); ref-absence was considered but promotion solves a minor problem while ignoring the dangling-reference risk. Branch tag becomes provenance only.
- **2026-06-05 — Reference-resolvability is the branch-relevance signal.** Replaces all git-history inspection; pulls the code graph into recall as a read-only dependency with graceful degradation when absent.
- **2026-06-05 — Annotate + demote (pinned/bugs caveat-only).** Preserve knowledge, inoculate the agent; importance overrides demotion but never the caveat.
- **2026-06-05 — Generous snapshot export.** Recall-time resolvability protects every consumer, so the export filter need not curate by branch.
- **2026-06-05 — No new schema columns.** The reframe eliminated `branch_commit`.
