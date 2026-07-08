# Recarto — HTTP Sync Layer for Cross-Machine Memory

**Status:** Implemented
**Date:** 2026-07-08

## 1. Problem

Recarto stores everything in a per-repo SQLite outside the working tree
(`internal/cache`, `~/.cache/recarto/<repo-id>/store.db`). The same repo worked from
multiple machines (or ephemeral per-task sandboxes) gets an independent cache on each —
memories/bugs/todos/decisions do not travel between machines.

Two kinds of data live in the store (`internal/store/migrate.go`):

| Data | Tables | Shareable? |
|---|---|---|
| **Memory** | `memories`, `memory_refs`, `memory_minhash`, `memories_fts`, `counters`, `sessions` | Yes — small, portable, machine-independent. |
| **Code graph** | `files`, `nodes`, `edges`, `nodes_fts*`, `type_bindings`, `edge_candidates`, `graph_deltas` | No — derived from AST, per-**worktree**, rebuilt in seconds, can be ~10⁶ rows. |

## 2. Owner decisions

- Sync **memory rows only** over HTTP; local SQLite stays authoritative; the code graph
  stays local/rebuilt (never shipped).
- **Sync-then-query-locally**, not read-through: a pull merges remote state into the
  local store (via the same MinHash merge core `Record`/`ImportMemory` already use), and
  every query — recall, list, graph — runs purely locally afterward. No query ever makes
  a network call.
- A single **global static bearer key** secures the server (no per-project keys).

## 3. Why not an off-the-shelf SQLite replicator

rqlite (Raft, leader-only writes — offline writes fail), Litestream (single-writer
disaster-recovery, not multi-writer), LiteFS (FUSE, single-primary), and libSQL/Turso
embedded replicas (closest in spirit, but the no-CGO driver is experimental and it still
replicates the whole file, including the graph) are all single-writer designs and/or
require CGO (recarto's store is deliberately pure-Go `modernc.org/sqlite`). Recarto's
workload is multi-writer-converging — independent authoring that must reconcile —
exactly what its MinHash near-duplicate merge already solves in pure Go. The right layer
is an application-level row sync over a thin HTTP transport, not a storage-engine swap.

## 4. Architecture

```
  machine A                                    machine B
  local SQLite (authoritative) <--merge-- pull  ...
      |  push (outbox + tombstones)                 ^
      v                                              |
          recarto server (`recarto server`)  ---------
          one SQLite store per repo key (sync_index + counters)
```

- **Pull** `GET /v1/repos/{key}/changes?since=&limit=` → paged records (+ tombstones)
  with server `seq > since`, applied via `memory.SyncApply`, cursor advanced per page.
- **Push** `POST /v1/repos/{key}/changes` → the local outbox (`memory_sync_outbox`) +
  pending tombstones (`memory_tombstones` with `pushed=0`); the server assigns new
  `seq`s and applies each record through the *same* `SyncApply` (the server is a
  headless recarto store).

## 5. Sync identity — the cross-machine repo key

`gitctx.RepoID` hashes the machine-local absolute path to `.git` — not stable across
clones. `gitctx.Context.RootCommit` is identical across every clone of the same repo, so
`internal/remotesync.RepoKey` resolves the sync key as:

1. `RECARTO_SYNC_REPO` env override, verbatim (shallow clones, multiple roots,
   intentional grouping).
2. else `sha256(RootCommit)[:16]`.
3. else sync stays disabled (no key can be resolved) — offline-only is always the floor.

## 6. Correctness-critical design (post-review corrections)

An initial draft of this design used `updated_at > push_watermark` as the push
selector and a 30-day tombstone GC default. Review caught three problems, all fixed in
the shipped implementation:

- **Echo-push risk.** Comparing timestamps to decide what to push cannot distinguish "I
  authored this" from "I just pulled this from the server" — a pulled row's
  `updated_at` can exceed an old watermark, so it would get echoed straight back.
  **Fix:** an explicit local **outbox** (`memory_sync_outbox`), populated *only* by
  local-authoring paths (`Record`, `ImportMemory`, the dedup merge's canonical bump) and
  cleared after a confirmed push. `SyncApply` (the pull-merge path) never writes to it —
  structurally, not just by timestamp discipline.
- **Unbounded server seq growth.** An idempotent re-push or a resurrection-guard-dropped
  record must not bump the server's sequence counter forever. **Fix:**
  `memory.SyncApply` returns a `SyncOutcome` (`SyncNoop` / `Inserted` / `Updated` /
  `Reinforced` / `Tombstoned`); the server only advances `sync_index`/`seq` when the
  outcome is not `SyncNoop`.
- **Tombstone resurrection.** An older non-deleted record arriving after a delete (an
  out-of-order pull page, or a slow peer) must not resurrect the row. **Fix:**
  `memory_tombstones(id, deleted_at, pushed)` is consulted before every non-deleted
  apply; a record older than a recorded tombstone is dropped as `SyncNoop`. A tombstone
  learned about remotely (via pull, or via the server applying a push) is marked
  `pushed=true` so it is never re-sent as if freshly authored locally — only an explicit
  `Forget` writes a `pushed=false` tombstone.
- **Canonical id on reinforce-merge.** When an incoming record has no local id match but
  reinforces an existing near-duplicate, `SyncApply` returns the *existing* row's id, not
  the incoming one (which may never exist as a row) — both the server's `sync_index` and
  the client's outbox bookkeeping key off this canonical id.
- **Reinforce-merge replay idempotency.** Because a reinforce-merge never creates a row at
  the incoming id, nothing distinguished a fresh convergence from a replay of an already-
  applied record (a retried push whose first response was lost, or a re-applied pull page).
  `memory_sync_merges(id, canonical_id, updated_at)` (migration v21) records what an
  incoming id last merged into; a replay at the same or older `updated_at` returns
  `SyncNoop` before `findNearDup` runs, so it neither reinforces again nor advances the
  server's `sync_index`/seq.
- **Tombstone alias resolution.** A delete for an incoming id that was previously merged
  into a different canonical row (via the reinforce-merge path above) must resolve through
  `memory_sync_merges` to that canonical id before the delete/tombstone is applied — the
  incoming id was never its own row here, so applying the delete verbatim would silently
  no-op while the real (canonical) row survives. Reachable whenever two peers (including
  the server) independently converge two near-duplicates onto different canonical ids and
  one is later `Forget`-ten by its authoring id.
- **Tombstone GC.** Given "full offline mode" is a hard requirement, the server does
  **not** default to any tombstone GC horizon (a machine offline longer than a fixed TTL
  would silently miss a delete and resurrect it later). No GC ships in this cut; add one
  later only if tombstone growth is measured to matter.

Only an explicit `Forget` ever writes a tombstone; compaction (archive/evict/dedup-merge/
superseded-GC) never does — one machine's routine housekeeping must never delete another
machine's live memories (`TestCompactionNeverWritesTombstones`).

## 7. Components (as shipped)

- `internal/syncwire` — the versioned JSON `Record`/`ChangesResponse`/`Push*` shapes
  shared by client and server.
- `internal/memory/sync.go` — `Engine.SyncApply` (tombstone guard → LWW update →
  near-dup reinforce → insert), `PendingPush`/`ClearPushed`,
  `PendingTombstones`/`MarkTombstonesPushed`, `markDirty`. `Forget` now also writes a
  tombstone; `Record`/`ImportMemory`/the dedup merge's canonical bump now call
  `markDirty`.
- `internal/store/migrate.go` v20/v21 — additive, idempotent: `memory_tombstones`,
  `memory_sync_outbox`, `memory_sync_merges` (the last guards reinforce-merge replay
  idempotency, §6). Not a graph migration.
- `internal/remotesync` — client: `config.go` (env), `identity.go` (`RepoKey`),
  `client.go` (HTTP `Pull`/`Push`/`Healthz`), `sync.go` (`Syncer`: `Pull`, `Push`,
  `Once`, cursor in `meta`).
- `internal/syncserver` — server: `schema.go` (`EnsureSyncSchema`, server-only,
  **not** in the client migration list), `auth.go` (constant-time bearer check),
  `server.go` (per-repo-key store cache, `bumpSyncIndex`), `handlers.go` (GET/POST
  `/v1/repos/{key}/changes`, `GET /healthz`).
- `internal/cli/server.go` (`recarto server`), `internal/cli/sync.go` (`recarto sync`).
- `internal/app/app.go` — `App.Sync *remotesync.Syncer`, nil when unconfigured.
- `internal/agents/handle.go` — bounded pull before recall at session-start; bounded
  push at Stop (the linchpin for an ephemeral sandbox: flush before it's destroyed).
- `internal/cli/doctor.go` / `status.go` — sync reachability/cursor/pending-count
  diagnostics.

## 8. Scope cuts from the original proposal

- **`internal/snapshot` was left untouched** (not refactored to share `syncwire.Record`
  with the committed-file path). It already has its own tested wire shape (`line`); the
  reuse was a nice-to-have, not a functional requirement, and touching well-tested,
  unrelated code was not worth the regression risk here.
- **No tombstone GC** (see §6) — deferred until real growth data justifies one.

Two items initially cut in the first pass were added back in review: gzip
request/response compression (`internal/remotesync.Client`, `internal/syncserver`'s
`gzip.go`) and a basic per-IP rate limiter on the server (`internal/syncserver/ratelimit.go`)
— both per the original design's §6.1/§11 expectations. `recarto serve`'s
per-write debounced push (`App.SignalSync`/`SyncTrigger`, consumed by
`mcp.Serve`'s `runSyncLoop`) also shipped, not cut — an interactive MCP
session converges within seconds of a `record_memory`/`record_bug`/
`complete_todo`/`forget_memory` call rather than waiting for the next
periodic tick.

## 9. Performance

- **Hot path unchanged** — recall/graph queries never touch the network.
- **Sync is O(delta)** — cursor-based pull, outbox-based push; steady state is a
  handful of small JSON records.
- **Bootstrap** (fresh clone / fresh sandbox, cursor 0) pulls all memories once — text
  JSON only, no graph, no binary DB copy (`TestBootstrapDoesNotCreateGraphTables` pins
  this).
- Every sync call is background + bounded (`syncPullBudget`/`syncPushBudget`, 5s each);
  a wedged or unreachable server never delays the agent.

## 10. ForgeBoard contract

The server ships in the `recarto` binary (`recarto server`), so the cross-repo surface
is config-only:

1. Run one `recarto server --addr … --data-dir …` with `RECARTO_SERVER_KEY` set and a
   persistent data volume, fronted by TLS.
2. Inject `RECARTO_SYNC_URL` / `RECARTO_SYNC_KEY` into every agent session's recarto
   invocation (MCP command + lifecycle hooks), alongside the existing
   `RECARTO_CACHE_DIR`.
3. Provision the one shared static key to both server and clients.
4. Optionally set `RECARTO_SYNC_REPO` per task if the harness clones shallowly.
