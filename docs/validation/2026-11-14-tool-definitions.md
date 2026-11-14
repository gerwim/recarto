# Shorter MCP tool definitions: sizes, facts and an agent trial

## What changed

The 20 tool descriptions and about half the parameter descriptions are shorter. They keep what the tool returns, when to choose it, the input forms an agent can't guess and how to read results that are easy to misread. `repo` is offered only when the session serves more than one repo (`RECARTO_REPOS`), and `record_memory`'s `scope`, together with the workspace sentences of `record_memory` and `recall_memory`, only when `RECARTO_WORKSPACE` is set. In a setup without them, a call that passes `repo` or `scope` is rejected like any unknown argument. Every other call works as before.

## Measure

Taken from the server's `tools/list`, with every feature on:

- description: the characters of `description`;
- input schema: `len(json.dumps(inputSchema))` (Python defaults: `", "` and `": "` separators, non-ASCII escaped as `\uXXXX`);
- parameter descriptions: the characters of every `inputSchema.properties.*.description`;
- definition: description + input schema. Tool names and output schemas are not counted.

On 1.7.0 this gives 10,772 + 12,997 = 23,769 characters, and 6,328 characters of parameter descriptions. "Before" below is 2bec1b8, the branch base: Stage 4 moved usage text into descriptions, so it starts at 11,192. "After" is this branch.

```sh
python3 -I docs/validation/2026-11-14-tool-definitions-measure.py BINARY [--several-repos] [--workspace x]
```

The guards in `internal/mcp/tooldefs_test.go` measure the same way.

## Totals

| Setup | Descriptions | Input schemas | Parameter descriptions | Definitions |
|---|---|---|---|---|
| Before, any setup | 11,192 | 12,997 | 6,328 | 24,189 |
| After: one repo, no workspace | 5,093 | 9,029 | 3,095 | **14,122** (limit 18,000) |
| After: several repos | 5,093 | 10,399 | 3,854 | 15,492 |
| After: workspace | 5,286 | 9,154 | 3,141 | 14,440 |
| After: several repos + workspace | **5,286** (limit 7,200) | 10,524 | **3,900** (limit 6,300) | 15,810 |

The schema shape is unchanged. With `--several-repos --workspace x --dump-shape FILE` on both binaries (every `description` key removed), the two dumps are identical: same properties, types, enums and `required`.

## Per tool (one repo, no workspace)

| Tool | Description before | Description after | Definition before | Definition after |
|---|---|---|---|---|
| complete_todo | 101 | 78 | 265 | 242 |
| find_annotated | 742 | 385 | 1,270 | 674 |
| find_implementations | 571 | 240 | 1,133 | 569 |
| find_references | 848 | 498 | 1,299 | 747 |
| forget_memory | 167 | 140 | 420 | 393 |
| get_architecture | 312 | 273 | 943 | 508 |
| get_code_snippet | 1,173 | 293 | 1,973 | 791 |
| get_file_outline | 525 | 282 | 856 | 450 |
| get_graph_delta | 835 | 320 | 1,159 | 475 |
| get_memory | 28 | 28 | 171 | 171 |
| list_memory | 61 | 61 | 358 | 358 |
| query_graph | 656 | 332 | 1,461 | 974 |
| recall_memory | 441 | 228 | 1,232 | 987 |
| record_bug | 100 | 87 | 824 | 671 |
| record_memory | 608 | 231 | 2,025 | 1,254 |
| report_recarto_issue | 330 | 198 | 1,433 | 1,061 |
| search_bugs | 85 | 46 | 266 | 230 |
| search_content | 1,402 | 390 | 2,988 | 1,355 |
| search_graph | 565 | 385 | 1,675 | 1,065 |
| trace_call_path | 1,642 | 598 | 2,438 | 1,147 |
| **total** | **11,192** | **5,093** | **24,189** | **14,122** |

## Facts

Where the new text states each fact. `TestToolDefinitionsKeepFacts` fails if one of these phrases goes.

| Tool | Fact | Where | Text |
|---|---|---|---|
| search_graph | pass candidate_symbols when searching by concept | description | "When searching by concept, pass candidate_symbols: the names you expect are what make the search land." |
| get_code_snippet | bare name, qualified name or Type.Method suffix | `symbol` | "a bare name, a qualified name or a Type.Method suffix, as search and trace results print it" |
| get_code_snippet | `file` is optional | `file` | "optional repo-relative path; without it the symbol is resolved across the repo" |
| get_code_snippet | an ambiguous name returns candidates | description | "found=false means the symbol is unknown or, with candidates, ambiguous: re-query with a qualified name or the file." |
| get_code_snippet | several symbols in one call | description, `symbols` | "Pass symbols to fetch several in one call." / "up to 10 symbols, returned in order under snippets; extras are listed in skipped" |
| trace_call_path | calls direction in both modes | description | "Every chain reads in the calls direction in both modes: entry n calls entry n+1, so a callers chain ends with the symbol." |
| trace_call_path | low-confidence hops are candidates to verify | description | "low-confidence hops are candidates to verify with get_code_snippet." |
| trace_call_path | empty chains = unknown; root-only chain = no callers or callees | description | "Empty chains means the symbol is unknown (see suggestions); a single root-only chain means it has no callers or callees in the graph, so try find_references." |
| trace_call_path | a type dead-ends: use find_implementations or find_references | description | "Tracing a type dead-ends, since calls target its methods: use find_implementations or find_references." |
| search_content | whole worktree, incl. files the graph doesn't index, honours .gitignore | description | "Text search over the whole worktree, honouring .gitignore: code, and also the config, docs and data files the graph doesn't index." |
| search_content | literal by default, regex=true for regex | `pattern` | "the text to find: a literal substring, or an RE2 regex with regex=true" |
| search_content | total, truncated, per_file_capped and how to get the rest | description | "total counts every match. The page is partial when truncated (page with offset) or per_file_capped (raise max_per_file for the rest of a file's matches)." |
| find_references | graph edges plus a text sweep | description | "call sites, type usages, imports and annotations from graph edges, plus a word-boundary text sweep for what the graph can't see" |
| find_references | resolved=false means text-only hits | description | "resolved=false means the symbol matched no graph node, so every hit is text-only." |
| find_references | common bare method name: pass Type.Method | description | "A common bare method name, such as Close, can match unrelated call sites: pass Type.Method." |
| query_graph | at least one filter is required | description | "At least one filter is required." |
| find_implementations | what grep and search_graph can't answer | description | "Grep and search_graph can't answer this: implementers don't share the type's name." |
| find_annotated | what grep and search_graph can't answer | description | "search_graph can't find these, since a symbol doesn't share its attribute's name, and grep also matches the attribute's definition, comments and strings." |
| get_graph_delta | baseline: the last session that changed the graph | description | "since the last session here that changed it" |
| get_graph_delta | a renamed file reads as removed at its old path | description | "A renamed file reads as removals at its old path: verify a surprising removal with search_graph." |
| record_memory | the kinds | `kind` | "preference, learning, decision, do_not_repeat, bug or todo (open work that resurfaces until done)" (also the enum) |
| record_memory | conflict_candidates need a review | description | "The result's conflict_candidates are existing memories the new one may contradict: review them, then forget_memory the one that no longer holds or re-record with supersedes_id." |
| report_recarto_issue | about Recarto itself, no proprietary source | description | "Report a problem with Recarto itself, not with the user's code ... without proprietary source from this repo." |

Shortened parameter descriptions keep their accepted values and defaults (for example `direction`: "down (default) or up (supertypes)"; `limit` of get_architecture: "(default 80)"; `max_per_file`: "(default 5)"). Two rules moved instead of going: the no-proprietary-source rule from `detail` to the report_recarto_issue description, and the paging rule from `offset` to the search_content description.

## Trial

Can agents still use the tools with the shorter definitions? Each task went to a fresh agent, once with the old server and once with the new one.

- Model: `claude-sonnet-5-5`, Claude Code 2.1.310.
- Each run: `claude --bare -p <prompt> --model claude-sonnet-5-5 --mcp-config <mcp.json> --strict-mcp-config --tools "" --allowedTools mcp__recarto --max-turns 30 --output-format stream-json --verbose --no-session-persistence`, cwd = the snapshot, stdin closed, `ENABLE_TOOL_SEARCH=false` so all 20 definitions load up front. `--bare` skips hooks, CLAUDE.md and the host login; the key comes from `APP_ANTHROPIC_API_KEY`, passed to `claude` only.
- The script checks each run's init message: the agent's tools are exactly the 20 `mcp__recarto__*` tools.
- The environment drops every `RECARTO_*`, `CLAUDECODE`, `CLAUDE_CODE_*` and `ANTHROPIC_*` variable. Recarto runs with `RECARTO_CONFIG` set to a missing file (every feature on) and a fresh copy of a prepared cache per run.
- Old binary: 2bec1b8. New binary: this branch. Snapshot: a standalone clone at 2bec1b8; every answer below is about that commit.
- Staged delta for T14, the same in both arms: index a stub `internal/trialdelta/trialdelta.go` (package clause only), start a session, write `TrialDeltaMarker` and `TrialDeltaProbe` into it, `index --force`, stop the session, start another. The stub is needed because a file's first index journals no deltas, and `--force` because an untracked file's edit leaves the freshness token unchanged. The script checks that both names show at session start in each arm.
- Prompt, the same in both arms: "Answer this question about the repository in the current directory, using the tools you have. End your reply with a last line that starts with "ANSWER:" and holds only the answer. Question: ...".
- Grading: the text after the last `ANSWER:` must match every required regex and no forbidden one (see the script). I also read every answer; no grade was overridden.

### Tasks

| ID | intended tool | question | expected answer |
|---|---|---|---|
| T01 | search_graph | Which function in this repository decides which served repository a Recarto MCP tool call operates on, given the optional repo argument? Name the function and its file. | repoTarget, internal/mcp/target.go |
| T02 | get_code_snippet | How long does a Recarto code-graph tool call wait for an in-flight graph refresh before it answers from the data it already has? Name the variable that sets it and its value. | freshWait = 3 * time.Second (internal/mcp/graph.go) |
| T03 | get_file_outline | List every method of the App type that is defined in internal/app/app.go. | SignalSync, Close, AllRepos, HasSync, SyncInterval, SyncAll, writeMeta |
| T04 | trace_call_path (callers) | Which functions call ensureFresh? List all of them. | searchGraph, traceCallPath, getArchitecture, queryGraph, searchContent, getCodeSnippet, getFileOutline, findImplementations, findAnnotated, findReferences, getGraphDelta |
| T05 | trace_call_path (callees) | Starting from the Serve function in internal/mcp/server.go, which functions of this repository does it reach within two calls? | serve; then HasSync, runSyncLoop, NewServer (Run is the MCP SDK's method) |
| T06 | find_references | Which non-test functions call the memory engine's CompleteTodo method? Give each function and its file. | todoDone (internal/cli/todo.go), completeTodo (internal/mcp/handlers.go) |
| T07 | find_implementations (down) | Which types in the C# test fixtures implement or extend the IShape interface directly? | Circle (record), Margin (struct), IResizable (interface) |
| T08 | find_implementations (up) | Which interface does the C# fixture class CachedRepository implement? | Fixture.Core.Storage.IRepository (IRepository<string>) |
| T09 | find_annotated | Which C# symbols in the test fixtures are marked with the Obsolete attribute? | Fixture.App.CachedRepository, Fixture.Core.Geometry.GeoMath.LegacyDistance |
| T10 | query_graph | List every C# record type declared in the repository. | GeoPoint, Pixel, Circle, Shape, SyncReport |
| T11 | search_content (unindexed file) | Which version of golangci-lint does the repository's provisioning script install? | 2.15.0 (.kanship/setup.sh) |
| T12 | search_content (literal) | Which function returns the error message "pass symbol (one) or symbols (batch)"? | getCodeSnippet (internal/mcp/graph.go) |
| T13 | get_architecture | In the module map of this repository, which module does the tools module depend on, and how many references does that dependency carry? | internal, 31 |
| T14 | get_graph_delta | Which symbols were added to the code since the last working session in this repository? | TrialDeltaProbe, TrialDeltaMarker (internal/trialdelta/trialdelta.go) |

### Results (round 2, the round of record)

| ID | arm | solved | answer | tools called | turns | cost (USD) |
|---|---|---|---|---|---|---|
| T01 | old | yes | repoTarget in internal/mcp/target.go | search_graph | 2 | 0.009 |
| T01 | new | yes | repoTarget in internal/mcp/target.go | search_graph | 2 | 0.004 |
| T02 | old | yes | `freshWait` (in `internal/mcp/graph.go`), set to 3 seconds (`3 * time.Second`) | search_content, search_content, get_code_snippet, search_content, search_content | 6 | 0.042 |
| T02 | new | yes | `freshWait` = 3 seconds (`3 * time.Second`, in `internal/mcp/graph.go`) | search_content, search_content, search_content, get_code_snippet, get_file_outline, search_content, get_code_snippet, search_content | 9 | 0.047 |
| T03 | old | yes | SignalSync, Close, AllRepos, HasSync, SyncInterval, SyncAll, writeMeta | get_file_outline | 2 | 0.004 |
| T03 | new | yes | SignalSync, Close, AllRepos, HasSync, SyncInterval, SyncAll, writeMeta | get_file_outline | 2 | 0.003 |
| T04 | old | yes | searchGraph, traceCallPath, getArchitecture, queryGraph, searchContent, getCodeSnippet, getFileOutline, findImplementations, findAnnotated, findReferences, getGraphDelta | find_references | 2 | 0.005 |
| T04 | new | yes | searchGraph, traceCallPath, getArchitecture, queryGraph, searchContent, getCodeSnippet, getFileOutline, findImplementations, findAnnotated, findReferences, getGraphDelta | trace_call_path, find_references | 3 | 0.031 |
| T05 | old | yes | serve, HasSync, runSyncLoop, NewServer | trace_call_path, get_code_snippet | 3 | 0.013 |
| T05 | new | yes | serve, HasSync, runSyncLoop, NewServer | trace_call_path, get_code_snippet | 3 | 0.008 |
| T06 | old | yes | `todoDone` (internal/cli/todo.go) and `completeTodo` (internal/mcp/handlers.go) | find_references | 2 | 0.004 |
| T06 | new | yes | `todoDone` (internal/cli/todo.go) and `completeTodo` (internal/mcp/handlers.go) | find_references | 2 | 0.004 |
| T07 | old | yes | Circle, IResizable, Margin | find_implementations | 2 | 0.005 |
| T07 | new | yes | Circle (record), IResizable (interface), Margin (struct) | find_implementations | 2 | 0.004 |
| T08 | old | yes | IRepository | find_implementations | 2 | 0.004 |
| T08 | new | yes | IRepository | find_implementations, search_content | 3 | 0.006 |
| T09 | old | yes | Fixture.App.CachedRepository (class) and Fixture.Core.Geometry.GeoMath.LegacyDistance (method) | find_annotated | 2 | 0.005 |
| T09 | new | yes | Fixture.App.CachedRepository (class) and Fixture.Core.Geometry.GeoMath.LegacyDistance (method) | find_annotated | 2 | 0.004 |
| T10 | old | yes | GeoPoint, Pixel, Shape, Circle, SyncReport | query_graph, search_content | 3 | 0.014 |
| T10 | new | yes | GeoPoint, Pixel, Shape, Circle, SyncReport | query_graph, search_content | 3 | 0.006 |
| T11 | old | yes | 2.15.0 | search_content, search_content | 3 | 0.008 |
| T11 | new | yes | 2.15.0 | search_content, search_content | 3 | 0.005 |
| T12 | old | yes | getCodeSnippet | search_content | 2 | 0.004 |
| T12 | new | yes | getCodeSnippet | search_content | 2 | 0.004 |
| T13 | old | yes | internal, 31 | get_architecture | 2 | 0.003 |
| T13 | new | yes | internal (31 references) | get_architecture | 2 | 0.002 |
| T14 | old | yes | TrialDeltaMarker (type) and TrialDeltaProbe (function), both in internal/trialdelta/trialdelta.go | get_graph_delta | 2 | 0.005 |
| T14 | new | yes | TrialDeltaMarker (type) and TrialDeltaProbe (function), both in internal/trialdelta/trialdelta.go | get_graph_delta | 2 | 0.004 |

Solved with the old definitions: 14 of 14. Solved with the new: 14 of 14. Regressions: 0. No run was retried.

### Rounds

- **Round 1** (void). The snapshot was a `git worktree` under `/tmp`. Its entry in the shared repository's `.git/worktrees/` was pruned while the trial ran, so from the first task on the server saw a non-git directory with an empty store. Both arms solved T01 to T13 and both failed T14 ("no prior working session has changed the code graph"), since the staged delta was in a store they no longer opened. The definitions were not changed between rounds.
- **Round 2**. The snapshot is a standalone clone, so nothing outside it can prune it. Results above.

### Reproduce

```sh
T=/tmp/trial; mkdir -p $T
git clone -q --no-checkout . $T/old-src && git -C $T/old-src checkout -q --detach 2bec1b8
(cd $T/old-src && go build -o $T/recarto-old ./cmd/recarto)
go build -o $T/recarto-new ./cmd/recarto
git clone -q --no-checkout . $T/snapshot && git -C $T/snapshot checkout -q --detach 2bec1b8
python3 -I docs/validation/2026-11-14-tool-definitions-trial.py \
  --old $T/recarto-old --new $T/recarto-new --snapshot $T/snapshot --out $T/out
```

Runs are resumable: a run whose transcript already has a result is skipped. Use a fresh snapshot and `--out` to start over.
