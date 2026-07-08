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
