package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/version"
)

// NewServer builds the Recarto MCP server with every tool a's features leave on
// registered against a.
func NewServer(a *app.App) *sdk.Server {
	return newServer(a.Served(), func(context.Context) (*app.App, error) { return a, nil })
}

// newServer builds the server sv describes; each tool call runs against the App
// get returns. This is the ONLY file that imports the MCP SDK: tool
// logic lives in SDK-free handlers (handlers.go), so an SDK upgrade or breaking
// change touches this file alone (the same isolation principle as the swappable
// Lexical backend).
func newServer(sv app.Served, get func(context.Context) (*app.App, error)) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "recarto", Version: version.Version}, &sdk.ServerOptions{Instructions: serverInstructions(sv.Features)})
	f := sv.Features
	if f.On(features.MemoryTools) {
		// Without a workspace, record_memory has no scope and recall finds no
		// workspace memories, so neither description mentions the store.
		recordDesc := "Record a memory, or reinforce a near-duplicate of one. The result's conflict_candidates are existing memories the new one may contradict: review them, then forget_memory the one that no longer holds or re-record with supersedes_id."
		recallDesc := "Recall the memories most relevant to a task, ranked and token-budgeted. An empty result means nothing stored is relevant. Each memory reports matched_on, the signals that matched it; the result reports truncated and budget_used."
		if sv.Workspace {
			recordDesc += recordWorkspace
			recallDesc += recallWorkspace
		}
		addToolSchema(s, get, "record_memory", recordDesc, recordMemorySchema(sv), recordMemory)
		addTool(s, sv, get, "record_bug",
			"Record a bug with its error message, root cause and fix (a wrapper over record_memory).",
			recordBug)
		addTool(s, sv, get, "recall_memory", recallDesc, recallMemory)
		addTool(s, sv, get, "search_bugs",
			"Search recorded bugs before you attempt a fix.",
			searchBugs)
		addTool(s, sv, get, "get_memory",
			"Fetch a single memory by id.",
			getMemory)
		addTool(s, sv, get, "list_memory",
			"List stored memories, optionally filtered by kind and branch.",
			listMemory)
		if f.On(features.Todos) {
			addTool(s, sv, get, "complete_todo",
				"Mark a todo done so it stops surfacing. No-op if the id is not an active todo.",
				completeTodo)
		}
		addTool(s, sv, get, "forget_memory",
			"Delete a memory recorded in error, permanently. No-op for an unknown id. To replace a memory instead, record the new one with supersedes_id.",
			forgetMemory)
	}
	if f.On(features.GraphTools) {
		addTool(s, sv, get, "search_graph",
			"Find symbols (functions, methods, types) by name or concept, ranked, each with a one-line signature; product code ranks above tests and generated code. When searching by concept, pass candidate_symbols: the names you expect are what make the search land. For text inside code, comments or config, use search_content."+omittedName,
			searchGraph)
		addToolSchema(s, get, "trace_call_path",
			"Trace the call chains from a symbol: what it calls (callees) or what calls it (callers). Every chain reads in the calls direction in both modes: entry n calls entry n+1, so a callers chain ends with the symbol. Each hop after the root has a resolution and a confidence; low-confidence hops are candidates to verify with get_code_snippet. Empty chains means the symbol is unknown (see suggestions); a single root-only chain means it has no callers or callees in the graph, so try find_references. Tracing a type dead-ends, since calls target its methods: use find_implementations or find_references.",
			traceCallPathSchema(sv), traceCallPath)
		addTool(s, sv, get, "get_architecture",
			"Summarize the codebase to orient in unfamiliar code: modules with file and symbol counts, and the heaviest dependencies between them. Only the top entries are listed: module_count and dep_count give the totals, and truncated flags a cap. Pass scope to drill into a subtree.",
			getArchitecture)
		addToolSchema(s, get, "query_graph",
			"Exact structured lookup in the code graph, e.g. every class in one file: node filters (kind, name_contains, path, language) combined with AND, plus an optional edge filter. At least one filter is required. To discover symbols by concept or name, use search_graph."+omittedName,
			queryGraphSchema(sv), queryGraph)
		addTool(s, sv, get, "search_content",
			"Text search over the whole worktree, honouring .gitignore: code, and also the config, docs and data files the graph doesn't index. Use it instead of grep. Each match has its path, line and text, and in indexed code its enclosing symbol. total counts every match. The page is partial when truncated (page with offset) or per_file_capped (raise max_per_file for the rest of a file's matches).",
			searchContent)
		addTool(s, sv, get, "get_code_snippet",
			"Return a symbol's source with its path and line range, instead of reading the whole file. found=false means the symbol is unknown or, with candidates, ambiguous: re-query with a qualified name or the file. Overloads come back together in one snippet. Pass symbols to fetch several in one call.",
			getCodeSnippet)
		addTool(s, sv, get, "get_file_outline",
			"List the symbols of one file with kind, signature and line range, in source order, nested ones with their parent: its structure without reading the whole file. Then fetch the symbol you need with get_code_snippet."+omittedName,
			getFileOutline)
		addTool(s, sv, get, "find_implementations",
			"Find the implementers and subtypes of a type, or its supertypes, from inheritance edges. Grep and search_graph can't answer this: implementers don't share the type's name."+omittedName,
			findImplementations)
		addTool(s, sv, get, "find_annotated",
			"Find the symbols that carry an attribute, annotation or decorator, such as every @Component class. Each result is the decorated symbol with its kind and location. search_graph can't find these, since a symbol doesn't share its attribute's name, and grep also matches the attribute's definition, comments and strings."+omittedName,
			findAnnotated)
		addTool(s, sv, get, "find_references",
			"Find every use of a symbol: call sites, type usages, imports and annotations from graph edges, plus a word-boundary text sweep for what the graph can't see, deduplicated by path:line. Use it instead of a search_content regex for the name. Each hit has its kind (text for sweep-only hits), the referencing symbol and is_test. resolved=false means the symbol matched no graph node, so every hit is text-only. A common bare method name, such as Close, can match unrelated call sites: pass Type.Method.",
			findReferences)
		addTool(s, sv, get, "get_graph_delta",
			"List the symbols added, removed or changed in the code graph since the last session here that changed it. Check it before you trust recalled knowledge about code that may have moved. A renamed file reads as removals at its old path: verify a surprising removal with search_graph. total and truncated report completeness.",
			getGraphDelta)
	}
	if f.On(features.IssueReports) {
		addToolSchema(s, get, "report_recarto_issue",
			"Report a problem with Recarto itself, not with the user's code, to the maintainer's cross-repo queue. Describe the Recarto interaction and a minimal repro, without proprietary source from this repo.",
			reportRecartoIssueSchema(sv), reportRecartoIssue)
	}
	return s
}

// Keep the canonical-identifier explanation identical across list tools.
const omittedName = " Results omit name when it equals qualified_name or its last segment."

const (
	recordWorkspace = " scope=workspace stores it in the cross-repo workspace store, for knowledge that applies beyond this repo."
	recallWorkspace = " Memories in the cross-repo workspace store are searched too and carry scope=workspace."
)

// addTool adapts an SDK-free handler func(ctx, *app.App, In) (Out, error) into the
// SDK's typed tool handler, with the input schema sv calls for. Confining the
// SDK signature to this one adapter keeps every handler transport-agnostic and
// unit-testable.
func addTool[In, Out any](s *sdk.Server, sv app.Served, get func(context.Context) (*app.App, error),
	name, desc string, fn func(context.Context, *app.App, In) (Out, error)) {
	addToolSchema(s, get, name, desc, inputSchema[In](sv), fn)
}

// addToolSchema is addTool with an explicit InputSchema, used where a field needs
// a JSON-Schema enum (the jsonschema struct tag is description-only and cannot
// express enum).
func addToolSchema[In, Out any](s *sdk.Server, get func(context.Context) (*app.App, error),
	name, desc string, schema *jsonschema.Schema, fn func(context.Context, *app.App, In) (Out, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: desc, InputSchema: schema},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			a, err := get(ctx)
			if err != nil {
				var zero Out
				return nil, zero, err
			}
			out, err := fn(ctx, a, in)
			return nil, out, err
		})
}

// Serve runs the MCP server over stdio until the client disconnects. It is the
// body of `recarto serve`: the handshake is answered before any store opens.
func Serve(ctx context.Context, cfg app.Config) error {
	sv, cfg, err := prepareServe(ctx, cfg)
	if err != nil {
		return err
	}
	return serve(ctx, sv, func(ctx context.Context) (*app.App, error) { return app.OpenContext(ctx, cfg) },
		&sdk.StdioTransport{})
}

// prepareServe returns what a server offers and the config its open uses, with
// the code-less decision fixed now, before the handshake is answered: a file
// created after the server started never changes what tools without repo act on.
func prepareServe(ctx context.Context, cfg app.Config) (app.Served, app.Config, error) {
	sv, err := app.Describe(cfg)
	if err != nil {
		return app.Served{}, cfg, err
	}
	if cfg.DecideCodeless {
		codeless, err := app.CodelessNow(ctx, cfg)
		if err != nil {
			return app.Served{}, cfg, err
		}
		cfg.Codeless = &codeless
	}
	return sv, cfg, nil
}

// serve is Serve with the opener and transport injected, so tests can drive the
// full serve path — including the background open and sync goroutine — over an
// in-memory transport. It owns, and closes, the App open returns.
func serve(ctx context.Context, sv app.Served, open func(context.Context) (*app.App, error), t sdk.Transport) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Hooks point at the MCP tools only while a server marks their repo
	// (app.ServerRunning); marking before Run means every mark is held before
	// the handshake can be answered.
	for _, id := range sv.RepoIDs {
		release, err := app.MarkServing(id)
		if err != nil {
			// Unmarked, that repo's hooks only leave out their pointers at the tools; serving matters more.
			fmt.Fprintf(os.Stderr, "recarto: hooks will not point at the MCP tools: %v\n", err)
			continue
		}
		defer release()
	}
	o := startOpen(ctx, open)
	err := newServer(sv, o.get).Run(ctx, t)
	// The cancel interrupts a running check, but an open in progress may be
	// repairing: leaving mid-repair risks the quarantine/install window, so wait.
	cancel()
	<-o.done
	if o.a != nil {
		err = errors.Join(err, o.a.Close())
	}
	return err
}

// openWait is how long a tool call waits for the store to open before failing;
// a variable so tests can change it.
var openWait = 10 * time.Second

var errStillOpening = errors.New("recarto is still opening its store: the integrity check or repair it runs at start-up can take minutes on a large store. Try again in a few minutes")

// opening is the background open of a serving process's App.
type opening struct {
	done chan struct{}
	a    *app.App
	err  error
}

// startOpen runs open in the background and, once it succeeds, the sync loop.
func startOpen(ctx context.Context, open func(context.Context) (*app.App, error)) *opening {
	o := &opening{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		o.a, o.err = open(ctx)
		if o.err != nil {
			// Closing the session interrupts the open; that is not a failure to report.
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "recarto: %v\n", o.err)
			}
			return
		}
		if o.a.HasSync() {
			go runSyncLoop(ctx, o.a)
		}
	}()
	return o
}

// get returns the opened App, waiting up to openWait for the open to finish.
func (o *opening) get(ctx context.Context) (*app.App, error) {
	select {
	case <-o.done:
	default:
		t := time.NewTimer(openWait)
		defer t.Stop()
		select {
		case <-o.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			return nil, errStillOpening
		}
	}
	if o.err != nil {
		return nil, fmt.Errorf("recarto could not open its store: %w", o.err)
	}
	return o.a, nil
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
