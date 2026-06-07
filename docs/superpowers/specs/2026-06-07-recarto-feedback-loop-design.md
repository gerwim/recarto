# Recarto self-improvement feedback loop — design

**Date:** 2026-06-07
**Branch:** feature/dogfooding
**Status:** Approved, ready for planning

## Problem

Recarto is used across many repos (Ordia, HotChocolate, and every other project).
While an LLM works in one of those repos it regularly has direct, first-hand
evidence that Recarto itself is misbehaving — a tool returns an error, ranks the
wrong symbol first, exposes a confusing parameter, or lacks a capability the task
needed. That signal is the single best source of Recarto bugs, and today it is
lost: there is no channel for "a bug about Recarto" as distinct from
`record_bug`, which records bugs about the *user's* code into the *per-repo*
store.

We want a self-improvement loop: let an LLM report Recarto's own problems from
whatever repo it is in, collect those reports in one place, and let the developer
triage and act on them from anywhere.

## Goals

- Capture all four signal types: hard **errors/crashes**, **quality/ranking**
  misses, **API friction/confusion**, and **capability gaps**.
- Let agents report **opportunistically in the moment**, with a **session-end
  reflection** backstop for what they forgot to flag.
- Collect every report in **one global queue**, independent of which repo it came
  from, and never pollute per-repo project memory.
- Let the developer triage with a single CLI command that works **from anywhere on
  the system**, including outside any git repo.

## Non-goals

- No automatic interception of tool errors (rejected: misses the judgment-based
  signals, which are the valuable ones).
- No scheduled/periodic dogfooding agent.
- No session-start auto-injection of open reports, and therefore **no self-repo
  detection** — review is purely manual via the CLI.
- No GitHub-issue filing. (Possible future: an export step that turns triaged
  reports into issues on demand. Out of scope here.)

## Key architecture decision: reuse the memory engine

A report is modeled as a `memory.Record` of a new kind, `KindFeedback`, stored in
a dedicated global store opened at a **reserved repo-id**. We reuse
`memory.Engine` + `store.Store` rather than building a purpose-built feedback
table.

Rationale:
- **Near-duplicate reinforcement for free.** The same bug reported from five repos
  converges to one entry with an occurrence count and the set of source repos,
  using the engine's existing reinforcement path.
- Reuses existing schema, migrations, lexical search, ranking, and recall.
- Minimal new persistence surface.

The alternative (a bespoke feedback table) would reimplement dedup, ranking, and
storage that already exist. Rejected.

## Components

### 1. Storage — the global feedback store

- A reserved repo-id constant, `_recarto_feedback`, that **cannot collide** with a
  real git-derived repo-id (real ids are git-hash derived; the leading-underscore
  sentinel is reserved). Define it as a single exported constant so the tool, the
  hook, and the CLI all agree.
- Store path: `cache.StorePath("_recarto_feedback")` →
  `<userCacheDir>/recarto/_recarto_feedback/store.db`.
- `cache.Dir()` already resolves purely from `os.UserCacheDir()` /
  `RECARTO_CACHE_DIR` with **no dependence on cwd or git** — this is what makes the
  store path identical from every directory.
- Opened **lazily on first report** (reuse `store.Open` + schema migration). One
  `memory.Engine` bound to it.
- A thin opener helper (e.g. `feedback.OpenStore` / `feedback.Engine`) that both
  the MCP handler and the CLI call, so neither duplicates the reserved-id wiring.

### 2. `report_recarto_issue` MCP tool

The agent calls this the moment it hits or notices a Recarto problem.

Input schema:

| Field | Req | Notes |
|-------|-----|-------|
| `signal_type` | yes | enum: `error` \| `quality` \| `friction` \| `gap` |
| `title` | yes | one-line summary |
| `detail` | yes | what the agent was doing; expected vs. actual |
| `tool` | no | which Recarto tool/area was involved |
| `severity` | no | `low` \| `medium` \| `high` |
| `repro` | no | the call/args that triggered it |
| `suggested_fix` | no | the agent's proposed fix, if any |

Auto-captured (the agent does **not** supply these; the handler fills them):
- source repo-id and name (so the developer knows where it came from — the id, not
  the source),
- Recarto version,
- timestamp.

(Agent host was considered but dropped: the host identity isn't available at
MCP-tool time, only at hook time. Not worth a dead field; see the plan.)

Behavior:
- Writes the record to the **feedback store** (reserved id), **never** to the
  current repo's store. This is a hard isolation requirement with a test.
- Maps to a `memory.NewMemory{Kind: KindFeedback, ...}` with `signal_type`,
  `severity`, `tool`, `repro`, `suggested_fix`, and the auto-captured context in
  `Extra`; `title`/`detail` form the title/body so lexical search and
  reinforcement work.
- Returns `{id, reinforced}` — `reinforced: true` when it merged into an existing
  report.

Tool description guidance: describe the **Recarto interaction and a minimal
repro**, not pasted proprietary source from the user's other repos. Keeps the
global channel privacy-clean.

### 3. Session-end reflection — Stop hook

- A `Stop` hook, installed via `recarto init` (written into settings.json by the
  existing per-host installer), emits a terse prompt at session end:
  > "Did Recarto error, mis-rank, confuse, or lack something this session? If so,
  > call `report_recarto_issue` before finishing."
- **Gated:** fires only if a Recarto MCP tool was actually used this session,
  reusing the existing per-session marker machinery the hooks already maintain.
  Silent in non-Recarto sessions.
- This is the backstop for judgment-based signals (quality/friction/gap) that
  agents forget to flag in the moment.

### 4. `recarto feedback` CLI — triage

Mirrors the existing `recarto todo` / `recarto bug` command shape.

- `recarto feedback` / `recarto feedback list` — open reports, ranked by
  reinforcement/severity: id, signal_type, severity, title, occurrence count,
  source repos. `--all` includes resolved.
- `recarto feedback show <id>` — full detail incl. auto-captured context.
- `recarto feedback resolve <id> [--note <text>]` — mark resolved; drops from the
  default list.

**Location independence (core requirement):** the `feedback` command must **skip
the normal git-context / repo-id resolution** that every other command performs.
It opens the reserved `_recarto_feedback` store directly. This is what lets it run
from `~`, from inside any repo, or outside a git repo entirely, always against the
one global queue.

## Data flow

```
agent in repo X
  ├─ hits/notices a Recarto problem  ──┐
  └─ Stop hook reflection nudge       ─┤
                                       ▼
                       report_recarto_issue (MCP)
                                       │  auto-captures repo-id/name, version, ts
                                       ▼
                 feedback store  <cache>/recarto/_recarto_feedback/store.db
                                       │  engine dedups + reinforces (occurrence count, source repos)
                                       ▼
  developer, from anywhere ── recarto feedback [list|show|resolve]
```

## Testing (TDD — test-first)

- **Store isolation:** a report writes to the feedback store and **not** to the
  current repo's store.
- **Location independence:** the feedback CLI resolves and opens the store
  **without touching git context** (works with cwd outside any repo).
- **Reserved-id safety:** the sentinel id cannot equal a real git-derived id.
- **Reinforcement:** two similar reports from different source repos converge to
  one record with a bumped occurrence count and a merged source-repo set.
- **Handler:** required-field validation; `Extra` carries the auto-captured
  context; returns `{id, reinforced}`.
- **CLI:** `list` (open vs `--all`), `show`, `resolve` (with `--note`) state
  transitions.
- **Stop-hook gating:** reflection prompt emitted only when a Recarto tool was
  used this session; silent otherwise.

## Affected/added surface (indicative, finalized in the plan)

- `internal/feedback/` — reserved-id constant + store/engine opener helper.
- `internal/mcp/` — `report_recarto_issue` handler + input schema.
- `internal/cli/feedback.go` — `list` / `show` / `resolve`.
- `internal/memory/` — `KindFeedback` kind.
- Hooks + `recarto init` installer — the gated Stop reflection prompt.
- Agent-instruction snippet (agents core) — encourage `report_recarto_issue`.

## Future (out of scope)

- `recarto feedback file-issue <id>` to export a triaged report to a GitHub issue.
- Optional session-start surfacing inside the Recarto dev repo (would reintroduce
  self-repo detection — deliberately deferred).
