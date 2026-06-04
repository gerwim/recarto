# Recarto — Multi-Agent Support (OpenCode + GitHub Copilot CLI) — Design Spec

> Status: APPROVED-PENDING-REVIEW · 2026-06-04
> Phase 3 slice: "OpenCode + Copilot installers" (see cerebrum Decision Log 2026-06-03).
> Goal: Recarto supports **OpenCode** and **GitHub Copilot CLI** with feature parity
> to the existing Claude Code integration; a README with per-platform install
> instructions (Windows x64, Linux x64, macOS ARM64); a Makefile that builds all.

## 1. Background & Constraints

Recarto today integrates with **Claude Code** only, via `internal/agents/claudecode`:
- MCP server registration in `.mcp.json` (`mcpServers.recarto` → `recarto serve`).
- Lifecycle hooks in `.claude/settings.json` (`SessionStart`, `PostToolUse[Read]`,
  `PostToolUse[Edit|Write]`, `PostToolUse[Grep|Glob]`, `Stop`) → `recarto hook <event>`.
- A standalone `RECARTO.md` instructions file.

The hook handlers (`SessionStart`/`PostRead`/`PostEdit`/`Augment`/`Stop`) currently mix
three concerns: (a) parse Claude-Code-shaped JSON, (b) run agent-neutral logic
(recall, graph reconcile/reindex, ledger, session rows, compaction), (c) emit
Claude-Code-shaped JSON. Only (a) and (c) are host-specific.

The MCP server (`recarto serve`, `internal/mcp`) is **already protocol-standard** and is
reused verbatim by every host — no changes. Recarto is a CGO binary (tree-sitter,
Phase 2): cross-compilation requires `zig cc` + `CGO_ENABLED=1` (mirrors CI).

### Decisions locked with the user (2026-06-04)
1. **Architecture:** shared agent-neutral hook *core* + thin per-host *adapters*.
2. **`recarto init` default:** auto-detect installed agents (binary on `PATH`) and
   configure each detected one; `--agent <name>` forces a subset.
3. **Instructions:** unify on a single canonical content. At **repo scope** it is one
   shared `AGENTS.md` (read by all three hosts) — Claude's bespoke `RECARTO.md` is
   retired. At **user scope** the same content is written to each host's native global
   instructions file.
4. **Scope:** `recarto init` asks **user (global)** vs **repo** scope and applies the
   chosen scope to all three agents (`--scope user|repo` flag; interactive prompt when
   a TTY and no flag; default `repo` when non-interactive).

## 2. Host Capability Matrix (researched 2026-06-04)

| Concern | Claude Code | OpenCode | Copilot CLI |
|---|---|---|---|
| MCP (repo) | `.mcp.json` → `mcpServers.recarto` `{command,args:["serve"]}` | `opencode.json` → `mcp.recarto` `{type:"local",command:["<bin>","serve"],enabled:true}` | `.mcp.json` → `mcpServers.recarto` `{type:"local",command,args:["serve"]}` |
| MCP (user) | `~/.claude.json` → `mcpServers.recarto` | `~/.config/opencode/opencode.json` → `mcp.recarto` | `~/.copilot/mcp-config.json` → `mcpServers.recarto` |
| Hooks (repo) | `.claude/settings.json` `hooks` | `.opencode/plugins/recarto.js` (TS/JS plugin) | `.github/hooks/recarto.json` `{version:1,hooks:{…}}` |
| Hooks (user) | `~/.claude/settings.json` `hooks` | `~/.config/opencode/plugins/recarto.js` | `~/.copilot/hooks/recarto.json` (honors `$COPILOT_HOME`) |
| Instructions (repo) | `AGENTS.md` | `AGENTS.md` | `AGENTS.md` |
| Instructions (user) | `~/.claude/CLAUDE.md` | `~/.config/opencode/AGENTS.md` | `~/.copilot/copilot-instructions.md` |
| Session-start inject | `SessionStart` hook → `additionalContext` | plugin `experimental.chat.system.transform` → `output.system.push()` | `sessionStart` hook → `additionalContext` |
| Post-edit reindex | `PostToolUse[Edit\|Write]` | plugin `file.edited` event | `postToolUse` hook (edit tool) |
| Post-read / grep augment | `PostToolUse[Read]` / `[Grep\|Glob]` | plugin `tool.execute.after` (append to result) | `postToolUse` hook (read/grep tool) |
| Stop nudge | `Stop` → `systemMessage` | plugin `session.idle` | `agentStop` → `{decision,reason}` (informational) |

Parity holds: every host has MCP + an auto-loaded instructions file + lifecycle hooks
that can inject context. Differences are purely in config/payload **shapes**.

## 3. Architecture

### 3.1 Shared core: `internal/agents` (new package)

Move the agent-neutral logic out of `claudecode` into `package agents`:

```go
type EventKind int // SessionStart, PostRead, PostEdit, Augment, Stop

type Event struct {
    Kind       EventKind
    Agent      string // "claude-code" | "opencode" | "copilot" (→ sessions.agent)
    SessionID  string
    Source     string // startup|resume|new (best-effort)
    FilePath   string // absolute path for PostRead/PostEdit
    Pattern    string // grep/glob query for Augment
    ToolResult string // raw tool result text for PostRead token estimate
    StopActive bool
}

type Result struct {
    Context       string // text to inject as additionalContext (may be empty)
    SystemMessage string // Stop nudge (may be empty)
}

func Handle(ctx context.Context, a *app.App, ev Event) (Result, error)
```

`Handle` switches on `ev.Kind` and runs exactly the logic that lives in
`claudecode/hooks.go` + `graphhooks.go` today (session row upsert using `ev.Agent`,
graph reconcile, compaction, recall, repeated-read ledger, bug surfacing, post-edit
reindex+resolve, grep augment), returning a neutral `Result`. The formatting helpers
(`formatSessionContext`, `formatBugWarnings`, `trimOneLine`, `nullable`, budget
constants/env) move here too. **No behavior change** for the logic itself — this is a
pure extract-and-rename refactor proven by porting the existing `claudecode` hook tests
to drive `agents.Handle`.

### 3.2 Per-host adapters

Each adapter is a small package: an `Install(opts)` installer + a hook
`Decode(stdin) (Event, error)` / `Encode(out, Result)` pair.

**`internal/agents/claudecode`** (adapt existing):
- `Decode`: existing CC payloads (`session_id`, `tool_input.file_path`, `tool_result`,
  `pattern`/`query`, `stop_hook_active`).
- `Encode`: `{"hookSpecificOutput":{"hookEventName":<Event>,"additionalContext":…}}`;
  Stop → `{"systemMessage":…}`.

**`internal/agents/copilot`** (new):
- `Decode`: camelCase payloads (`sessionId`, `cwd`, `toolName`, `toolArgs`,
  `toolResult`, `source`). File path is extracted generically from `toolArgs`
  (`path`/`filePath`/`file_path`/`file`); grep pattern from `pattern`/`query`/`regex`.
  `toolName` routes a single `postToolUse` invocation to the right `EventKind`
  (edit-like→PostEdit, read-like→PostRead, search-like→Augment). Exact Copilot tool
  names are verified against the installed CLI during implementation (matchers are
  regex; routing is by substring so it is resilient).
- `Encode`: `sessionStart`/`postToolUse` → `{"additionalContext":…}`;
  `agentStop` → informational (no block) — emit `{}` (or `additionalContext`).

**`internal/agents/opencode`** (new):
- OpenCode hooks run inside a JS plugin we generate; the plugin shells to
  `recarto hook <event> --agent opencode`, passing a **neutral** JSON on stdin and
  reading a **neutral** JSON on stdout (we own both ends).
- `Decode`: neutral `{session_id, source, file_path, pattern, tool_result}`.
- `Encode`: neutral `{"additionalContext":…,"systemMessage":…}` — the plugin applies
  `additionalContext` via `output.system.push()` (session start) or by appending to the
  tool result (`tool.execute.after`).

### 3.3 CLI wiring

- `recarto hook <event> [--agent claude|opencode|copilot]` — `--agent` (default
  `claude`) selects the adapter for `Decode`/`Encode`; the event still maps to an
  `EventKind`. Hooks remain non-blocking/exit-0.
- `recarto init [--agent <list>|all] [--scope user|repo]` — resolves the agent set
  (auto-detect when `--agent` absent) and scope (prompt/flag/default), then calls each
  selected adapter's `Install`.

### 3.4 Installers

A shared `Scope` (`repo`/`user`) + resolved target paths. Each `Install`:
- **MCP:** idempotent JSON merge of `recarto` entry into the host's MCP doc
  (reuse/generalize the existing `readJSONObject`/`writeJSONObject`/`ensure*` helpers).
- **Hooks:**
  - Claude: idempotent merge into `settings.json` `hooks` (existing logic; commands
    gain `--agent claude` — already the default, so the bare command stays valid).
  - Copilot: write a recarto-owned `recarto.json` hook file wholesale (Copilot merges
    across hook files, so a dedicated file needs no JSON merge and is idempotent by
    construction). Hooks call `recarto hook <event> --agent copilot`.
  - OpenCode: write a recarto-owned `recarto.js` plugin wholesale; merge `mcp.recarto`
    into `opencode.json`.
- **Instructions:** write the canonical content to the scope-appropriate file. When the
  target file already exists, insert/replace a delimited block
  (`<!-- recarto:begin -->` … `<!-- recarto:end -->`) so re-runs are idempotent and the
  user's other content is preserved.

The generated `recarto.js` plugin (illustrative):

```js
// recarto OpenCode plugin (generated by `recarto init`). Bridges OpenCode
// lifecycle hooks to `recarto hook … --agent opencode`.
const BIN = "<abs path to recarto>";
export const recarto = async ({ $, directory }) => {
  const run = async (event, payload) =>
    (await $`${BIN} hook ${event} --agent opencode`.cwd(directory)
       .stdin(JSON.stringify(payload)).quiet().nothrow()).stdout.toString();
  let injected = false;
  return {
    "experimental.chat.system.transform": async (_in, out) => {
      if (injected) return; injected = true;
      const r = JSON.parse(await run("session-start", {}) || "{}");
      if (r.additionalContext) out.system.push(r.additionalContext);
    },
    "file.edited": async (input) => { await run("post-edit", { file_path: input.file }); },
    "tool.execute.after": async (input, out) => {
      const isGrep = /grep|glob|search/i.test(input.tool || "");
      if (!isGrep) return;
      const r = JSON.parse(await run("augment", { pattern: out?.args?.pattern || "" }) || "{}");
      if (r.additionalContext && out) out.output = (out.output || "") + "\n\n" + r.additionalContext;
    },
  };
};
```

(Exact OpenCode plugin hook input field names are verified against the installed
`opencode@1.15.12` during implementation; the plugin is the only JS we ship and is
covered by a generation test, not unit-executed in Go tests.)

## 4. Makefile

Mirrors `worktree-dev/Makefile`, extended to Recarto's three ship targets and CGO:

```make
BINARY_NAME := recarto
PKG         := ./cmd/recarto
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -ldflags "-s -w -X git.gerwim.nl/gerwim/recarto/internal/version.Version=$(VERSION)"

.PHONY: all linux darwin windows test clean
all: linux darwin windows
linux:   ; GOOS=linux  GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl"  go build $(LDFLAGS) -o releases/linux/$(BINARY_NAME)        $(PKG)
darwin:  ; GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC="zig cc -target aarch64-macos"      go build $(LDFLAGS) -o releases/darwin-arm64/$(BINARY_NAME) $(PKG)
windows: ; GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-windows-gnu" go build $(LDFLAGS) -o releases/windows/$(BINARY_NAME).exe   $(PKG)
test:    ; go test ./...
clean:   ; rm -rf releases
```

`internal/version.Version` must be a package `var` (not const) so `-X` can override it
(verified during implementation; adjust if currently const). `zig` is a build-time
dependency only, never shipped.

## 5. README

Replace the GitLab boilerplate with: one-paragraph "what is Recarto", a feature
summary, **Install** (per platform: download the prebuilt binary for Windows x64 /
Linux x64 / macOS ARM64 **or** `make` from source — requires Go 1.26.4 + zig for
cross-compile), **Setup** (`recarto init`, scope/agent prompt, what each agent gets),
**Usage** (MCP tools + CLI commands), and a short architecture note.

## 6. Testing (TDD, per project mandate)

RED→GREEN→REFACTOR for every unit. Concurrency unaffected (no new write paths).

- `internal/agents`: port existing claudecode hook tests to drive `agents.Handle`
  (behavior-preservation proof); table-test `EventKind` routing.
- `claudecode` adapter: `Decode` round-trips existing CC payloads → `Event`; `Encode`
  emits the exact existing JSON (golden, so output is byte-identical to today).
- `copilot` adapter: `Decode` parses camelCase payloads incl. `toolArgs` path/pattern
  extraction + `toolName` routing; `Encode` emits `additionalContext`. `Install`
  writes/merges `.mcp.json`, `.github/hooks/recarto.json`, `AGENTS.md` (repo) and the
  user-scope equivalents; idempotent re-run.
- `opencode` adapter: neutral `Decode`/`Encode`; `Install` merges `opencode.json`
  `mcp.recarto`, writes `.opencode/plugins/recarto.js`, `AGENTS.md`; idempotent;
  generated plugin contains the expected hook keys + absolute binary path.
- `cli`: `init` auto-detect (fake PATH), `--agent`/`--scope` parsing, scope prompt;
  `hook --agent` selects the adapter.
- Instructions block insert/replace idempotency (delimited markers).
- Manual verification: `make all` produces three binaries; `recarto init` against the
  installed `opencode`/`copilot` registers servers (`opencode mcp list` /
  `copilot mcp list`) and a smoke hook invocation returns valid JSON.

## 7. Out of Scope / YAGNI

- No changes to `internal/mcp` (the MCP server is reused as-is).
- No `preToolUse` allow/deny policy hooks (advisory parity only, matching Claude today).
- No Gemini CLI / Cursor / other hosts (only the two requested).
- No release/publishing pipeline beyond the Makefile (no GitHub Releases upload, no
  Homebrew tap) — install doc covers download + build-from-source.
- No `userPromptSubmitted` per-turn injection (Copilot ignores its output; session
  start covers injection).

## 8. Risks

- **Copilot tool names / `toolArgs` shape** — resolved by substring routing + generic
  path extraction; verified against installed CLI.
- **OpenCode `experimental.chat.system.transform`** has had upstream bugs discarding
  mutations in some versions; if it no-ops on `opencode@1.15.12`, session-start
  injection degrades to AGENTS.md instructing the agent to call `recall_memory` first
  (MCP parity is unaffected). Verified during implementation.
- **`.mcp.json` shared by Claude Code + Copilot** — both key on `mcpServers`; Copilot
  tolerates a `command`+`args` entry (infers local stdio). The repo-scope entry is
  written in the lowest-common-denominator shape that both accept; verified with
  `copilot mcp list` + Claude Code.
