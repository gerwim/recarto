# Recarto

**Cross-session memory and a code graph for AI coding agents — one static binary.**

**AI-generated.** Recarto's code is written by AI coding agents, orchestrated with [Kanship](https://kanship.com).

Recarto gives your coding agent a persistent brain: it remembers preferences,
learnings, decisions, do-not-repeats and bugs across sessions, and it indexes your
codebase into a tree-sitter symbol/call graph. Both are exposed as **MCP tools** and
wired into each agent's **lifecycle hooks**, so the right context shows up
automatically at session start and while you work.

Everything lives in a shared per-repo SQLite cache **outside** your working tree
(`~/.cache/recarto/<repo-id>/`), keyed by the repository and tagged per branch and
worktree. Nothing is committed, so there are no file-merge conflicts.

Supported agents: **Claude Code**, **OpenCode** (1.x and 2.x), and **GitHub Copilot
CLI**. Each host exposes different tools and hooks, so what Recarto can see differs
per host; see [Shell searches and file views](#shell-searches-and-file-views).

---

## What it does

- **Memory** — `record_memory` / `recall_memory`: token-budgeted, branch-aware recall
  ranked by hybrid BM25, semantic embeddings and structural refs behind a relevance
  floor; near-duplicates are reinforced, not duplicated.
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
- **HTTP memory sync** (optional) — converge memories across machines working the same
  repo, while staying fully functional offline. See
  [Sharing memory across machines](#sharing-memory-across-machines-http-sync).

Memory recall embeds with the in-binary Model2Vec `potion-base-8M` model (MIT; its
WordPiece vocabulary derives from BERT/BGE) and adds about 8 MB to the binary. Set
`RECARTO_SEMANTIC=0` to use lexical-only ranking or `RECARTO_RECALL_FLOOR=0` to disable
the relevance floor.

## Install

Recarto is a single self-contained binary (no runtime dependencies).

### Download a prebuilt binary

Prebuilt binaries are published on <https://releases.gerwim.com/recarto/>:

| Platform | File |
|---|---|
| **Linux x64** | `recarto-linux-amd64` (static musl) |
| **Windows x64** | `recarto-windows-amd64.exe` |

On any other platform, [build from source](#build-from-source).

[`index.json`](https://releases.gerwim.com/recarto/index.json) lists every version
(`latest` is the newest) with each file's sha256.

**Linux**

```sh
curl -fLo recarto https://releases.gerwim.com/recarto/<version>/recarto-linux-amd64
echo "<sha256 from index.json>  recarto" | sha256sum -c -
chmod +x recarto
sudo mv recarto /usr/local/bin/recarto      # or anywhere on your PATH
recarto version
```

**Windows (PowerShell)**

```powershell
Invoke-WebRequest https://releases.gerwim.com/recarto/<version>/recarto-windows-amd64.exe -OutFile recarto.exe
(Get-FileHash .\recarto.exe -Algorithm SHA256).Hash   # must equal the sha256 in index.json
# Move recarto.exe to a folder on your PATH, e.g. %USERPROFILE%\bin
mkdir $HOME\bin -Force
Move-Item .\recarto.exe $HOME\bin\
setx PATH "$($env:PATH);$HOME\bin"          # new terminals pick this up
recarto version
```

### Build from source

Requires **Go 1.26.4 or later** and a C compiler. Recarto links tree-sitter via
CGO, so a C toolchain is needed.

```sh
git clone https://github.com/gerwim/recarto.git
cd recarto

# Host build (uses your system C compiler — best on macOS for the darwin binary):
make native            # → releases/recarto

# Cross-compile the release targets (requires `zig`):
make linux             # → releases/linux-amd64/recarto   (static musl)
make windows           # → releases/windows-amd64/recarto.exe
make darwin            # → releases/darwin-arm64/recarto   (see note below)
make all               # linux + windows + darwin
```

Or install straight to your `GOPATH/bin`; this also needs a C compiler:

```sh
go install github.com/gerwim/recarto/cmd/recarto@latest
```

Release tags have no `v` prefix, so Go resolves `@latest` to the newest commit on
`master`, and that binary reports a development version (`0.1.0-dev`). Run
`recarto update` to replace it with the latest signed release.

`zig` is used only as a cross-compiler (`zig cc`) and is never shipped. Install it
from <https://ziglang.org/download/> (or `brew install zig`).

**linux** and **windows** cross-build from any host with zig. The **darwin** target
additionally needs the macOS SDK frameworks (`CoreFoundation`, `Security`, linked by
Go's `crypto/x509`), which zig does not bundle — so build it on a macOS host
(`make native`) or with an SDK available (e.g. [osxcross](https://github.com/tpoechtrager/osxcross)).

## Updating

`recarto update` upgrades (or downgrades) the binary in place from
`https://releases.gerwim.com/recarto/`. Releases are signed with
[minisign](https://jedisct1.github.io/minisign/); the public key is embedded in the
binary, so a download is applied only if **both** its signature and its sha256 verify
— a compromised mirror cannot ship a malicious update.

```sh
recarto update                 # pick the latest (default) or, interactively, an older version
recarto update --version 1.3.0 # install a specific version (downgrade / pin)
recarto update --list          # list available versions
recarto update --check         # report whether an update exists; exit 0 = current, 10 = available
recarto update --yes           # skip prompts (always installs the latest)
```

`update` makes no changes under `--check`/`--list`. If the binary lives on a
read-only path (e.g. `/usr/local/bin`), re-run with the privileges needed to replace
it. Development builds (non-release versions) are always offered the latest release.

### Releasing

Tag a commit (`git tag X.Y.Z && git push origin X.Y.Z`). GitLab CI cross-compiles the
Linux and Windows binaries, computes sha256s, merges `index.json`, signs it with
minisign, and uploads the binaries + manifest + signature to the Cloudflare R2 bucket
behind `releases.gerwim.com`. The minisign secret key and R2 credentials live only in
masked CI variables; the minisign **public** key is committed
(`internal/update/pubkey.go`) and embedded in the binary.

The one-time setup — extracting the macOS SDK, building/pushing the builder image,
the Cloudflare R2 bucket, and the exact GitLab CI/CD variables — is documented in
[`ci/README.md`](ci/README.md).

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
| **OpenCode** | `opencode.json` / `~/.config/opencode/opencode.json` | `.opencode/plugins/recarto.js` / `~/.config/opencode/plugins/recarto.js` (generated) | `AGENTS.md` / `~/.config/opencode/AGENTS.md` |
| **Copilot CLI** | `.mcp.json` / `~/.copilot/mcp-config.json` | `.github/hooks/recarto.json` / `~/.copilot/hooks/recarto.json` | `AGENTS.md` / `~/.copilot/copilot-instructions.md` |

The instructions are a short block of rules between `<!-- recarto:begin -->` and `<!-- recarto:end -->`: what to record and recall, and which Recarto tool to use instead of reading whole files or grepping. How to use each tool is in the tool's own description. Re-running `recarto init` replaces the block, also one written by an older version, and leaves the rest of the file as it is.

`recarto init` is idempotent — re-running merges only what's missing and preserves
your existing config.

The generated OpenCode plugin works with both OpenCode 1.x and OpenCode 2.x (its
default export carries the 2.x `setup` entrypoint and the 1.x `server` one), so it
keeps working across an OpenCode upgrade. OpenCode 2 does not load plugins written by
older Recarto releases: re-run `recarto init` once to regenerate it. The MCP entry is
written in the 1.x shape both read, unless your `opencode.json` already uses 2.x's
native `mcp.servers` map — then it goes there. Auto-detection also finds OpenCode 2's
`opencode2` executable.

After `init`, **restart your agent** (Claude Code, OpenCode or Copilot CLI) so it
picks up the new MCP server and hooks. Verify the MCP server is connected with your
agent's own command — e.g. `opencode mcp list` or `copilot mcp list` should show
`recarto`.

## Usage

Day to day you don't call Recarto directly; your agent uses the MCP tools and the
hooks fire automatically. The CLI is available for inspection and scripting:

```
recarto config            turn features on or off (interactive; or list|enable|disable|path)
recarto licenses          print Recarto's licence and the third-party notices
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
recarto repair            rebuild a corrupt store, salvaging memories (--path <store.db>)
recarto serve             run the MCP server over stdio (used by the agents)
```

MCP tools exposed by `recarto serve`: `record_memory`, `recall_memory`, `get_memory`,
`list_memory`, `record_bug`, `search_bugs`, `search_graph`, `search_content`,
`get_code_snippet`, `get_file_outline`, `trace_call_path`, `find_implementations`,
`find_annotated`, `get_architecture`, `query_graph`.

## Turning features on or off

Every feature is on by default. Run `recarto config` in a terminal to switch them:
↑/↓ to move, space to toggle, `a` to turn everything on, `q` to quit. Each change is
saved at once and applies to new agent sessions.

The agent instructions `recarto init` writes name only the MCP tools that are on.
After you switch `memory-tools`, `todos`, `graph-tools` or `issue-reports`,
`recarto config` asks you to re-run `recarto init` (same `--agent` and `--scope`)
to update them.

```sh
recarto config                            # interactive; prints the list outside a terminal
recarto config list                       # every feature, its state and what it does
recarto config disable todos stop-nudge   # or enable
recarto config path                       # where the file lives
```

The features cover the session-start context, the hints while working (bug warnings,
todos, search and shell hints, …), the end-of-turn reminder and nudge, the MCP tool
groups, and background work such as sync and compaction. A new feature shows up in
`recarto config` on its own, switched on.

The file is `~/.config/recarto/config.toml` (`%AppData%\recarto\config.toml` on
Windows, `~/Library/Application Support/recarto/config.toml` on macOS);
`RECARTO_CONFIG` points elsewhere. It holds `feature = true|false` lines and `#`
comments, so you can edit it by hand: a feature left out is on, and saving from
`recarto config` keeps your comments and layout.

```toml
# Session start
session-memories = true  # Inject the most relevant memories for this branch
code-map = false         # Add a map of the codebase's modules
```

A misspelled feature or a value other than `true`/`false` is reported with its line
number by `recarto config` and `recarto doctor`; hooks skip the bad line.
`RECARTO_SEMANTIC=0` and `RECARTO_AUTO_REPAIR=0` still turn those features off too.

## Read notices

After a file read (Claude Code's `Read`, OpenCode's `read`, Copilot's `view`), Recarto adds the file's anchored notes and at most two notices:

- **Repeat read** (`repeated-reads`). Added when every line the read returned came back in earlier reads of the same file in this conversation, and the file has not changed since. It names the lines when the read returned only part of the file. Once a file changes, reads of its old content no longer count. A read whose lines Recarto can't tell gets no notice. That includes Claude Code's own "file unchanged" reply when the agent repeats its last read.
- **Large file** (`large-reads`). Added once per conversation, on the first whole-file read of a file with indexed symbols that returns at least 2,000 estimated tokens (`RECARTO_LARGE_READ_TOKENS`). It points to `get_file_outline` and `get_code_snippet`. A read is whole-file when it asked for no line range, even if the host cut the output short, or when the lines it returned cover the whole file. A read of part of a file never gets it.

On a read, an edit or a shell view, a bug note shows the title, the first 200 characters of the fix and, with `memory-tools` on, the `get_memory` call that returns the whole text. Titles longer than 160 characters are cut in these notes, in the session-start memory list and in its list of recent bugs.

`recarto status` counts the notices added under `repeated_reads_blocked` and `large_reads_nudged`. On OpenCode, re-run `recarto init` after upgrading so the plugin passes the read's line range.

## Conversations, compaction and resume

Recarto records what each conversation has been given: the lines it read, the notes it was shown (bugs, do-not-repeats, decisions, learnings, todos), the searches that got a hint, and the one-time large-file and shell-search nudges. The repeat-read notice and the once-only notes and hints follow this record.

- **Subagents.** On Claude Code the main agent and each subagent have their own record, told apart by the hook input's `agent_id`. An OpenCode subagent runs in its own session. Copilot CLI hooks don't name subagents, so a session there has one record.
- **Compaction.** When Claude Code compacts a conversation, Recarto forgets its reads, notes and searches: a file read before the compaction gets no repeat-read notice, and its notes are shown again. The one-time nudges are not repeated.
- The Stop hook's capture nudge counts the edits of the whole session.

What session start adds, by its `source`:

| `source` | Added |
|---|---|
| `startup`, `fork` (Claude Code); `new`, `startup` (Copilot CLI); none (OpenCode) | The full block: memories, code map, changes since the last session, recent bugs, served repos and the record reminder. |
| `clear` | The full block. If the session id stays the same, Recarto first forgets the record, as for `compact`. |
| `compact` | The full block, after forgetting the conversation's reads, notes and searches. Its memories count as shown again. |
| `resume` | Only the memories the block would show that the conversation has not been shown yet, such as ones recorded or synced since, in the block's format and budget. Nothing when there are none. A session Recarto has no record of gets the full block. |

Every session start also refreshes the code graph, syncs memories, imports the team snapshot and compacts memories, whatever the source. OpenCode's plugin runs session start once per session.

## Search hints

After a Grep or Glob call, or a shell search, Recarto lists the symbols the search names and their known bugs (`search-hints`).

- **Symbols.** A symbol is listed when an identifier in the pattern is exactly its name or qualified name, case included (`handleAugment`, `Engine.Recall`).
  - Part of a longer identifier, a file name or path (`AGENTS.md`, `internal/agents`) and regex syntax (`\b`, `\s`, `.*`) name nothing.
  - A file-name glob names its stem (`*Widget*.go` names `Widget`); a stemless glob like `*.go` names nothing.
  - At most 5, product code before tests and fixtures.
- **Bugs.** A bug is listed, one line with its title, at most 2, when all of these hold:
  - it is anchored to a listed symbol or to the file that defines one;
  - Recarto's branch check does not flag it as referencing code missing from this worktree;
  - this conversation has not been shown it.
- **Size.** A search that names no symbol adds nothing. One tool call adds at most 800 characters of hint, however many searches it runs.
- **Repeats.** A pattern that already got a hint in this conversation, from the shell or Grep/Glob, adds nothing. `recarto status` counts these under `repeated_augments_blocked`.

## Shell searches and file views

Claude Code's native macOS, Linux and WSL builds (2.1.117 and later) have **no Grep or
Glob tools**. Claude searches with `grep`, `rg` and `find` in its Bash tool, and views
files with `cat`, `head`, `tail` or `sed -n`. Those calls never reach a Grep/Glob or Read
hook, so Recarto reads the shell command instead. OpenCode's and Copilot's agents shell
out too, although those hosts keep their dedicated tools. Recarto reads the commands of
Claude Code's `Bash` tool, OpenCode's shell tool (`bash`, or `shell` in 2.x) and
Copilot's `bash` tool. These run in a POSIX shell: bash, sh, zsh, or Git Bash on Windows.

| Command | What Recarto adds |
|---|---|
| `grep`/`egrep`/`fgrep`, `rg`, `ugrep` or `git grep`, searching a directory or the repo | The search hint the Grep tool gives for the same pattern (see [Search hints](#search-hints)). |
| `find`/`bfs` with `-name`, `-iname` or `-path` | The search hint the Glob tool gives. The glob is reduced to its identifier stem; a stemless glob like `*.go` gives nothing. |
| `cat`, `head`, `tail` or `sed -n` printing files, or a grep over named files | The files' anchored bugs, do-not-repeats, guidance and todos. |

How it reads them:

- **Chains and flags.** Commands are found anywhere in `&&`, `;` and `|` chains. Flag
  values (`-A 3`, `-t go`, `--include=*.go`) are never taken for patterns, and every
  `-e` pattern is searched.
- **Paths.** Paths resolve against the shell's working directory, including after a
  leading `cd` and OpenCode's `workdir`. Absolute, `~`-based and Git Bash `/c/…` paths
  work too.
  - A file in a `RECARTO_REPOS` repo surfaces that repo's memories.
  - A file outside every served repo surfaces nothing.
- **One hint per line.** The searches of one command line share one hint and its 800-character cap. The shell-search nudge and the notes of printed files come on top.
- **Nudge.** A conversation's first shell search also points the agent at `search_content`
  and `search_graph`, once per conversation.
- **Dedup** works as for the tools. A pattern searched again, by the shell or by
  Grep/Glob, adds nothing. A memory already shown at
  session start, by a Read or by an earlier view is not shown again in the same conversation.

Recarto adds nothing to:

- commands that filter another command's output, like `ps aux | grep foo` and
  `go test ./... | grep FAIL`, or that aren't `git grep`, like `git log --grep fix`;
- builds, tests, git, and anything else that neither searches nor prints a file;
- writes and edits: searches and views whose output goes to a file (`cat > f <<EOF`,
  `rg Foo > out.txt`, `cat f | head > g`, `{ cat f; } > g`), `sed -i` and scripts.
  The code graph catches up with shell edits through its freshness check on the next
  graph query. Claude Code's `tool_response.bashEditDiff` is not read;
- PowerShell and cmd: Claude Code's `PowerShell` tool, Copilot's `powershell` tool, and
  OpenCode's shell when it runs PowerShell or cmd. On Windows these hosts keep their own
  grep/glob/read tools, which Recarto hooks;
- a command it cannot parse.

Shell views are not counted as reads, so they get no repeat-read notice, large-file
nudge or read count. Recarto never blocks or rewrites a command and never changes its
output; it only adds the hint.

Each host starts Recarto only as often as it has to:

- **Claude Code** runs the shell hook only when a command runs one of the commands above
  (one hook per command, each with an `if` filter).
- **OpenCode**'s plugin checks the command before it starts Recarto.
- **Copilot** filters by tool name only, so every `bash` call starts Recarto. Any other
  command returns as fast as a hook with nothing to do.

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
- **Hooks start fast.** A hook decodes its payload first and opens only what that
  event needs; one with nothing to act on exits at about the cost of a bare process
  start. The git identity is remembered in `<cache>/gitctx/` and re-resolved when the
  checkout's HEAD changes. Measurements: `docs/validation/2026-10-22-hook-startup.md`.
- **Languages:** Go, C#, TypeScript (+TSX), JavaScript (+JSX), Rust, Python via tree-sitter.

## If the store gets corrupted

A SQLite file can be damaged by things outside Recarto's control — a host crash
mid-write, a disk error, a backup tool or antivirus rewriting the cache. The symptom
is `database disk image is malformed (11)`: often reads keep working for a whole
session and the first write is what fails, because the damage sits in pages a read
never touches.

Recarto repairs itself:

- **On open.** A store that fails to open is rebuilt automatically, and the session
  continues.
- **At `recarto serve` startup.** A `PRAGMA quick_check` runs at most once per
  `RECARTO_QUICKCHECK_INTERVAL` (default `24h`; `0` disables it), so a store that
  opens and reads fine but is damaged underneath is caught *before* the session's
  first write rather than by it. Hooks and one-shot CLI commands skip this and stay
  as fast as before.
- **On demand.** `recarto repair` rebuilds this repo's store; `recarto repair --path
  <store.db>` targets any other one (the workspace or feedback store, another repo's
  cache). It has no corruption precondition, so it doubles as "rebuild my store,
  keep my memories".

A repair never edits the damaged file. It builds a replacement next to it, salvaging
row by row everything unregenerable — memories, their refs and MinHash bands,
counters, sessions, sync tombstones/outbox and graph deltas — verifies the result on a
fresh connection, and only then swaps it in. **The code graph is deliberately not
salvaged:** it is derived from your source, and the next session reindexes it.

The damaged file is renamed aside to `<store.db>.corrupt-<timestamp>` (with its
`-wal`/`-shm`), never deleted; quarantines older than 7 days are pruned, and the
newest is always kept. `recarto doctor` reports the last repair and its per-table
salvage counts. If HTTP sync is configured, the next sync re-converges anything
salvage could not recover.

A write that hits corruption *during* a session is not repaired in place — swapping
the file under a live connection pool is not safe — so it returns an actionable error
instead; the next session heals the store. `RECARTO_AUTO_REPAIR=0` disables every
automatic repair (`recarto repair` still works) if you would rather inspect a damaged
store before anything rewrites it.

## Cross-repo workspace memory (RECARTO_WORKSPACE)

Recarto's store is per-repo, but some knowledge spans repos: your preferences, org-wide
decisions, conventions shared by a polyrepo project. Set a workspace name to open a
second, cross-repo store alongside every repo store:

```sh
export RECARTO_WORKSPACE=acme
```

With it set:

- `record_memory` accepts `scope: "workspace"` to store a memory there (default stays
  `repo`); workspace memories are branch-less by construction.
- `recall_memory`, `search_bugs` and `list_memory` search both stores and label
  workspace hits with `scope: "workspace"`; `get_memory`/`forget_memory`/`complete_todo`
  resolve ids across both.
- Session start injects a budgeted "Workspace memory" tier next to the repo tier.
- The workspace store works from **any** directory — including one that is no git repo
  at all — because it is keyed by the name, never by the working directory.
- With HTTP sync configured (§ below), the workspace store syncs under its own
  name-derived bucket key automatically: every machine (or ephemeral sandbox) sharing
  the same `RECARTO_WORKSPACE`, server and key converges.

Workspace memories surface at session start and via recall — never through the
file-anchored just-in-time read/edit hooks, whose repo-relative paths are ambiguous
across repos.

## Serving multiple repos from one session (RECARTO_REPOS)

A session is normally cd'd into the one repo it works on, and Recarto resolves that
repo from the working directory. Some sessions aren't: a multi-repo verifier or test
runner working from a scratch directory with several checkouts beside it. List those
checkouts explicitly and one Recarto process serves them all:

```sh
export RECARTO_REPOS=/work/task/repo1:/work/task/repo2   # filepath list; ';' on Windows
```

Each listed root resolves to the SAME per-repo store a session cd'd into it would
use (the store key is the git common dir, not the serving process's cwd), so the
repo-less session reads and writes exactly the memories the cd'd sessions see:

- Graph and file tools (`search_graph`, `get_code_snippet`, `get_file_outline`, …)
  take an optional `repo` (name or path) to target a served repo; omitted = the cwd
  repo, and an unknown name errors with the served list.
- `recall_memory`/`search_bugs`/`list_memory` merge every served repo's memories
  (hits labeled `repo`); `record_memory`/`record_bug` take `repo` to write into a
  specific repo's store; id lookups resolve across all of them.
- The read/edit hooks map each touched file to the repo that owns it, so a memory
  anchored to `a/b/c.go` in repo1 fires even when the session reads it as
  `/work/task/repo1/a/b/c.go` from outside that repo — and edits reindex the owning
  repo's graph.
- With HTTP sync configured, every served repo's store syncs under its own
  root-commit key, exactly as if a session were cd'd there.

Missing paths, non-git directories and duplicates are skipped silently — an
incomplete list never fails the session. Pair with `RECARTO_WORKSPACE` (§ above)
for the cross-repo store that belongs to no single repo.

## Sharing memory across machines (HTTP sync)

Recarto's store is per-machine: working the same repo from a laptop, a desktop, and an
ephemeral CI/agent sandbox gives each one its own independent memory. HTTP sync converges
just the **memory** (preferences, learnings, decisions, bugs, todos) across machines —
never the code graph, which is derived from source, per-worktree, and rebuilt locally in
seconds regardless of sync.

**Run a server** (one instance is enough — this is per-user data, not a multi-tenant
service):

```sh
RECARTO_SERVER_KEY=<a long random token> recarto server --addr :8080 --data-dir /var/lib/recarto-sync
```

Put it behind a TLS-terminating reverse proxy, or pass `--tls-cert`/`--tls-key` directly.
`RECARTO_SERVER_KEY` is required — the process refuses to start without it.

**Point every machine's client at it**, using the same static key:

```sh
export RECARTO_SYNC_URL=https://recarto-sync.internal
export RECARTO_SYNC_KEY=<the same token>
```

That's it — with both set, sync is fully automatic:

- **Session start**: a bounded pull merges remote changes in before recall runs, so
  injected context reflects other machines' work.
- **Session stop**: a bounded push sends anything recorded this session — the reason an
  ephemeral sandbox that's about to be destroyed still contributes what it learned.
- **`recarto serve` (MCP)**: also syncs on a `RECARTO_SYNC_INTERVAL`-second timer
  (default 60s) for long-lived sessions.
- `recarto sync [--pull] [--push] [--full]` runs it manually; `recarto status` and
  `recarto doctor` show sync state (repo key, cursor, pending pushes, reachability).

Sync is entirely optional and additive: with no `RECARTO_SYNC_URL`/`RECARTO_SYNC_KEY`,
behavior is identical to today, and every sync attempt is best-effort and time-boxed — an
unreachable or slow server is never allowed to block or fail a local operation. **Full
offline mode keeps working exactly as before**; sync just catches up whenever the server
is next reachable.

Machines are matched up by the repo's first (root) commit, not by local path — so a
laptop clone and a fresh ephemeral sandbox clone of the same repo converge automatically.
For a shallow clone (no reachable root commit) or to intentionally group otherwise-
unrelated repos onto one sync key, set `RECARTO_SYNC_REPO` explicitly on every machine
that should share it.

Under the hood: writes merge via the same MinHash near-duplicate detection Recarto
already uses locally (an update wins on last-write-wins by timestamp; independently
authored near-duplicate memories converge into one, reinforced), so two machines editing
concurrently — including while offline — reconcile without conflicts to resolve by hand.
An explicit `forget` propagates as a tombstone; routine local housekeeping (archiving,
dedup, GC) never does, so one machine's cleanup can't delete another's memories.

### Deploy the sync server with Docker Compose

The repo ships a `Dockerfile` and `docker-compose.yml` that run `recarto server` as a
single long-lived container, plain HTTP internally, fronted by your own TLS-terminating
reverse proxy.

```sh
cp .env.example .env
# edit .env: set RECARTO_SERVER_KEY, e.g.  openssl rand -hex 32

docker network create proxy   # once, unless your proxy already has a network to join

docker compose up -d --build
```

Then point every client machine at the proxied URL, same as any other server (§ above):

```sh
export RECARTO_SYNC_URL=https://recarto-sync.example.com
export RECARTO_SYNC_KEY=<the same token as RECARTO_SERVER_KEY>
```

By default the container joins an **external** `proxy` Docker network and publishes no
host port — only a reverse proxy on that network can reach it. If you front it with a
proxy running on the host instead (not dockerized), comment out `networks:` in
`docker-compose.yml` and uncomment the `ports: ["127.0.0.1:8080:8080"]` line so it's only
reachable from localhost.

Whichever proxy you use, it must:
1. **Terminate TLS** — the bearer token travels in the `Authorization` header and must
   never cross an unencrypted public hop.
2. **Forward the request path unmodified** (`/v1/repos/{key}/changes`, `/healthz`).
3. **Allow request bodies up to 32 MB** (`client_max_body_size 32m;` in nginx) and not
   strip `Content-Encoding: gzip`.

Example nginx config:

```nginx
server {
  listen 443 ssl;
  server_name recarto-sync.example.com;
  # ssl_certificate ...; ssl_certificate_key ...;
  client_max_body_size 32m;
  location / {
    proxy_pass http://recarto-sync:8080;   # or 127.0.0.1:8080 for a host proxy
    proxy_set_header Host $host;
  }
}
```

Caddy is a one-liner (auto-TLS, body size and gzip pass through by default):

```
recarto-sync.example.com {
  reverse_proxy recarto-sync:8080
}
```

For Traefik, uncomment and adjust the `labels:` block already present in
`docker-compose.yml`.

**Known caveat:** the server's rate limiter keys on the TCP peer address and does not
read `X-Forwarded-For`, so behind a reverse proxy every client shares one rate-limit
bucket (20 req/s, burst 60). For a personal, few-machine sync setup this is comfortably
within headroom; it only matters if you're fronting a large fleet.

State lives entirely in the named `recarto-sync-data` volume — back it up like any other
SQLite data directory, and it survives `docker compose restart`/upgrades.

### Seed the server from an existing local history

Ordinary sync (above) only ever ships a store's **outbox** — writes made after sync was
configured. If you've been using Recarto on one machine for a long time before setting up
a server, that pre-existing history never automatically travels: the outbox never saw it.
`recarto export` is the one-shot bulk migration path for that case — it reads every
per-repo store under the cache dir directly (every status, including archived/done/
superseded rows, not just what recall currently surfaces; plus the global feedback store,
under a fixed key), never the code graph, and writes it all to one portable archive.

Export and upload in one step:

```sh
recarto export --push   # writes recarto-export-<date>.ndjson.gz, then uploads it
```

Or split across two steps (e.g. write the archive on a laptop, upload from elsewhere):

```sh
recarto export --out history.ndjson.gz
recarto import history.ndjson.gz
```

`--push`/`import` reuse `RECARTO_SYNC_URL`/`RECARTO_SYNC_KEY` (or `--url`/`--key`), so no
extra server-side configuration is needed: the same `POST /v1/import` endpoint authenticates
with the existing `RECARTO_SERVER_KEY` and merges every entry through the identical
MinHash/LWW/tombstone core an ordinary push uses — so a memory that already converged
elsewhere reinforces rather than duplicates, and an import is safely re-runnable. Because
each accepted entry bumps the target repo's sync cursor exactly like a push, **no other
client needs any change to benefit**: every machine's next ordinary session-start pull
picks up the seeded history automatically.

Each project's server key is derived the same way an ordinary client push derives it (the
repo's root commit, not its local path — see above), computed offline from each store's
recorded metadata. A store with no resolvable root commit (a shallow clone) is skipped and
named in the summary. A whole-machine export can't apply `RECARTO_SYNC_REPO` to fix this —
one override would collide every unrelated project it enumerates into the same bucket — so
export that project on its own instead, from inside its checkout, exactly like ordinary sync
requires:

```sh
cd path/to/that/shallow/clone
RECARTO_SYNC_REPO=<key> recarto export --repo --push
```

## Development

```sh
make test     # go test ./...
make vet      # go vet ./...
make fmt      # gofmt -l .
```

CI runs the suite on Linux, macOS and Windows, gofmt-checks every file, and
cross-compiles all three release targets with `zig cc`.

## Contributing

Issues and pull requests are welcome on [GitHub](https://github.com/gerwim/recarto),
and contributions are accepted under the MIT licence. GitHub is a mirror of the main
repository: accepted changes are applied there and reach GitHub through the mirror.
See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## Licence

Recarto is released under the [MIT licence](LICENSE). The binary also contains
third-party code and the potion-base-8M model, each under its own licence;
`recarto licenses` prints all of them.
