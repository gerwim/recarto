# LLM-UX Round 2 — Design

Four features improving Recarto's cross-session loop, designed with and approved by the user
on 2026-06-10. Successor to the round-1 plan (`docs/superpowers/plans/2026-06-10-llm-ux-improvements.md`).
Origin: the LLM-user feedback session that produced round 1 — these are the four items that
needed a design pass.

Post-merge note: this branch now includes master's bounded non-blocking freshness
(`EnsureFresh`, 3s wait, background refresh, hard session-start index budget). The delta
design below accounts for it.

## 1. Session-end capture nudge — gated block-once

**Problem.** The capture side of the memory loop leaks: preferences and learnings surface
mid-conversation and are forgotten by session end. The existing `handleStop` nudge fires on
*every* stop with static text via `systemMessage` — a channel Claude Code shows to the human,
never to the model, so it nudges the wrong party and trains the human to ignore it.

**Design.**
- `handlePostEdit` bumps a per-session edit counter: `Ledger.Incr(ctx, ev.SessionID, "edits", 1)`
  (the ledger already supports arbitrary scopes; "lifetime" is just the conventional one).
- `handleStop` gating, in order:
  1. `ev.StopActive` (Claude Code's `stop_hook_active` loop guard) → pass through silently.
  2. Already nudged this session (ledger flag `nudged` under the session scope) → pass through.
  3. Edits this session < 3 → pass through.
  4. Memories recorded this session == 0 → NUDGE. Count: `SELECT COUNT(*) FROM memories
     WHERE updated_at >= session.started_at AND status = 'active' AND (branch IS NULL OR
     branch = ?)` — no schema change. `updated_at` (not `created_at`) so reinforcements
     count: Record on a near-dup bumps only `updated_at`, and "record freely — near-dups
     converge" is the system's own contract, so a converged record must satisfy the nudge
     exactly like a fresh insert. Inserts stamp `updated_at = created_at` and count through
     the same predicate; the status filter keeps archive/supersede/resolve/done bumps out.
     Accepted fail-quiet noise (nudge skipped, never falsely fired): a session-start compact
     absorb, or a concurrent session recording on the same branch.
- The nudge **blocks the stop once**: the Claude Code adapter encodes
  `{"decision": "block", "reason": "<nudge text>"}` for `KindStop` when the handler result
  asks for it (new `Result.Block bool` + `Result.Reason string`; other adapters ignore Block
  and keep their current behavior). Nudge text:
  *"You edited N files this session and recorded nothing. Record learnings, preferences, or
  bug fixes now (record_memory / record_bug) — or finish if there's genuinely nothing to keep."*
- Mark `nudged` in the ledger before returning, so the follow-up stop passes through even
  if the model records nothing.
- The unconditional every-stop `SystemMessage` is replaced by the block-once model nudge; the static human-facing reminder is retained ONLY as the no-block fallback (edits < 3, memories already recorded, or any fault while deciding the nudge — every fault degrades to the reminder, never to silence and never to a false block) — without it, conversation-heavy low-edit sessions, exactly where preference corrections surface, would get no capture prompt at all. The already-nudged follow-up stop passes through silently: the model just saw the block reason, so re-showing the reminder would be noise.
- `sessions.ended_at` keeps being set on every stop, as today.

**Scope.** Claude Code adapter only. OpenCode/Copilot keep current pass-through behavior.

## 2. Graph delta on resume — journal + budgeted tier + drill-down tool

**Problem.** Resuming a branch, the model knows what it *learned* (memories) but not what
*moved* (code). It reconstructs that from git by hand — token-expensive and lossy — and risks
acting on stale knowledge.

**Design — journaling.**
- New table (migration v19, `schemaVersion = 19`):
  ```sql
  CREATE TABLE graph_deltas (
    id             TEXT PRIMARY KEY,
    worktree       TEXT NOT NULL,
    path           TEXT NOT NULL,
    qualified_name TEXT NOT NULL,
    kind           TEXT NOT NULL,
    change         TEXT NOT NULL CHECK (change IN ('added','removed','changed')),
    at             INTEGER NOT NULL
  );
  CREATE INDEX idx_graph_deltas_wt_at ON graph_deltas(worktree, at);
  ```
- Written inside `indexWorklist`'s per-file transaction: before replacing a file's nodes,
  diff old vs new node sets by `(qualified_name, kind)` — present-only-in-old → `removed`,
  only-in-new → `added`, in both with different `(start_byte, end_byte, signature)` →
  `changed`. A file's *first* index in a worktree journals nothing (no old set — covers fresh
  clones and wipe migrations), with one carve-out: a first index at a path that already has
  journal rows is a delete-then-recreate (branch switch, stash pop) and journals its symbols
  as `added`, so the earlier `removed` folds to a present disposition instead of standing as
  false. File deletions journal each removed symbol. Renames remain a known blind spot: the
  old path journals removals, the new path is a genuine first index — the tool description
  carries the caveat.
- Retention: on session start, prune rows older than the 10th-most-recent session's
  started_at for that worktree (keeps the journal bounded without a config knob) — clamped
  to the current baseline, because the baseline walk is unbounded: any run of consecutive
  no-op sessions, however long, must not erase the deltas the tier is still reporting.

**Design — baseline (the /clear rule, per user).** The delta a session shows is "changes
since the last session that did real work," not "since the previous session row":
- Walk this worktree's sessions backwards from the current one; the baseline is the
  `started_at` of the most recent prior session whose `[started_at, ended_at|now]` window
  contains ≥ 1 `graph_deltas` row.
- A `/clear` or open-and-close session contains no deltas, so it is skipped — the next
  session shows the same delta again. The tier therefore stays visible until a context has
  lived alongside the changes, and is omitted entirely when no prior session has deltas.

**Design — session-start tier.** One more tier in `formatSessionContext`, after the code
map, hard-capped at 150 estimated tokens (the existing `ledger.EstimateTokens` budgeting):
```
Changed since your last working session here (Jun 8):
- internal/mcp: +4 ~2 (find_references, get_graph_delta, …)
- internal/parse: ~1
(get_graph_delta lists all 7)
```
Per-module `+added ~changed -removed` counts (module = same grouping as the code map), up to
3 symbol names for the heaviest module, title-only degradation under budget pressure, tier
omitted when empty. Because session-start indexing is budget-bounded and refreshes can finish
in the background, the tier renders what is journaled *at render time*; the trailing
`(get_graph_delta lists all N)` line makes the drill-down discoverable.

**Design — drill-down tool.** MCP tool `get_graph_delta`:
- Input: `limit` (default 50). No baseline parameter in v1 — it answers exactly the tier's
  question, using the same baseline walk.
- Output: `{baseline_at, count, total, truncated, deltas: [{path, qualified_name, kind,
  change}]}` — list conventions identical to the other tools (qualified_name only, honest
  truncation). Plus `hint` for freshness, matching the post-merge pattern.

## 3. Contradiction candidates on record — surface, don't decide

**Problem.** Near-duplicates reinforce, but *contradictions* (same topic, opposite claim)
sit in the similarity middle band and are silently kept alongside the memory they negate.
A future session may recall the stale one. `supersedes_id` exists but relies on the recorder
searching first, which doesn't reliably happen.

**Design.** Judgment stays in the recording LLM; Recarto only surfaces suspects, on the
write path where they can't be missed:
- `Engine.Record` gains a conflict-candidate pass. `findNearDup` already gathers LSH band
  candidates and computes exact Jaccard; candidates with Jaccard in `[0.25, dedupThreshold)`
  are kept instead of discarded. Additionally, same-kind active memories sharing ≥ 1
  file/symbol ref with the new memory qualify (refs are the structural "same topic" signal).
- Applies only to judgment-bearing kinds: preference, decision, do_not_repeat, learning.
  Bugs and todos are factual/transient and excluded.
- Cap 3, ranked by Jaccard. Returned as `[]ConflictCandidate{ID, Title, Snippet (first ~160
  chars of body), Reason ("similar wording" | "shares refs")}` from `Record`; the recording
  insert/reinforce is unchanged and atomic — candidates are advisory and computed inside the
  same transaction's read snapshot.
- Reinforced records return no candidates (the near-dup IS the match).
- MCP `record_memory` output gains `conflict_candidates` plus, when non-empty, a `note`:
  *"Review these existing memories — if your new memory supersedes or contradicts one, call
  forget_memory(old_id), or re-record with supersedes_id. If they coexist, no action."*
- CLI `recarto remember` prints the same candidates (shared engine return, small render).

## 4. CLI parity — mechanical

`internal/cli/graph.go` additions, mirroring round 1's MCP features:
- `graph refs <symbol> [--limit=<n>]` — renders find_references: `path:line kind from [test]`
  per hit, `(text)` matches show the line text, `resolved`/`truncated` summary line.
- `graph grep --offset=<n>` — passes through to `ContentSearchOpts.Offset`.
- `graph snippet` prints a `// path:start-end` header line above the source.
- `graph callers/callees` annotate test hops with `[test]` and print the result `hint`
  (all-test-callers, dead-ends) when present.
- Help text updated accordingly. The agents instructions template is NOT changed (MCP
  guidance already covers the concepts; CLI help is the CLI's documentation).

## Cross-cutting

- **Order of implementation:** CLI parity → graph delta (owns the migration) → session-end
  nudge → contradiction candidates. Independent seams; delta lands its migration before the
  nudge touches sessions queries only additively.
- **Migrations:** exactly one new migration (v19, the `graph_deltas` table). It does NOT
  wipe the graph (no parser change). Guard tests updated per the established convention
  (`TestSchemaVersionIs18` → 19; the rebuild test keys off graph-wipe migrations and v19
  isn't one — verify it asserts on the right migration).
- **Token discipline:** the only new ambient cost is the delta tier (≤150 tokens, often 0).
  Everything else is response-side (paid only when the tool is called) or hook-side (one
  blocked stop per session at most).
- **Testing:** house conventions — engine tests on real temp stores; hook tests through
  `RunHook` + adapter fixtures (`source`, `stop_hook_active` already decoded); CLI tests
  assert rendered output; migration guard tests per `internal/store` convention.
- **Out of scope:** other-host nudge adapters; delta baselines by arbitrary git ref;
  polarity heuristics or LLM calls inside Recarto; CLI batch snippets (a human can run the
  command twice).
