# Recarto — `todo` Memory Kind

**Date:** 2026-06-05
**Status:** Approved design

## Summary

Add a sixth durable memory kind, `todo`, for **open work the agent should pick
up later**. Unlike the existing `action` kind — a *past-tense*, "disposable
activity log: fastest decay" that is deliberately excluded from session-start
recall — a `todo` is *future-tense*: it keeps surfacing until it is explicitly
completed or fades from neglect. The two kinds are temporal mirror images:
`action` = "I did X, forget it soon"; `todo` = "X still needs doing, remind me
until it's done."

A `todo` unifies three uses the current kinds don't cover:

1. **Open tasks with a completion lifecycle** — surface while open, stop once done.
2. **Deferred follow-ups** — "remember to do X" notes that decay if never revisited.
3. **Code-anchored todos** — tied to a file/symbol, resurfaced on code contact.

## Data Model

### New kind
- `KindTodo Kind = "todo"` in `internal/memory/record.go`, added to `validKinds`.

### New status
- A new `'done'` status, distinct from `'active'`, `'superseded'`, `'archived'`.
- `done` ≠ `archived` so we can distinguish a **finished** todo from one that
  **faded** via decay.

### Lifecycle

```
            ┌─────────── reinforce (record near-dup) ──────────┐
            ▼                                                   │
        active ──── complete_todo / recarto todo done ────▶ done
            │
            └──────── decay (never reinforced) ──────────▶ archived
            │
            └──────── superseded by a newer memory ──────▶ superseded
```

- `active` → `done`: explicit completion (terminal).
- `active` → `archived`: compaction decay when untouched (terminal; still dedup-able).
- `active` → `superseded`: an incoming memory with `SupersedesID` pointing at the
  todo, via the existing supersede machinery.

## Migration (v12)

Both the `kind` and `status` CHECK constraints live on the `memories` table and
must each admit a new value. SQLite cannot `ALTER` a CHECK, and the constraints
on a rebuild are awkward here: migrations run inside a single `BEGIN IMMEDIATE …
COMMIT` (so `PRAGMA foreign_keys=OFF` is a no-op), and connections set
`foreign_keys(ON)`, so dropping/rebuilding `memories` would cascade-delete
`memory_refs` and `memory_minhash`.

**Chosen approach: edit the stored schema via `writable_schema`** — contained, no
data movement, no FK cascade, children/FTS untouched, idempotent.

```sql
PRAGMA writable_schema=ON;
UPDATE sqlite_master
   SET sql='<memories CREATE TABLE text with both CHECKs widened>'
 WHERE type='table' AND name='memories';
PRAGMA writable_schema=OFF;
```

- `kind` CHECK gains `'todo'`; `status` CHECK gains `'done'`.
- The `user_version` bump that `migrateOnce` already performs forces the schema
  reparse on subsequent connections.
- Re-running writes the identical SQL, so the migration stays idempotent (the
  property `TestLatestGraphMigrationRebuildsIndex` expects of the last migration).
- Memories/refs/minhash rows are preserved (assert via a test mirroring
  `TestV9MigrationPreservesMemories`).

Rejected alternatives:
- **Full table rebuild** (`foreign_keys=OFF` + create/copy/drop/rename): would
  require restructuring `migrateOnce` to run this migration outside its wrapping
  transaction and risks the FK children. Too invasive for a CHECK widening.
- **Avoid a new status** (`extra.done=true` + `archived`): splits "is it done?"
  across two fields and reuses `archived`, which we explicitly want to keep
  distinct from "finished."

## Surfacing — "Both"

### Session start
- Add `todo` to the baseline recall kind set (`baselineIDs` in
  `internal/memory/recall.go`, currently
  `('preference','learning','decision','do_not_repeat')`) so open,
  branch-visible todos inject in the budgeted session-start payload.
- `done`/`archived`/`superseded` are excluded automatically because `baselineIDs`
  filters `status='active'`.
- Also add `todo` to the task-hinted recallable-kinds list (recall.go around the
  `kind IN (...)` filter) so an explicit recall query can return open todos.

### Just-in-time on code contact
- New `TodosForCode(ctx, branch, files, symbols)` mirroring `BugsForCode`
  (`internal/memory/bugsurface.go`): active `kind='todo'` rows whose refs match a
  touched file/symbol, branch-visible, most-recently-updated first.
- Wire into the same hook path that surfaces bugs on read/edit/grep so a
  code-anchored todo reappears the moment its code is touched.
- Code-anchored todos do **not** auto-resolve when their ref dangles — a moved or
  deleted file does not mean the work is done. (The existing worktree-aware
  dangling-ref demotion/caveat still applies as for any memory.)

## Completion

- **`complete_todo(id)` MCP tool** + **`recarto todo done <id>` CLI**, both calling
  one engine method `CompleteTodo(ctx, id)` that transitions `active → done`.
  (A `recarto todo list` read command is a nice-to-have, decided during planning.)
- **Decay if untouched:** add `KindTodo` to `evictable` (`internal/memory/compact.go`)
  with the **standard 30-epoch half-life** (`halfLifeStdEpochs`), slower than
  `action`'s 5 epochs. An untouched todo eventually archives; recording a
  near-duplicate reinforces and refreshes it.
- **Supersede-on-record:** already handled by the existing `SupersedesID` path; a
  newer memory that supersedes a todo closes it (`status='superseded'`).

## Plumbing To Update

- `validKinds` (record.go).
- Baseline + task-hinted recall kind sets (recall.go).
- `remember` CLI kind list + error/help string (`internal/cli/remember.go`).
- `record_memory` MCP tool description (`internal/mcp/server.go`).
- Agent instructions/format so agents know the `todo` kind and the complete tool
  (`internal/agents/instructions.go`, `internal/agents/format.go`).
- Snapshot export/import: **no kind-specific change** — it dedups by content and
  exports only active rows, so `done`/`archived` todos are naturally excluded and
  a new active kind flows through unchanged. (Confirm with an assertion during
  planning rather than new code.)

## Testing (TDD throughout)

1. **Migration v12** — CHECK now accepts `kind='todo'` and `status='done'`;
   existing memory/ref/minhash rows preserved; migration idempotent on re-run.
2. **Record + session-start recall** — an active todo surfaces in the baseline
   payload for its branch; a `done` todo does not.
3. **`TodosForCode`** — a code-anchored todo resurfaces when its file/symbol is
   among the touched refs; unrelated refs do not surface it.
4. **`CompleteTodo`** — transitions `active → done`; a completed todo no longer
   surfaces at session start or on code contact.
5. **Decay** — an untouched todo archives after enough epochs at the 30-epoch
   half-life; reinforcing it resets that.

## Out Of Scope

- Auto-resolving code-anchored todos on dangling refs (explicitly excluded).
- Priorities, due dates, or assignees on todos.
- A dedicated todo UI beyond the CLI/MCP surfaces above.
