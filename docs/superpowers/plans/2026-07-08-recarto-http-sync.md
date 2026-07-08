# Recarto HTTP Sync — Implementation Log

Companion to
[docs/superpowers/specs/2026-07-08-recarto-http-sync-design.md](../specs/2026-07-08-recarto-http-sync-design.md).
All steps below shipped in one pass, TDD-style (tests written alongside each package,
full suite green throughout).

1. **`internal/store` migration v20** — `memory_tombstones`, `memory_sync_outbox`
   (additive, idempotent, non-graph). `TestSchemaVersionIs20` pins the count.
2. **`internal/syncwire`** — the shared wire types (`Record`, `ChangesResponse`,
   `Push*`).
3. **`internal/memory`** — `SyncApply` plus outbox/tombstone plumbing
   (`sync.go`), `markDirty` wired into `Record`/`ImportMemory`/`applyMerge`, `Forget`
   writes a tombstone. 12 table tests in `sync_test.go` cover insert/LWW-update/
   reinforce/tombstone/resurrection-guard/idempotent-noop/compaction-never-tombstones/
   outbox-lifecycle.
4. **`internal/remotesync`** — client: config/identity/HTTP client/`Syncer`
   orchestration (`Pull`/`Push`/`Once`, cursor in `meta`).
5. **`internal/syncserver`** — server: static-key auth, per-repo-key store cache,
   `EnsureSyncSchema`, GET/POST `/v1/repos/{key}/changes`, `/healthz`. 7 handler tests
   (auth, round-trip, idempotent-no-reseq, tombstone propagation, paging, invalid-key
   rejection).
6. **CLI** — `recarto server`, `recarto sync`; registered in `cli.go`.
7. **Wiring** — `App.Sync` constructed from env in `app.Open`; bounded pull at
   session-start (before recall), bounded push at Stop
   (`internal/agents/handle.go`); `doctor`/`status` sync diagnostics.
8. **Integration tests** (`internal/remotesync/sync_test.go`, two in-memory "machines"
   against one in-process server over a unix socket — this sandbox's loopback TCP is
   unreachable, so `httptest.Server` could not be used; a unix listener exercises the
   identical `net/http` server/client code): two-machine convergence on a recorded
   memory, independent-authoring convergence, forget propagation + no resurrection,
   offline-then-catch-up, bootstrap without ever touching graph tables, and
   root-commit-based repo-key stability across differing local `RepoID`s.
9. **README** — new "Sharing memory across machines (HTTP sync)" section.

## Verification

- `go build ./...`, `go vet ./...`, `gofmt -l .` clean (except one pre-existing
  unrelated file, `internal/mcp/handlers_feedback_test.go`, untouched by this work).
- `go test ./...` green across every package except a pre-existing,
  environment-specific failure in `internal/update` (`TestHTTPFetcher`) that fails
  identically on a clean checkout before this work, caused by this sandbox blocking
  loopback TCP connections entirely (confirmed with a minimal `httptest`-only
  reproduction) — unrelated to this change.
- `-race` clean on `internal/memory`, `internal/remotesync`.

## Round 2 — review fixes

A review pass found three correctness gaps before merge; all three are fixed:

1. **Server apply + seq/index bump were not atomic.** `handlePostChanges` ran
   `memory.SyncApply` in one `WriteTx`, then `bumpSyncIndex` in a second, separate
   `WriteTx` — a crash or error between the two left a change applied but never
   indexed for future pulls, with no way to recover it. Fixed by generalizing
   `SyncApply` into `SyncApplyTx(ctx, rec, afterApply)`: when the merge produces a
   genuine change, `afterApply` runs on the SAME `store.Conn` before the transaction
   commits. `bumpSyncIndex` became `bumpSyncIndexTx`, called from that hook — the
   memory change and its `sync_index`/`seq` bump are now one commit, or neither
   happens. `SyncApply` is now a thin wrapper (`SyncApplyTx(ctx, rec, nil)`) so every
   existing caller is unaffected. New test:
   `TestSyncApplyTxHookFailureRollsBackTheMerge` proves a failing hook rolls back the
   merge too.
2. **Completed todos (and other explicit status transitions) never synced.**
   `CompleteTodo` didn't call `markDirty`, and `PendingPush` filtered to
   `status='active'`, so even a queued completion would have been excluded. Fixed:
   `CompleteTodo` and `ResolveFeedback` (the identical pattern one kind over) now
   `markDirty` on their explicit write; `PendingPush`'s `status='active'` filter is
   removed (the outbox itself is the gate — only explicit local-authoring paths ever
   populate it, compaction never does, so removing the filter is safe). An explicit
   `supersedes_id` write in `Record` now also marks the superseded row dirty, for the
   same reason. While fixing this, found and fixed a related bootstrap bug:
   `syncApplyAbsent` (the insert-via-sync path) always landed a fresh row as
   `status='active'`, so a machine bootstrapping a memory whose origin had already
   moved it on (e.g. pulling an already-completed todo for the first time) would
   have resurrected it as active — now the incoming non-active status/`superseded_by`
   is applied immediately after insert, and the row drops out of the lexical index
   exactly like the local completion paths do. New tests:
   `TestCompleteTodoQueuesForPushWithDoneStatus`, `TestResolveFeedbackQueuesForPush`,
   `TestSupersedeQueuesOldMemoryForPush`,
   `TestSyncApplyBootstrapInsertLandsNonActiveStatus`.
3. **`recarto serve` (MCP) had no periodic sync loop or write-triggered push, and
   session-start only pulled.** Fixed: `Syncer` now carries its configured
   `Interval()`; `mcp.Serve` starts a background goroutine (`runSyncLoop`) that runs
   `Sync.Once` on that interval AND on a signal from `app.App.SyncTrigger` (a
   buffered, always-allocated channel; `App.SignalSync()` is a non-blocking send).
   `record_memory`, `record_bug`, `complete_todo` (only when it actually completed
   something), and `forget_memory` (only when it actually deleted something) now call
   `a.SignalSync()` after a successful write, so an interactive MCP session converges
   within seconds instead of waiting up to `RECARTO_SYNC_INTERVAL`. Session-start now
   pushes (in addition to the existing pull) so a prior session's un-flushed outbox
   (e.g. one that crashed before its Stop hook fired) catches up at the next session,
   not just at the next Stop. New tests: `TestRecordMemorySignalsSyncLoop`,
   `TestRecordBugSignalsSyncLoop`, `TestCompleteTodoSignalsSyncLoopOnlyWhenCompleted`,
   `TestForgetMemorySignalsSyncLoopOnlyWhenFound`.

Not added: an end-to-end test of the session-start/Stop hook wiring driving a real
`Syncer` against a live server. `internal/remotesync/sync_test.go`'s integration tests
already exercise `Syncer.Pull`/`Push` end-to-end (two-machine convergence, offline
catch-up, bootstrap) via a unix-socket server (this sandbox blocks loopback TCP); doing
the same from `internal/agents` would require adding transport injection to
`remotesync.Client` purely for that test, which felt like more surface than the
two-line hook change (`boundedSync` for `Push`, mirroring the already-shipped `Pull`)
warranted this pass — flagging it rather than silently skipping it.

Verification after round 2: `go build ./...`, `go vet ./...` clean; `gofmt -l .`
clean except the same pre-existing unrelated file; `go test ./...` green except the
same pre-existing `internal/update` `TestHTTPFetcher` failure; `-race` clean on
`internal/memory`, `internal/remotesync`, `internal/syncserver`, `internal/mcp`,
`internal/agents` (+ its `claudecode`/`copilot`/`opencode` subpackages), `internal/app`.
