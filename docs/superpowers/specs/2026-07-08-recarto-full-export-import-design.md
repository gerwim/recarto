# Recarto — Full Export (CLI) + Bulk Import (HTTP)

**Status:** Implemented
**Date:** 2026-07-08

## 1. Problem

[HTTP sync](2026-07-08-recarto-http-sync-design.md) converges memory rows across
machines going forward, but its push path only ever ships a store's **outbox**
(`memory_sync_outbox`), populated solely by local-authoring writes made *after* sync was
configured. A machine with years of pre-existing Recarto history — memories, preferences,
bugs, todos, recorded before HTTP sync ever existed on it — has none of that in its
outbox, so ordinary sync can never move it to a server. That history needs a one-shot
bulk migration path that reads every row directly, bypassing the outbox entirely.

## 2. Owner decisions

- **Scope:** export **everything** — every status (active, archived, done, superseded),
  not just what recall currently surfaces — plus every delete tombstone. Fullest
  history, at the cost that most exported rows won't be recall-surfaced on import (they
  land in whatever status they carry; a superseded/archived row stays superseded/archived).
- **Delivery:** a new `recarto export` CLI command writes the archive; a new
  authenticated bulk server endpoint (`POST /v1/import`) accepts it; `recarto export
  --push` and `recarto import <file>` are convenience uploaders so one command can do
  both steps.
- **Feedback store:** the global "feedback about Recarto itself" store
  (`internal/feedback`, kind=feedback) is included, tagged under its own reserved,
  fixed server key (`_recarto_feedback` — the same sentinel `feedback.StoreID` already
  uses as its local cache-dir name, and it already satisfies the server's `keyPattern`).

## 3. The ID/key question this design answers

There are three distinct identifiers in play, and conflating them is the source of any
doubt about whether this "actually works":

| Identifier | Where | Derived how | Stable across machines? |
|---|---|---|---|
| Local store dir id (`gitctx.RepoID`) | `~/.cache/recarto/<id>/store.db` | `sha256(abs path to .git common dir)[:16]` | No — path-based, machine-local. |
| Memory row id | `memories.id` | `ids.New()`, a UUIDv7 | Yes, but two machines mint *different* ids for "the same" memory — reconciled by content, not id. |
| Server bucket key (`repokey.Hash`) | `/v1/repos/{key}/...`, `/v1/import` entries, `<data-dir>/<key>/store.db` | `sha256(RootCommit)[:16]` (or `RECARTO_SYNC_REPO` override) | Yes — RootCommit is identical in every clone. |

Row ids are never small sequential integers and never collide across machines; the
engine reconciles independently-authored rows by **content** (MinHash near-dup) and by
**explicit id match**, never by an id being "the same number." The thing that actually
keys a shared server bucket is the root-commit hash, and an ephemeral container that
clones the same repo computes it identically — so yes, a fresh clone's ordinary sync
lands in the same bucket a bulk import seeded. The one caveat is unchanged from ordinary
sync: a shallow clone with no reachable root commit must set `RECARTO_SYNC_REPO`, or the
key cannot be resolved at all (fail closed, not a guessed machine-local identity).

## 4. Architecture

```
  machine A (years of local history)              recarto server
  <cache-dir>/<repo-id-1>/store.db  --\           one SQLite store per repo key
  <cache-dir>/<repo-id-2>/store.db  ---> export -> gzip'd NDJSON -> POST /v1/import
  <cache-dir>/_recarto_feedback/store.db /          (fans out per-entry repo_key,
                                                      applies via the SAME merge core
                                                      push already uses)
```

`internal/fullexport.ExportAll` walks every per-repo store directory under the cache
root (`cache.Dir()`), resolves each one's server key from its own `meta` table (written
once by `app.writeMeta` — `repo_name`/`root_commit`), and streams every memory row (any
status) + every tombstone as one gzip'd NDJSON archive. `internal/syncserver`'s new
`POST /v1/import` streams that archive back apart line-by-line and applies each entry
through `applyRecord` — the exact `memory.SyncApplyTx` + `bumpSyncIndexTx` body
`handlePostChanges` already used, now shared by both handlers.

## 5. Wire format — `recarto-export/v1`

Gzip'd NDJSON, one JSON object per line:

```
line 1 (header, informational only, skipped by the importer):
  {"format":"recarto-export/v1","created_at":<unixms>,"recarto_version":"x.y.z",
   "repos":[{"key":"<serverkey>","name":"myrepo"}, ...]}

line 2..N (entry, self-describing — the importer never needs to buffer the archive):
  {"repo_key":"<serverkey>","record":{...syncwire.Record...}}
```

`syncwire.Record` is the same shape `/changes` already uses — a tombstone is
`{"id":...,"deleted":true,"updated_at":...}` with every other field empty, exactly like
an ordinary push's tombstone. New types (`ExportHeader`, `ExportRepoMeta`,
`ExportEntry`, `ImportResponse`, `RepoImportStat`) live in `internal/syncwire` alongside
the existing push/pull types, all with explicit `json` tags matching the field names
above.

Header generation is a separate, lightweight first pass — open each store just far
enough to read its `meta` row and close it again — so the header lists every project up
front while the streaming pass that follows still touches only one store at a time.

## 6. Server endpoint

`POST /v1/import`, same auth as everything else (`Authorization: Bearer
<RECARTO_SERVER_KEY>`, the identical static key `RECARTO_SYNC_KEY` already uses — no new
secret). The repo key travels **per entry** in the body rather than in the URL path
(one archive spans many repos); `storeFor` still validates every key against the same
`keyPattern` before it is ever joined into a filesystem path, so a malformed
`repo_key` in the archive is a 400, not a traversal risk.

Two independent size limits, mirroring `/changes`'s compressed/decompressed pair but
sized for a legitimately large, multi-project archive: `maxImportWireBytes` (64MB,
compressed, enforced by `http.MaxBytesReader` before any decompression) and
`maxImportBytes` (256MB, decompressed, enforced by a further `io.LimitReader` on the
gzip stream) — so a small compressed body cannot "gzip-bomb" past the decompressed cap.

Idempotent exactly like a push: re-importing an unchanged archive accepts 0 for every
entry (`memory.SyncNoop`), because it runs through the identical merge core. A near-
duplicate row may **reinforce** an existing active row rather than land as its own
distinct historical entry — this is Recarto's existing, intentional convergence
behavior (dedup by content), not a defect specific to import; `recarto export`'s CLI
output deliberately says "records accepted," not "records restored," to avoid implying
byte-for-byte table restoration.

## 7. Avoiding an import cycle

`internal/fullexport` needs the same `sha256(RootCommit)[:16]` formula
`internal/remotesync.RepoKey` uses, to guarantee a store's export lands in the exact
bucket its ordinary client push would use. `internal/remotesync`'s own test suite
imports `internal/syncserver` (to run a real handler in-process over a unix socket), and
`internal/syncserver` imports `internal/fullexport` (for the bulk archive reader) — so
`internal/fullexport` cannot import `internal/remotesync` without a test-only import
cycle. The formula is instead factored into a new, dependency-free `internal/repokey`
package that both `remotesync.RepoKey` and `fullexport.serverKeyFor` call, guaranteeing
one formula with no risk of drift and no cycle.

## 8. Components

- `internal/repokey` — the shared `sha256(rootCommit)[:16]` formula.
- `internal/syncwire` — `ExportHeader`/`ExportRepoMeta`/`ExportEntry`/`ImportResponse`/
  `RepoImportStat`, alongside the existing push/pull types.
- `internal/memory` — `AllRecords` (every status, unlike `List`'s active-only default)
  and `AllTombstones` (every tombstone, unlike `PendingTombstones`'s pushed=0 filter).
- `internal/fullexport` — `ExportAll` (write path) and `Read` (streaming NDJSON reader,
  shared by the server's import handler).
- `internal/syncserver` — `handleImport` (new), `applyRecord` (extracted from
  `handlePostChanges`, now shared by both).
- `internal/remotesync` — `Client.Import`.
- `internal/cli` — `Export` (`recarto export`) and `Import` (`recarto import`).

## 9. Scope cuts

- A per-repo store with no resolvable root commit (a shallow clone, or a non-git
  working directory) is skipped and named in the export summary, not failed — the rest
  of the machine's history should still travel. Set `RECARTO_SYNC_REPO` for that project
  and re-run, exactly like ordinary sync requires.
- No tombstone GC and no new store migration — consistent with the HTTP sync design's
  own deferrals; `memory_tombstones`/`sync_index` already exist.
- `internal/export` (markdown rendering) and `internal/snapshot` (the committed
  `.recarto/memory.jsonl` team file, active-only, merged via `ImportMemory`) are
  untouched — this path is deliberately separate, using `syncwire.Record` (the server's
  merge shape) rather than refactoring those already-tested wire shapes.

## 10. ForgeBoard / operational contract

No change to the cross-repo contract beyond the one new in-binary endpoint: the server
is still the same `recarto server` binary, authenticating with the existing
`RECARTO_SERVER_KEY`, and clients still get `RECARTO_SYNC_URL`/`RECARTO_SYNC_KEY`
injected unchanged. The one new operational step is manual and one-time, run by a user
on their own long-lived machine — not part of the ephemeral agent lifecycle:
`recarto export --push` to seed the server from pre-existing history. Ephemeral
containers need no change to benefit: because each accepted import entry bumps the
target repo's `sync_index`/seq exactly like a push, their next ordinary session-start
pull just finds more history already waiting.
