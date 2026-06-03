# Recarto — Session Context Injection & Bug Visibility — Design Spec

_Date: 2026-06-03 · Branch: `feat/session-context-injection`_

## 1. Goal & guiding principle

Make recarto orient Claude **early** — surface the right memories and a code map at the
start of a session, and surface prior bugs the moment Claude touches the code that had
them — **in order to save tokens**.

The guiding principle: **session-start injection is a budgeted bet.** Every token injected
costs context *now* to save (hopefully more) tokens *later* by avoiding rediscovery and
re-reads. The objective is **net savings = (tokens saved by not rediscovering) − (tokens
spent injecting)**. Therefore we inject only high-probability, high-value items, under a
hard, tunable budget, with every tier **trimmable** so it degrades instead of overflowing.

Corollary that shapes the bug design: the most token-efficient way to expose knowledge is
**just-in-time** (at the moment of need), not preloaded. Recarto's `augment` hook already
proves this for code navigation; we extend the same idea to bugs.

## 2. Current behaviour (baseline)

- `claudecode.SessionStart` (`internal/agents/claudecode/hooks.go`) calls
  `Engine.Recall(RecallQuery{Branch, TokenBudget: 1500})` with an **empty query**.
- `Engine.Recall` (`internal/memory/recall.go`) builds its candidate set from the union of
  lexical FTS matches, structural (file/symbol) matches, and **pinned `do_not_repeat`**.
  With an empty query, only pinned do-not-repeats remain → SessionStart injects nothing
  else.
- `bug add` (`internal/cli/bug.go`) records a `kind=bug` memory but only `--file` becomes a
  structural ref; there is **no `--symbols`** flag.
- Bugs are only retrievable via `search_bugs` / `bug search` — which relies on Claude
  remembering to search. Nothing surfaces a bug when Claude reads/edits the relevant code.
- `memory_refs` matches `ref_value` **exactly** (`recall.go` `structuralScores`), and refs
  are stored as-given on write (`engine.go`). Hooks receive **absolute** file paths, so
  matching requires repo-relative normalization.

## 3. Design

### 3.1 Session-start context (tiered, budgeted)

`SessionStart` builds one `additionalContext` block from up to four tiers, filled in
priority order until the budget is spent. Budget default **1500**, overridable via
`RECARTO_SESSION_BUDGET`.

| Tier | Content | Render | Budget |
|---|---|---|---|
| 1. Pins | pinned `do_not_repeat` | title + body | exempt (as today) |
| 2. Cerebrum | active, branch-visible `preference` / `learning` / `decision` (+ unpinned `do_not_repeat`), ranked by recency + reinforcement | title + trimmed body (~1 line); degrades to **titles-only** then drops under budget pressure | bulk of budget |
| 3. Orientation map | module rollup from `Engine.Architecture` | compact (a few lines) + one nudge line | small reserved slice (~10%, ~150 tok) |
| 4. Recent bugs | the few most-recent `kind=bug` titles (ambient awareness) | **titles-only** | small reserved slice (~5%, ~75 tok) |

Excluded from Tier 2: `action` (ephemeral) and `bug` (handled by Tier 4 + just-in-time
surfacing). Empty reserved slices (3/4) return their budget to Tier 2. The nudge line:
*"Query the code graph (search/snippet/callers) instead of reading whole files; grep/glob
auto-injects matching symbols."*

### 3.2 Baseline recall (engine)

Add `Baseline bool` to `RecallQuery`. When set (only `SessionStart` sets it), `Recall`
additionally seeds its candidate id-set from a new helper:

```
baselineIDs(ctx, branch, limit) → ids of active, branch-visible memories
  WHERE kind IN ('preference','learning','decision','do_not_repeat')
        AND status='active' AND (branch IS NULL OR branch = ?)
  ORDER BY updated_at DESC LIMIT ?   -- limit = defaultLimit (50)
```

These flow through the **existing** scoring loop and `selectWithinBudget` unchanged. With
no query, structural/lexical signals are 0, so ranking falls to recency (0.15) +
reinforcement (0.10) + branch (0.05) — exactly the "cerebrum importance" proxy we want. No
new scoring weights → existing golden tests stay stable. Query-driven recall and the
`recall_misses` metric are untouched (the flag is off for them).

### 3.3 Just-in-time, code-anchored bug surfacing

Bugs surface **when Claude touches the code that had them**, reusing structural refs.

- New engine helper `BugsForCode(ctx, branch, files, symbols []string) ([]*Record, error)`:
  returns active `kind=bug` memories referencing any of the given file/symbol refs (via
  `memory_refs`), branch-visible, ranked by recency.
- **`PostRead`** (`hooks.go`): after the re-read check, normalize the read file to a
  repo-relative slash path, call `BugsForCode(files=[rel])`, and if any match, append
  *"⚠ prior bug in this file: `<title>` — fix: `<fix>`."* to the hook output.
- **`PostEdit`** (`graphhooks.go`): currently ignores its `io.Writer`. Change it to use the
  writer: after re-indexing, surface bugs for the edited file (same shape as PostRead).
  This is the highest-value moment — Claude is about to change buggy code.
- **`Augment`** (`graphhooks.go`): in addition to the code-graph symbol matches, run a
  `kind=bug` recall on the search pattern (`Recall{TaskHint: pattern, Kind: bug, limit:
  small}`) and include any matching bugs in the injected block.

### 3.4 Symbol-anchoring for bugs (prerequisite)

`PostRead` and `PostEdit` match bugs by **file** ref; the `Augment` path matches by
**symbol/pattern**. For the symbol path to work, bugs must carry symbol refs:

- `bug add` (`internal/cli/bug.go`): add `--symbols` (comma-separated) → `RefSymbol` refs,
  alongside the existing `--file` → `RefFile`.
- `record_bug` (`internal/mcp/handlers.go`): accept `symbols` and pass them through
  `refsFrom(...)` (the generic `record_memory` path already supports symbols).

### 3.5 Ref normalization (correctness)

Standardize file refs to **repo-relative, forward-slash** form on both ends:

- **On write** (`bug add` / `record_bug` / `record_memory`): convert any absolute file ref
  to repo-relative (`filepath.Rel(worktree, p)` + `ToSlash`) before storing in
  `memory_refs`. Relative inputs pass through unchanged.
- **On match** (hooks): convert the absolute path from the hook payload the same way before
  calling `BugsForCode` / structural recall.

Without this, a bug recorded as `internal/foo.go` never matches a read of
`/abs/repo/internal/foo.go`.

## 4. Components touched

- `internal/memory/recall.go` — `RecallQuery.Baseline`; baseline seeding in `Recall`; new
  `baselineIDs`, `BugsForCode`, `recentBugs` helpers.
- `internal/agents/claudecode/hooks.go` — tiered `SessionStart` + `formatSessionContext`
  (trim/degrade); budget from env; `PostRead` bug surfacing.
- `internal/agents/claudecode/graphhooks.go` — `PostEdit` uses its writer + bug surfacing;
  `Augment` includes matching bugs.
- `internal/cli/bug.go` — `--symbols` on `bug add`.
- `internal/mcp/handlers.go` — `record_bug` accepts `symbols`.
- Ref normalization helper shared by record paths + hooks (small util, e.g. in
  `internal/memory` or `internal/gitctx`).

## 5. Data flow (session start)

```
Claude session start
  → recarto hook session-start (stdin: {session_id, source})
    → ensure session row; warm graph; opportunistic Compact   (unchanged)
    → Recall{Branch, Baseline:true, TokenBudget:env|1500}
        candidates = baselineIDs ∪ pinnedIDs                    (no query)
        score (recency+reinforcement+branch) → selectWithinBudget
    → Architecture(worktree, "")                                (Tier 3 map)
    → recentBugs(branch, 5)                                     (Tier 4 line, N default 5)
    → formatSessionContext: pins → cerebrum(trim/degrade) → map+nudge → bugs
    → emit hookSpecificOutput.additionalContext
```

## 6. Error handling

All hooks remain **exit-0 advisory** (spec §13 parity): any error from baseline recall,
`Architecture`, `BugsForCode`, or `recentBugs` is non-blocking and must never prevent the
agent from proceeding. A failed tier is simply omitted from the injected block.

## 7. Testing (TDD — tests written first)

**Engine (`internal/memory`):**
- `TestBaselineRecallSurfacesCerebrumKinds` — preference/learning/decision returned; action excluded.
- `TestBaselineRecallExcludesActionAndBug` — neither appears in baseline output.
- `TestBaselineRecallRespectsBudgetAndPinsFirst` — budget caps; pinned do_not_repeat always included.
- `TestBaselineRecallBranchVisible` — branch-scoped + global visibility.
- `TestBugsForCodeMatchesByFileRef` / `...BySymbolRef` — structural bug lookup.
- `TestRecentBugsOrderedByRecency`.
- `TestFileRefNormalizedToRepoRelative` — abs path on write/match resolves to the same ref.

**Hooks (`internal/agents/claudecode`):**
- `TestSessionStartInjectsCerebrumArchAndRecentBugs` — additionalContext contains cerebrum titles, module names, recent-bug titles, and the nudge.
- `TestSessionStartDegradesToTitlesUnderTightBudget` — tiny budget → lower cerebrum entries titles-only; pins stay full.
- `TestSessionStartBudgetEnvOverride` — `RECARTO_SESSION_BUDGET` respected.
- `TestPostReadSurfacesBugForFile` — reading a file with a bug ref injects the warning.
- `TestPostEditSurfacesBugForEditedFile` — editing surfaces the bug via the writer.
- `TestAugmentSurfacesBugsMatchingPattern` — grep pattern surfaces matching bug.

**CLI / MCP:**
- `TestBugAddAcceptsSymbols` — `--symbols` creates `RefSymbol` refs.
- `TestRecordBugCapturesSymbols` — MCP `record_bug` stores symbol refs.

## 8. Measurement (validates the goal)

Watch the existing ledger counters to confirm net savings:
- `repeated_reads_blocked`, `recall_tokens_injected`, `recall_misses`, `memories_recalled`.
A win looks like: modest, steady `recall_tokens_injected` at session start (the bet) with
rising `repeated_reads_blocked` and bug-warnings landing on touched files (savings).

## 9. Out of scope (deliberate, measure-first)

- Per-file **prose descriptions + token-estimate catalog** (a literal `anatomy.md` clone).
  The `augment` hook already provides just-in-time "where is X". Build per-file descriptions
  only if measured numbers justify it.
- New scoring weights / kind-priority tuning beyond the ephemeral-kind exclusion.
- Embeddings-based recall (tracked separately via `recall_misses`).
