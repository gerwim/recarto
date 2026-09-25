package mcp

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// NewServer builds the Recarto MCP server with every Phase-1 tool registered
// against a. This is the ONLY file that imports the MCP SDK: tool logic lives in
// SDK-free handlers (handlers.go), so an SDK upgrade or breaking change touches
// this file alone (the same isolation principle as the swappable Lexical backend).
func NewServer(a *app.App) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "recarto", Version: version.Version}, nil)
	addToolSchema(s, a, "record_memory",
		"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, or todo (open work that resurfaces until done). When the new memory looks like it may contradict an existing one (same topic, different claim — or anchored to the same code), the response lists conflict_candidates with a resolution note; review them rather than leaving both claims standing. When a workspace is configured (RECARTO_WORKSPACE), scope=\"workspace\" stores it in the cross-repo workspace store instead of this repo's — use that for preferences and knowledge that apply beyond this one repo.",
		recordMemorySchema(), recordMemory)
	addTool(s, a, "record_bug",
		"Record a bug with its error message, root cause, and fix (a convenience wrapper over record_memory).",
		recordBug)
	addTool(s, a, "recall_memory",
		"Recall the most relevant memories for the current task — hybrid lexical (BM25) + semantic ranking plus structural refs, branch-aware and token-budgeted. Returns nothing when no stored memory is relevant to the task. Each memory reports matched_on (the signals behind it); the result reports truncated/budget_used. When a workspace is configured (RECARTO_WORKSPACE), cross-repo workspace memories are searched too and carry scope=\"workspace\".",
		recallMemory)
	addTool(s, a, "search_bugs",
		"Search recorded bugs (keywords or a plain-words description) before attempting a fix.",
		searchBugs)
	addTool(s, a, "get_memory",
		"Fetch a single memory by id.",
		getMemory)
	addTool(s, a, "list_memory",
		"List stored memories, optionally filtered by kind and branch.",
		listMemory)
	addTool(s, a, "complete_todo",
		"Mark a todo memory done (active → done) so it stops surfacing. No-op if the id is not an active todo.",
		completeTodo)
	addTool(s, a, "forget_memory",
		"Delete a memory recorded in error (hard delete; the memory is gone). No-op if the id is unknown. To replace rather than remove, record a new memory with supersedes_id.",
		forgetMemory)
	addTool(s, a, "search_graph",
		"Search the code graph for symbols (functions, methods, classes, types). Pass `query` as the concept in plain words; when searching by concept, also pass `candidate_symbols` — the identifier names you expect to exist (e.g. AddressLookup, Geocode). The names you supply are what make concept search land; any form works (PascalCase, interface-prefixed, snake_case, or plain words). Each result carries a one-line signature.",
		searchGraph)
	addToolSchema(s, a, "trace_call_path",
		"Trace call chains to/from a symbol (direction: callers or callees). Every chain reads in the calls direction — entry n calls entry n+1 — in BOTH modes: callees list the symbol first then what it calls; callers list the outer caller first and the queried symbol last. The `legend` field restates this on each result, so a chain is never read backwards. Each hop past the root is annotated with `resolution` (self/typed/static/iface/name — how the call was bound) and `confidence` (high/medium/low). Ambiguous low-confidence hops carry the `receiver` expression, appear as sibling branches (the candidate alternatives), and are not expanded further. By default confident hops are shown and low-confidence ones surface only when nothing better exists; set `min_confidence` (0–1) to raise the floor. Trust high/typed/self hops; treat low/name hops as candidates to verify with get_code_snippet. Hops in test files carry is_test; when every caller of a symbol is test code the hint says so (production usually reaches it through a wrapper — find_references sees what the trace cannot). Tracing a type (class/interface/record) dead-ends with a `hint`: calls target its methods, not its declaration, so use find_implementations for implementers and find_references for references. A symbol that resolves but has no edges returns a single root-only chain (plus a `hint`), never an empty result — so empty `chains` always means the symbol is unknown (see `suggestions`), while a root-only chain means it exists but has no callers/callees here (an entrypoint, or dynamically-bound sites): use find_references to find the references the call edges miss.",
		traceCallPathSchema(), traceCallPath)
	addTool(s, a, "get_architecture",
		"Summarize the codebase structure: modules with file/symbol counts and the heaviest inter-module dependencies. The map is budgeted to the highest-signal entries (default 80 each; module_count/dep_count report the full totals, truncated flags a cap); raise `limit` for more or pass `scope` to drill into a subtree.",
		getArchitecture)
	addToolSchema(s, a, "query_graph",
		"Run a structured query over the code graph: AND-combined node filters (kind, name_contains, path, language) plus an optional edge traversal (edge_kind/edge_direction). Use it for exact structured lookups — \"every class in this file\", \"all methods that emit a `calls` edge\" — where search_graph's relevance ranking isn't what you want. At least one filter is REQUIRED: an all-empty query is rejected (it would return the first page of the entire graph ordered by qualified name — i.e. vendored/generated noise — never an answer). For concept/identifier discovery use search_graph instead.",
		queryGraphSchema(), queryGraph)
	addTool(s, a, "search_content",
		"Full-text search across the whole worktree — a complete replacement for the external grep/search tool. Honors .gitignore and covers not just parsed code but config, build, docs and data files the code graph never indexes (.csproj, .json, .yaml, .md, …), so you never have to shell out for a file kind the graph happens not to cover. Finds arbitrary text that search_graph cannot: string/error-message literals, config keys, magic constants, attribute/annotation usages, comments, and call sites inside method bodies. Each match returns its file, line, and the matching line text; matches inside indexed code also carry their enclosing symbol (kind + qualified_name) from the code graph, while matches in unparsed files read as file scope. Literal substring by default; set regex=true for RE2 regex. Filter with path_glob/language (language restricts to indexed code of that language); generated-file matches are demoted; binary files are skipped. The result reports `total` (every matching line) and `truncated`/`per_file_capped` so you know when the page is partial — page deeper with offset+limit (the result order is stable) or narrow the filters, rather than treating a capped page as complete — but when per_file_capped is true, the withheld same-file matches are reached by raising max_per_file, not by offset. Prefer this over the external grep/search tool so codebase search stays in one place.",
		searchContent)
	addTool(s, a, "get_code_snippet",
		"Return the exact source of a symbol instead of reading the whole file. Pass `symbol` alone (file optional) to resolve it by name across the repo — the common case right after a search returns a name, with no need to know the path. `symbol` accepts a bare name, a fully-qualified name, or a `Type.Method` suffix (e.g. `DefaultRequestExecutor.ExecuteAsync`) — the same identifier form trace_call_path/callers/callees emit, so a name copied from a trace resolves as-is. Supply `file` too to pin it. Returns the definition's path and line range, or found=false (not an error) when the symbol is unknown. When the bare name is ambiguous it returns found=false with `candidates` (each path/line/qualified_name) so you can re-query with a qualified name (usually unique) or the file. Overloads of one method (same qualified name, same file — which neither a name nor a file can tell apart) instead come back together: found=true with every body in one snippet, each prefixed by its `// path:line`. Fetch several symbols in one call with symbols (≤10; overflow is listed in skipped, in input order under snippets) — one call replaces N when a search returned several relevant hits.",
		getCodeSnippet)
	addTool(s, a, "get_file_outline",
		"List the symbols defined in one file — kind, name, signature, and line range — in source order, so you grasp a file's structure WITHOUT reading the whole thing, then pull just the symbol you need with get_code_snippet. This is the structural alternative to opening a large file with the Read tool: an outline of a 400-line file costs a handful of lines. Nested symbols carry their `parent`. Prefer this over reading a whole file to find your way around it.",
		getFileOutline)
	addTool(s, a, "find_implementations",
		"Find the types that implement or subclass a given interface/base type (direction=down, default), or the supertypes a type extends (direction=up), from the graph's inheritance edges. This is the structural answer to the classic interface grep — \"what implements IRequestExecutor\" — that search_graph cannot give, because implementers don't share the interface's name. Pass `symbol` as the type's bare or qualified name. Prefer this over grepping for `: TypeName` so codebase navigation stays in recarto.",
		findImplementations)
	addTool(s, a, "find_annotated",
		"Find every symbol decorated with a given attribute/annotation/decorator — e.g. all `[ExtendObjectType]` classes in C#, every `@Component` in TS. This is the structural answer to the attribute grep that search_graph cannot give (the decorated class shares no name with the attribute) and that plain text grep pollutes (it also matches the attribute's own definition, doc comments, and string literals). Each result is the decorated symbol itself (class/method/property) with its kind and location. Matching is C# `Attribute`-suffix tolerant. Pass `attribute` as the name written at the use site. Prefer this over grepping for `[Attr` so codebase navigation stays in recarto.",
		findAnnotated)
	addTool(s, a, "find_references",
		"Find every place a symbol is used — call sites, type usages, imports, annotations — from the graph's edges, plus a word-boundary text sweep for references the graph cannot see (comments, config, unparsed files, dynamically-bound sites), deduplicated by path:line. Each hit carries its file/line, the referencing symbol (`from`), an edge kind (`calls`, `extends`, … or `text` for grep-only hits), and `is_test` so test usages are tell-apart-able at a glance. `resolved` reports whether the symbol matched a graph node at all — false means hits are text-only. NOTE: bare-name queries on common method names (Get, Close, String) can match unrelated same-named call sites among unresolved edges — pass a Type.Method form to narrow. Prefer this over hand-rolling a `\\bName\\b` regex in search_content: one call, structurally ranked, already deduplicated.",
		findReferences)
	addTool(s, a, "get_graph_delta",
		"List what changed in the code graph since your last working session here — the symbol-level detail behind the session-start 'Changed since…' block (same baseline: the last session that actually changed the graph, so /clear or idle sessions don't reset it). Each entry is a symbol with its path, kind, and disposition relative to that baseline: added, removed, or changed; a symbol both added and removed within the window nets out, and a deleted-then-recreated file (branch switch, stash pop) reads as changed. CAVEAT: a renamed file reads as removals at the OLD path — the new path has no journal history — so verify a surprising 'removed' with search_graph before trusting it. Use it to reorient on a branch before trusting recalled knowledge about code that may have moved. total/truncated report completeness; raise limit for more.",
		getGraphDelta)
	addToolSchema(s, a, "report_recarto_issue",
		"Report a problem with Recarto ITSELF (not the user's code) so the maintainer can fix it: a tool that errored, ranked/returned the wrong thing, was confusing to use, or lacked a capability you needed. Routes to a global cross-repo queue. Describe the Recarto interaction and a minimal repro — not proprietary source from this repo.",
		reportRecartoIssueSchema(), reportRecartoIssue)
	return s
}

// addTool adapts an SDK-free handler func(ctx, *app.App, In) (Out, error) into the
// SDK's typed tool handler. Confining the SDK signature to this one adapter keeps
// every handler transport-agnostic and unit-testable.
func addTool[In, Out any](s *sdk.Server, a *app.App, name, desc string,
	fn func(context.Context, *app.App, In) (Out, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			out, err := fn(ctx, a, in)
			// A non-nil err is surfaced by the SDK as a tool error result.
			return nil, out, err
		})
}

// addToolSchema is addTool with an explicit InputSchema, used where a field needs
// a JSON-Schema enum (the jsonschema struct tag is description-only and cannot
// express enum).
func addToolSchema[In, Out any](s *sdk.Server, a *app.App, name, desc string,
	schema *jsonschema.Schema, fn func(context.Context, *app.App, In) (Out, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: desc, InputSchema: schema},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			out, err := fn(ctx, a, in)
			return nil, out, err
		})
}

// Serve runs the MCP server over stdio until the client disconnects. It is the
// body of `recarto serve`.
func Serve(ctx context.Context, a *app.App) error {
	return serve(ctx, a, &sdk.StdioTransport{})
}

// serve is Serve with the transport injected, so tests can drive the full
// serve path — including the background sync goroutine — over an in-memory
// transport.
func serve(ctx context.Context, a *app.App, t sdk.Transport) error {
	if a.HasSync() {
		go runSyncLoop(ctx, a)
	}
	return NewServer(a).Run(ctx, t)
}

// runSyncLoop keeps a long-lived `recarto serve` session converged with the
// remote sync server for as long as the process runs: a periodic tick
// (RECARTO_SYNC_INTERVAL) catches up even an idle session, while
// a.SyncTrigger — signaled by write-tool handlers right after a successful
// record/complete/forget — collapses that wait to near-immediate for the
// common case of "I just wrote something, converge now". Every cycle is
// best-effort (Sync.Once already treats a network failure as non-fatal); this
// goroutine is detached from any single tool call, so a wedged or
// unreachable server can never block or fail one.
func runSyncLoop(ctx context.Context, a *app.App) {
	// Sync is best-effort by contract, but a panic in a goroutine takes down
	// the whole process — for a stdio MCP server that means dying before the
	// handshake and losing every tool, not just sync (the 1.2.0 SIGSEGV when
	// remote sync was configured from a non-git cwd). Contain any panic here:
	// background sync stops for the session, the server keeps serving.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "recarto: background sync disabled after panic: %v\n", r)
		}
	}()
	// a.SyncInterval, not a.Sync.Interval: the primary repo's syncer is nil
	// when the cwd is not a git repo, while the workspace/RECARTO_REPOS
	// syncers this loop exists for are configured.
	ticker := time.NewTicker(a.SyncInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.SyncAll(ctx)
		case <-a.SyncTrigger:
			a.SyncAll(ctx)
		}
	}
}
