package mcp

import (
	"context"

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
	addTool(s, a, "record_memory",
		"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, or action.",
		recordMemory)
	addTool(s, a, "record_bug",
		"Record a bug with its error message, root cause, and fix (a convenience wrapper over record_memory).",
		recordBug)
	addTool(s, a, "recall_memory",
		"Recall the most relevant memories for the current task — branch-aware, structurally ranked, and token-budgeted.",
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

// Serve runs the MCP server over stdio until the client disconnects. It is the
// body of `recarto serve`.
func Serve(ctx context.Context, a *app.App) error {
	return NewServer(a).Run(ctx, &sdk.StdioTransport{})
}
