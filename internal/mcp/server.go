package mcp

import (
	"context"

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
		"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, action, or todo (open work that resurfaces until done).",
		recordMemorySchema(), recordMemory)
	addTool(s, a, "record_bug",
		"Record a bug with its error message, root cause, and fix (a convenience wrapper over record_memory).",
		recordBug)
	addTool(s, a, "recall_memory",
		"Recall the most relevant memories for the current task — branch-aware, structurally ranked, and token-budgeted. Each memory reports matched_on (the signals behind it); the result reports truncated/budget_used.",
		recallMemory)
	addTool(s, a, "search_bugs",
		"Search recorded bugs by keywords before attempting a fix.",
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
		"Trace call chains to/from a symbol (direction: callers or callees). Each hop past the root is annotated with `resolution` (self/typed/static/iface/name — how the call was bound) and `confidence` (high/medium/low). Ambiguous low-confidence hops carry the `receiver` expression, appear as sibling branches (the candidate alternatives), and are not expanded further. By default confident hops are shown and low-confidence ones surface only when nothing better exists; set `min_confidence` (0–1) to raise the floor. Trust high/typed/self hops; treat low/name hops as candidates to verify with get_code_snippet.",
		traceCallPathSchema(), traceCallPath)
	addTool(s, a, "get_architecture",
		"Summarize the codebase structure: modules with file and symbol counts.",
		getArchitecture)
	addToolSchema(s, a, "query_graph",
		"Run a structured query over the code graph (node filters + optional edge traversal).",
		queryGraphSchema(), queryGraph)
	addTool(s, a, "get_code_snippet",
		"Return the exact source of a symbol by file + name, instead of reading the whole file. Returns found=false (not an error) when the symbol is unknown, plus its line range.",
		getCodeSnippet)
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
	return NewServer(a).Run(ctx, &sdk.StdioTransport{})
}
