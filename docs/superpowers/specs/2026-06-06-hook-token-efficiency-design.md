# Hook token-efficiency: glob-noise + generated-edge demotion

Date: 2026-06-06
Branch: feature/e2e-llm-test

## Problem (evidence)

Live hook runs on `~/Projects/graphql-platform` surfaced two token-efficiency
defects in the context the hooks inject into the agent:

1. **Glob patterns inject misleading symbol noise.** The augment hook
   (`handleAugment`) feeds the agent's Grep/Glob pattern straight into
   `graph.Search` as a symbol query. For a *file glob* like `*.cs`, the search
   term degrades to the literal `cs`, returning minified-JS functions
   (`cs`, `Cs.u`) from `dist/index.js` and vendored yarn releases. This fires on
   **every glob**, wasting tokens and actively misleading the agent with
   irrelevant symbols.

2. **The session-start code map is dominated by generated-code coupling.** The
   heaviest dependency edges are `ChilliCream.Nitro.Client → Types.Abstractions
   (7435)` etc. — references originating in a single generated GraphQL client
   file (`.../Generated/ApiClient.Client.cs`, ~23k symbols). These codegen edges
   crowd out genuine product→product coupling and, via the edge-endpoint
   backfill, drag generated client modules into the module list, diluting
   orientation signal.

## Goals / non-goals

- Improve the signal-to-token ratio of hook-injected context. No regressions:
  ordinary Grep identifier searches and coherent maps must behave identically.
- TDD: each change lands with a failing test first.
- Non-goal: demoting legitimate-but-large modules (docs sites). Stay conservative
  — false demotion of hand-written code is a quality regression.

## Part 1 — Glob-aware augment search term

New pure helper (`internal/agents/augment.go`):

```
augmentSearchTerm(pattern string) (term string, ok bool)
```

- **Not a file glob** → `(pattern, true)`. Grep identifier/regex patterns
  (`AddProjection`, `Add.*Projection`, `foo*`) pass through byte-identical. No
  regression.
- **File glob** → reduce to the identifier stem of the basename minus extension
  minus wildcards. `**/*Projection*.cs` → `("Projection", true)`.
- **Pure-extension / stemless glob** (`*.cs`, `*.json`, `src/**/*.go`,
  `internal/**`) → `("", false)`.

A pattern "looks like a file glob" when it contains `**`, OR contains both `/`
and a wildcard (`*`/`?`), OR is a wildcard basename ending in a `.<ext>`
(1–5 alnum) extension. This is deliberately narrow so regex Grep patterns are
never misclassified.

`handleAugment` uses the helper: when `ok`, run symbol search and bug recall
using `term` (not the raw glob); when `!ok`, skip the symbol/bug injection
entirely (inject nothing rather than noise). The session dedup metric stays
keyed on the original `ev.Pattern`.

## Part 2 — Generated-edge demotion in the map

- Add `GeneratedFrom int` to `graph.ModuleEdge`: count of references on the edge
  whose **source file** path is generated (`isGeneratedPath`). Populated in
  `Engine.Architecture` from the existing `srcf.path` in the deps query.
- In `MapArchitecture`, demote edges that are *predominantly* generated-sourced
  (majority of `Count`), ranking them below genuine edges — the same shape as the
  existing test-edge demotion. Test-edge demotion takes precedence (outer key).
- Consequence: generated client edges fall out of the top-N deps, so the
  endpoint backfill no longer pulls generated client modules into the list. The
  map leads with product modules and product→product coupling.
- Reuses the validated, conservative `isGeneratedPath`; coherent maps with no
  generated edges are unaffected (no-op).

## Instructions/docs

CLAUDE.md hook guidance does not describe glob-vs-symbol behavior, so no
user-facing instruction change is required. If wording in the augment area needs
a touch it will be made alongside the code.

## Testing

- `augment_test.go`: table test for `augmentSearchTerm` covering glob shapes,
  stems, and pass-through identifiers/regexes.
- `handle_test.go`: augment with a `*.cs` glob injects nothing; augment with a
  clean identifier still injects symbols (no regression).
- `architecture_test.go`: `MapArchitecture` ranks a genuine product edge above a
  heavier generated-sourced edge; a map with no generated edges is unchanged.
- Full `go test ./...` green before commit.
```
