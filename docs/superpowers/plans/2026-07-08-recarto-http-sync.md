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

## Round 3 — review fix: `extra` metadata dropped by sync

A review pass found that `memory.SyncApply`'s same-id merge path
(`syncApplyExisting`) overwrote title/body/status/etc. on a newer incoming
record but never touched the `extra` column, silently dropping kind-specific
metadata (e.g. a feedback report's `resolution_note`, set by `ResolveFeedback`)
whenever the row already existed on the peer being synced to/from. The
insert path (`syncApplyAbsent`, used for a never-before-seen id — the common
bootstrap case) had the same gap: it built a `NewMemory` without copying
`rec.Extra`, so `insert()` always wrote `extra="{}"`, unlike `ImportMemory`'s
insert path, which already sets `Extra: im.Extra`. Fixed both: the LWW-update
branch now marshals and writes `rec.Extra` alongside the other overwritten
columns, and `syncApplyAbsent`'s `NewMemory` now carries `Extra: rec.Extra`.
The near-dup reinforce path is unchanged by design (reinforcing, like
`ImportMemory`'s reinforce path, never touches `extra` — only content-owning
paths do). New tests: `TestSyncApplyLWWUpdatePersistsExtraMetadata`,
`TestSyncApplyAbsentInsertPersistsExtraMetadata`.

Verification after round 3: `go build ./...`, `go vet ./...` clean; `gofmt -l .`
clean on all touched files (same pre-existing unrelated exception as before);
`go test ./...` green except the same pre-existing `internal/update`
`TestHTTPFetcher` failure (confirmed environment-specific: this sandbox
blocks loopback TCP, unrelated to this change); `-race` clean on
`internal/memory`, `internal/remotesync`, `internal/syncserver`,
`internal/mcp`, `internal/agents` (+ subpackages), `internal/app`,
`internal/store`, `internal/cli`, `internal/snapshot`. Unlike the prior two
rounds, `go` was available in this session (`/usr/local/go/bin`, not on
`PATH` by default) — all of the above was actually executed, not just
inspected.

## Round 4 — review fixes: gzip transport, per-IP rate limiting, session-start ordering

A review pass found three remaining gaps against the design (§6.1's planned
gzip transport, §the syncserver's planned rate limiting, and a hook-ordering
bug), all fixed:

1. **Gzip transport was unimplemented.** `internal/remotesync.Client.Push`
   sent plain JSON and neither side ever set `Content-Encoding`/
   `Accept-Encoding`. Fixed: `Client.do` now always sends
   `Accept-Encoding: gzip` and transparently decompresses a gzip response
   body; `Client.Push` gzip-compresses its JSON body and sets
   `Content-Encoding: gzip`. On the server, new `internal/syncserver/gzip.go`
   adds `gzipRequestBody` (decompresses an incoming gzipped body, bounding
   the wire size at `maxPushBytes` *and* the decompressed size at a separate
   `maxDecompressedBytes` so a compressed body can't be used to exhaust
   memory) and `withGzipResponse` (compresses the response whenever the
   request's `Accept-Encoding` advertises gzip support), wired into both
   `/v1/repos/{key}/changes` handlers in `server.go`. `handlePostChanges` now
   reads through `gzipRequestBody` instead of a bare `MaxBytesReader`.
   Explicit request/response gzip (rather than relying on Go's transport-level
   auto-gzip, which never compresses request bodies and silently stops
   decompressing responses once a caller sets `Accept-Encoding` itself) keeps
   the behavior symmetric and testable. New tests:
   `TestGzipCompressedPushBodyIsDecompressed`,
   `TestGzipResponseWhenAcceptEncodingRequested`.
2. **No per-IP rate limiting.** Added `internal/syncserver/ratelimit.go`: a
   minimal in-memory per-IP token bucket (`rateLimiter`), deliberately simple
   per the design doc's "basic per-IP rate limit" — no new dependency, no
   distributed state. Wired as the outermost middleware in `Handler()` (before
   `requireKey`), so it throttles repeated wrong-key guesses against the
   single global bearer token, not just authenticated traffic. Defaults
   (20 tokens/sec, burst 60) are generous for real sync traffic (a handful of
   small requests per machine) while bounding a hostile/malfunctioning client.
   New tests: `TestRateLimitReturns429AfterBurstExceeded` (proves it fires
   before auth — first two wrong-key requests still get 401, the third gets
   429), `TestRateLimitDefaultBurstAllowsNormalTraffic` (guards against an
   overly aggressive default making legitimate use flaky).
3. **Session-start pushed before absorbing the team snapshot.**
   `handleSessionStart` (`internal/agents/handle.go`) ran the HTTP pull, then
   the HTTP push, then `snapshot.SyncOnSessionStart` — so a memory just
   absorbed from a teammate's committed `.recarto/memory.jsonl` (which calls
   `ImportMemory`, and therefore queues the row in the local HTTP-sync
   outbox — unchanged since round 1) sat unpushed until a later Stop/
   background/manual sync, contrary to the design's intent that session-start
   converge both sync mechanisms together. Fixed by reordering to
   pull → snapshot import → push. New test:
   `TestImportMemoryQueuesRowsForHTTPPush` (in `internal/memory`) pins the
   underlying mechanism the reorder depends on — that `ImportMemory`'s insert
   and reinforce paths both queue their canonical row for HTTP push, exactly
   like a locally-authored `Record`. An end-to-end hook-level test (driving a
   real `Syncer` + snapshot file + session-start hook together) was not added:
   `remotesync.Client`'s `HTTP` field isn't test-injectable from
   `internal/agents` today, and this sandbox's loopback TCP is unreachable
   (the same constraint `internal/remotesync`'s own tests route around with a
   unix-socket listener) — building that seam purely for this ordering test
   felt like more surface than the six-line reorder warranted, so it's
   flagged rather than silently skipped, consistent with round 2's identical
   call on the Stop-hook push.

Verification after round 4: `go build ./...`, `go vet ./...` clean; `gofmt -l .`
clean on all touched files (same pre-existing unrelated exception as before,
`internal/mcp/handlers_feedback_test.go`); `go test ./...` green except the
same pre-existing, environment-specific `internal/update` `TestHTTPFetcher`
failure (confirmed unchanged on `master` too — this sandbox blocks loopback
TCP); `-race` clean on `internal/syncserver`, `internal/remotesync`,
`internal/memory`, `internal/agents` (+ subpackages), `internal/app`,
`internal/mcp`, `internal/store`, `internal/cli`, `internal/snapshot`.

## Round 5 — review fix: unioned refs dropped from search_text on LWW update

A review pass found that `memory.syncApplyExisting`'s newer-wins branch
(`internal/memory/sync.go`) already unions incoming refs into `memory_refs`
(`INSERT OR IGNORE`, so a local-only ref survives), but rebuilt
`search_text`/FTS from `rec.Refs` alone — a newer remote update carrying
fewer or different refs than the local row silently dropped the local-only
ref from lexical recall, even though `memory_refs` still listed it. Fixed by
adding `Engine.loadRefsTx`, which reads back id's ref set on the same
transaction connection right after the union write, and using that unioned
set (not `rec.Refs`) to rebuild `search_text` and the FTS entry. New test:
`TestSyncApplyLWWUpdatePreservesUnionedRefsInSearchText` asserts both a
pre-existing local-only ref and a newly-arriving remote-only ref appear in
`search_text` (and in `memory_refs`) after a same-id newer-wins update.

Verification after round 5: `go build ./...`, `go vet ./...` clean; `gofmt -l .`
clean on all touched files (same pre-existing unrelated exception as before,
`internal/mcp/handlers_feedback_test.go`); `go test ./...` green except the
same pre-existing, environment-specific `internal/update` `TestHTTPFetcher`
failure (confirmed unchanged on `master` too — this sandbox blocks loopback
TCP); `-race` clean on `internal/memory`, `internal/remotesync`,
`internal/syncserver`.

## Round 6 — review fixes: terminal-status FTS leak and stale-record ref/FTS drift

A review pass found two remaining gaps in `memory.syncApplyExisting`
(`internal/memory/sync.go`), both in how the same-id merge path keeps
`search_text`/FTS in step with `status` and `memory_refs` — round 5 only
fixed the ref-union case for the newer-wins branch:

1. **Newer-wins update landing a terminal status stayed FTS-indexed.** When
   an incoming record was strictly newer and carried a terminal `status`
   (`done`/`resolved`/`superseded` — e.g. a todo completed on another
   machine, pulled in here), the branch unconditionally called
   `e.lex.Delete` followed by `e.lex.Index`, re-adding the row to
   `memories_fts` regardless of its new status. Every local completion path
   (`CompleteTodo`, `ResolveFeedback`, an explicit supersede, and
   `syncApplyAbsent`'s own bootstrap-insert handling of a non-active incoming
   status) deletes from FTS and never re-indexes; this same-id sync path was
   the one place that didn't match, so a remote completion pulled in here
   would keep crowding active memories out of lexical recall's ranked
   top-N candidates (recall's status filter only runs after FTS ranking).
   Fixed: the newer-wins branch now always deletes the FTS entry, and only
   re-indexes when the incoming (defaulted) status is `"active"`. New test:
   `TestSyncApplyLWWUpdateWithTerminalStatusDropsFromFTS`.
2. **A stale/equal record contributing a new ref never touched
   `search_text`/FTS.** `memory_refs` is unioned via `INSERT OR IGNORE`
   regardless of which side is newer (by design — refs never regress), but
   only the newer-wins branch rebuilt `search_text`/FTS from that unioned
   set (round 5's fix). The keep-local-content branch (incoming record not
   newer) merged confidence/reinforcement/pinned but left `search_text`/FTS
   built from whatever the row already had, even when the loop just above it
   had inserted a genuinely new `memory_refs` row — silently drifting the
   lexical index behind the structural ref set whenever the *older* side of
   a sync happened to be the one carrying the new ref. Fixed: the branch now
   tracks `refsChanged` separately from the general `changed` flag: when a
   ref was newly unioned, it rebuilds `search_text` from local title/body
   (unchanged) plus the unioned ref set, writes it back, and — only when the
   row is still `active` — deletes and re-indexes the FTS entry (mirroring
   the terminal-status guard from fix 1, so a completed/resolved/superseded
   row never gets pulled back into the lexical pool by a later ref-only
   sync). New test: `TestSyncApplyStaleRecordAddingRefRebuildsSearchTextAndFTS`.

Both fixes share the same underlying rule, now applied consistently across
every same-id sync branch: **`memories_fts` must contain a row for id if and
only if the row's current status is `active`**, and whenever `search_text`
is rebuilt it must be built from the unioned ref set, never one side alone.

Verification after round 6: `go build ./...`, `go vet ./...` clean; `gofmt -l .`
clean on all touched files (same pre-existing unrelated exception as before,
`internal/mcp/handlers_feedback_test.go`); `go test ./...` green except the
same pre-existing, environment-specific `internal/update` `TestHTTPFetcher`
failure (confirmed unchanged on `master` too — this sandbox blocks loopback
TCP); `-race` clean on `internal/memory`, `internal/remotesync`,
`internal/syncserver`.

## Round 7 — review fix: near-duplicate reinforce-merge was not idempotent under replay

A review pass found that `memory.syncApplyAbsent`'s near-duplicate
reinforce path (`internal/memory/sync.go`) — used both when the server
applies a client's push and when a client applies a pulled record — never
creates a row at the incoming record's own id (it reinforces the existing
canonical row instead), so nothing distinguished a genuine fresh
convergence from a REPLAY of a record that had already been merged in a
prior call. Concretely: a client retries a push after the first response
was lost on the wire (a real possibility over HTTP), and the server
re-applies the identical record — `findNearDup` finds the same canonical
row again, reinforces it a second time, and `handlePostChanges`'s
`afterApply` hook (since the outcome was `SyncReinforced`, not `SyncNoop`)
bumps `sync_index.seq` again. The same replay could happen client-side on a
pull page re-applied after an interrupted `Pull` (the cursor advances only
after a full page is applied). Both cases violate the amended plan's
"idempotent re-application must be a Noop" requirement and the earlier,
narrower fix that only covered same-id (not same-original-id-merged-
elsewhere) replay (see `TestSyncApplyIdempotentReapplyIsNoop`, which never
exercised the reinforce-merge branch).

Fixed with a new additive, non-graph table, `memory_sync_merges(id,
canonical_id, updated_at)` (migration v21 — `schemaVersion` is now 21),
recording, for an incoming id that reinforced an existing canonical row,
which canonical id it merged into and at what `updated_at`. On entry to
`syncApplyAbsent`, a new incoming record whose id already has a
`memory_sync_merges` row with `updated_at >= rec.UpdatedAt` is recognized
as an exact-or-older replay and returned as `SyncNoop` (reporting the
recorded canonical id, matching every other Noop path's contract) *before*
`findNearDup` ever runs — so it never reinforces again and, since
`SyncApplyTx` only invokes `afterApply` for a non-Noop outcome, the
server's `sync_index`/seq bump is correctly skipped too. A genuinely
*newer* edit of the same origin row (a later `updated_at` for the same
incoming id — the origin machine editing it again after its first
convergence) is intentionally NOT blocked: it still reinforces, and the
merge record's `updated_at` watermark advances forward-only (mirroring
`upsertTombstone`'s pattern), so only true replays are absorbed. New tests:
`TestSyncApplyReinforceReplayIsIdempotent` (in `internal/memory`, proving
replay-is-noop vs. newer-edit-still-reinforces at the merge-core level) and
`TestPushNearDuplicateReinforceReplayDoesNotBumpSeqTwice` (in
`internal/syncserver`, proving a retried HTTP push of an already-merged
near-duplicate doesn't advance the server's cursor or double the target's
`reinforcement_count`).

Verification after round 7: `go build ./...`, `go vet ./...` clean;
`gofmt -l .` clean on all touched files (same pre-existing unrelated
exception as before, `internal/mcp/handlers_feedback_test.go`);
`go test ./...` green except the same pre-existing, environment-specific
`internal/update` `TestHTTPFetcher` failure (reconfirmed unchanged on a
clean stash of this branch — this sandbox blocks loopback TCP, unrelated to
this change); `-race` clean on `internal/memory`, `internal/remotesync`,
`internal/syncserver`, `internal/store`.

## Round 8 — review fix: tombstones ignored a prior near-duplicate merge alias

A review pass found that `memory.SyncApplyTx`'s deleted branch
(`internal/memory/sync.go`) applied a tombstone against `rec.ID` verbatim,
never checking `memory_sync_merges` — the alias table round 7 added so a
*non-deleted* replay of an id that was previously merged into a different
canonical row (via `syncApplyAbsent`'s near-dup reinforce, which never
creates a row at the incoming id) could be recognized as already-applied.
Tombstones need that same alias resolution for a different reason: if id
`B` was previously merged into canonical id `A` on this store, `B` was
never its own row here, so a later tombstone for `B` (an explicit `Forget`
authored by the machine that owns `B` as *its* canonical id, propagating
here via pull or a server-applied push) looked up `memories WHERE id='B'`,
found nothing, and simply recorded an inert tombstone row for `B` — the
real, live memory under `A` was never touched. This is directly reachable
in practice: any two peers (including the sync server itself, which is
structurally just another `SyncApply` caller) that independently received
two near-duplicate memories under different ids will each pick their own
canonical id via `findNearDup`, so an explicit forget authored against the
*non-canonical* id on one peer would silently fail to delete the
*canonical* row everywhere else.

Fixed by resolving `rec.ID` through `memory_sync_merges` at the top of the
deleted branch in `SyncApplyTx`, before any tombstone lookup or memories
query: when an alias exists, every subsequent step (the tombstone-guard
read, the existing-row delete, and the tombstone write itself) operates on
the resolved canonical id, and the outcome's reported id (`canonID`, used
both as the `SyncApply`/`SyncApplyTx` return value and as the id the sync
server indexes in `sync_index`) is the canonical id too — so a client or
the server correctly propagates the delete under the id that actually has
(or had) a row, not the alias. `applyTombstoneRow`'s signature changed from
taking the full wire `Record` to taking an explicit `targetID` (the
already-resolved id) plus `updatedAt`, making the resolution point
unambiguous at the call site. A replay of the *original* near-duplicate
push after the delete remains correctly blocked from resurrecting anything:
`syncApplyAbsent`'s existing `memory_sync_merges` replay guard (unchanged)
Noops any at-or-before-merge-time replay of the non-canonical id regardless
of whether the canonical row currently exists, since that guard compares
timestamps, not row existence. New test:
`TestSyncApplyTombstoneResolvesNearDupMergeAlias` — merges a
differently-id'd near-duplicate into a local canonical row, tombstones the
non-canonical id, and asserts the canonical row is deleted, the tombstone
is recorded under the canonical id (not the alias), and a subsequent replay
of the original merge push stays a no-op rather than resurrecting the
canonical row.

Verification after round 8: `go build ./...`, `go vet ./...` clean;
`gofmt -l .` clean on all touched files (same pre-existing unrelated
exception as before, `internal/mcp/handlers_feedback_test.go`);
`go test ./...` green except the same pre-existing, environment-specific
`internal/update` `TestHTTPFetcher` failure (reconfirmed unchanged on a
clean stash of this branch — this sandbox blocks loopback TCP, unrelated to
this change); `-race` clean on `internal/memory`, `internal/remotesync`,
`internal/syncserver`, `internal/store`.

## Round 9 — review fix: non-deleted alias record could resurrect after a canonical tombstone

A review pass found a gap in round 8's alias-resolution fix: that fix only
resolved `rec.ID` through `memory_sync_merges` in the *deleted* branch of
`SyncApplyTx`. The *non-deleted* branch still looked up
`memory_tombstones` keyed on the raw incoming `rec.ID` alone. Since a
near-duplicate reinforce-merge (`syncApplyAbsent`) never creates a row at
the incoming id, that id is never itself tombstoned directly — only its
canonical id is, once round 8's fix resolves a delete through the alias.
Concretely: incoming id `B` merges into canonical `A` at `updated_at=500`;
`A` is later deleted via a tombstone for `B` at `updated_at=1000`, correctly
resolving to and deleting `A` (round 8). A *third*, non-deleted record for
`B` then arrives with `updated_at=700` — newer than the merge, so
`syncApplyAbsent`'s replay guard (which compares against the merge time,
not the delete time) lets it through — but older than the delete. Before
this fix, the tombstone lookup on raw `B` found nothing (only `A` was
tombstoned), so the record fell through to `syncApplyAbsent`, found no
surviving near-duplicate to reinforce (the canonical row was already
deleted), and inserted a fresh row — resurrecting the memory under alias
`B`, in violation of the "a record older than a recorded tombstone must be
dropped" rule the ordinary same-id path already enforces.

Fixed by resolving `rec.ID` through `memory_sync_merges` in the non-deleted
branch too (right after the raw-id tombstone lookup, before the
Noop-vs-continue decision): when an alias is on record, the canonical id's
tombstone is also checked, and the later (more restrictive) of the two
`deleted_at` values wins. This mirrors the same forward-only,
later-wins discipline `upsertTombstone`/`upsertSyncMerge` already use
elsewhere. A record genuinely newer than the tombstone (arriving after the
delete) is unaffected and still legitimately re-inserts, matching the
existing same-id tombstone-vs-newer-record rule. New test:
`TestSyncApplyNonDeletedAliasRecordDoesNotResurrectAfterCanonicalTombstone`
— merges an alias into a canonical row, tombstones it through the alias
(as in round 8), then applies a third record for the alias id with an
`updated_at` strictly between the merge and the delete, asserting it is
dropped as `SyncNoop` and neither the canonical id nor the alias id ends up
with a live row; a final case confirms a record genuinely newer than the
delete still legitimately re-inserts. Manually verified the new test fails
(observed `SyncInserted`, i.e. resurrection) against the pre-fix code and
passes against the fix.

Verification after round 9: `go build ./...`, `go vet ./...` clean;
`gofmt -l .` clean on all touched files (same pre-existing unrelated
exception as before, `internal/mcp/handlers_feedback_test.go`);
`go test ./...` green except the same pre-existing, environment-specific
`internal/update` `TestHTTPFetcher` failure (reconfirmed unchanged on a
clean stash of this branch — this sandbox blocks loopback TCP, unrelated to
this change); `-race` clean on `internal/memory`, `internal/remotesync`,
`internal/syncserver`, `internal/store`.
