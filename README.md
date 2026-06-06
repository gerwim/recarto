# Recarto

**Cross-session memory and a code graph for AI coding agents — one static binary.**

Recarto gives your coding agent a persistent brain: it remembers preferences,
learnings, decisions, do-not-repeats and bugs across sessions, and it indexes your
codebase into a tree-sitter symbol/call graph. Both are exposed as **MCP tools** and
wired into each agent's **lifecycle hooks**, so the right context shows up
automatically at session start and while you work.

Everything lives in a shared per-repo SQLite cache **outside** your working tree
(`~/.cache/recarto/<repo-id>/`), keyed by the repository and tagged per branch and
worktree. Nothing is committed, so there are no file-merge conflicts.

Supported agents: **Claude Code**, **OpenCode**, and **GitHub Copilot CLI** — with
feature parity across all three.

---

## What it does

- **Memory** — `record_memory` / `recall_memory`: token-budgeted, branch-aware recall
  ranked by lexical (BM25) + structural signals; near-duplicates are reinforced, not
  duplicated.
- **Bugs** — `record_bug` / `search_bugs`: searchable bug history (error, root cause,
  fix), surfaced just-in-time when you read or edit the related file.
- **Code graph** — `search_graph` / `get_code_snippet` / `trace_call_path` /
  `find_implementations` / `find_annotated` / `search_content` / `get_architecture` /
  `query_graph`: query symbols, call paths, interface implementations, attribute/
  decorator usages and module structure instead of reading whole files. Re-indexed
  incrementally as the agent edits.
  Searching by concept? Pass `search_graph` the identifier names you expect via
  `candidate_symbols` — the agent supplies the semantic signal, the dependency-free
  lexical engine does the lookup (no embedding model required). See
  [the design](docs/superpowers/specs/2026-06-04-query-guided-search-design.md).
- **Session injection** — at session start Recarto injects the most relevant memories
  for the current branch, a compact module map, and recent bugs.

## Install

Recarto is a single self-contained binary (no runtime dependencies).

### Download a prebuilt binary

Grab the binary for your platform from the
[releases page](https://git.gerwim.nl/gerwim/recarto/-/releases), then put it on your
`PATH`:

| Platform | File |
|---|---|
| **Linux x64** | `recarto` (linux-amd64, static musl) |
| **Windows x64** | `recarto.exe` (windows-amd64) |
| **macOS (Apple Silicon)** | `recarto` (darwin-arm64) |

**Linux / macOS**

```sh
chmod +x recarto
sudo mv recarto /usr/local/bin/recarto      # or anywhere on your PATH
recarto version
```

**Windows (PowerShell)**

```powershell
# Move recarto.exe to a folder on your PATH, e.g. %USERPROFILE%\bin
mkdir $HOME\bin -Force
Move-Item .\recarto.exe $HOME\bin\
setx PATH "$($env:PATH);$HOME\bin"          # new terminals pick this up
recarto version
```

### Build from source

Requires **Go 1.26.4**. Recarto links tree-sitter via CGO, so a C toolchain is
needed.

```sh
git clone https://git.gerwim.nl/gerwim/recarto.git
cd recarto

# Host build (uses your system C compiler — best on macOS for the darwin binary):
make native            # → releases/recarto

# Cross-compile the release targets (requires `zig`):
make linux             # → releases/linux-amd64/recarto   (static musl)
make windows           # → releases/windows-amd64/recarto.exe
make darwin            # → releases/darwin-arm64/recarto   (see note below)
make all               # linux + windows + darwin
```

`zig` is used only as a cross-compiler (`zig cc`) and is never shipped. Install it
from <https://ziglang.org/download/> (or `brew install zig`).

**linux** and **windows** cross-build from any host with zig. The **darwin** target
additionally needs the macOS SDK frameworks (`CoreFoundation`, `Security`, linked by
Go's `crypto/x509`), which zig does not bundle — so build it on a macOS host
(`make native`) or with an SDK available (e.g. [osxcross](https://github.com/tpoechtrager/osxcross)).

## Setup

From inside a git repository, run:

```sh
recarto init
```

`recarto init` **auto-detects** which agents are installed (`claude`, `opencode`,
`copilot` on your `PATH`) and configures each one. When run interactively it asks
whether to install at **repo** scope (this project) or **user** scope (global).

Flags:

```
recarto init [--agent claude,opencode,copilot] [--scope user|repo]
```

What each agent gets:

| Agent | MCP server | Hooks | Instructions |
|---|---|---|---|
| **Claude Code** | `.mcp.json` (repo) / `~/.claude.json` (user) | `.claude/settings.json` / `~/.claude/settings.json` | `AGENTS.md` / `~/.claude/CLAUDE.md` |
| **OpenCode** | `opencode.json` / `~/.config/opencode/opencode.json` | `.opencode/plugins/recarto.js` (generated) | `AGENTS.md` / `~/.config/opencode/AGENTS.md` |
| **Copilot CLI** | `.mcp.json` / `~/.copilot/mcp-config.json` | `.github/hooks/recarto.json` / `~/.copilot/hooks/recarto.json` | `AGENTS.md` / `~/.copilot/copilot-instructions.md` |

`recarto init` is idempotent — re-running merges only what's missing and preserves
your existing config.

After `init`, **restart your agent** (Claude Code, OpenCode or Copilot CLI) so it
picks up the new MCP server and hooks. Verify the MCP server is connected with your
agent's own command — e.g. `opencode mcp list` or `copilot mcp list` should show
`recarto`.

## Usage

Day to day you don't call Recarto directly; your agent uses the MCP tools and the
hooks fire automatically. The CLI is available for inspection and scripting:

```
recarto status            repo id, branch, memory counts, ledger
recarto doctor            health checks (db, WAL, integrity, git)
recarto remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)
recarto forget <id>       hard-delete a memory by id
recarto recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)
recarto bug add|search    record or search bug memories
recarto memory show       render all active memory as markdown (--branch)
recarto index             build/refresh the code graph for this worktree
recarto graph <sub>       query the code graph (search|grep|callers|callees|impls|annotated|outline|arch|query|snippet)
                          graph search [--symbol=Name ...] <concept words>  (candidate-guided)
                          graph impls <Type> [--super]  (implementations/subtypes; --super for supertypes)
                          graph annotated <Attribute>  (symbols decorated with [Attribute] / @decorator)
                          graph outline <file>  (a file's symbols/signatures/lines, instead of reading it)
recarto compact           prune/decay memory (--force --dry-run --budget-tokens)
recarto serve             run the MCP server over stdio (used by the agents)
```

MCP tools exposed by `recarto serve`: `record_memory`, `recall_memory`, `get_memory`,
`list_memory`, `record_bug`, `search_bugs`, `search_graph`, `search_content`,
`get_code_snippet`, `get_file_outline`, `trace_call_path`, `find_implementations`,
`find_annotated`, `get_architecture`, `query_graph`.

## Verify it's working

After `recarto init` and restarting your agent, sanity-check the install.

**Linux / macOS**

```sh
recarto version
recarto doctor            # health checks — every line should read [ok]
recarto index             # build the code graph for this worktree
```

**Windows (PowerShell)**

```powershell
recarto version
recarto doctor            # health checks — every line should read [ok]
recarto index             # build the code graph for this worktree
```

`recarto doctor` checks the store (WAL, integrity, schema) and git identity, and exits
non-zero if any check fails — so it doubles as a smoke test in scripts and CI.

**Is it doing useful work?** Run `recarto status` after a few agent sessions and watch
the lifetime ledger counters:

- `memories_recalled` climbing while `recall_misses` stays low → recall is hitting.
- `recall_tokens_injected` → how much context is injected at session start.
- `reads` / `repeated_reads_blocked` → the just-in-time bug/read surfacing is firing.
- `live tokens N / 250000` → how full the live-set budget is (compaction prunes as it
  approaches the cap).

**Is it fast?** Recarto has no built-in timing, so measure the two heavy operations —
the graph index and recall — directly. Hooks are exit-0 CLI calls, so their latency is
essentially these two (session-start / post-edit reconcile + session-start injection):

**Linux / macOS**

```sh
time recarto index
time recarto recall "auth flow"
```

**Windows (PowerShell)**

```powershell
Measure-Command { recarto index }
Measure-Command { recarto recall "auth flow" }
```

## How it works

- **One store per repo** in a global cache (`~/.cache/recarto/<repo-id>/`), shared
  across all worktrees; the repo id comes from `git rev-parse --git-common-dir`.
  Records are branch/worktree-tagged.
- **SQLite is the single source of truth** (pure-Go `modernc.org/sqlite`, WAL);
  markdown is on-demand export only.
- **No daemon.** Hooks are pure exit-0 CLI invocations; the code graph reconciles
  lazily at session start and incrementally on each edit.
- **Languages:** Go, C#, TypeScript (+TSX), JavaScript (+JSX) via tree-sitter.

## Development

```sh
make test     # go test ./...
make vet      # go vet ./...
make fmt      # gofmt -l .
```

CI runs the suite on Linux, macOS and Windows, gofmt-checks every file, and
cross-compiles all three release targets with `zig cc`.
