package cli

import (
	"context"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/mcp"
)

// Serve runs the MCP server over stdio until the client disconnects. It is a thin
// delegation to internal/mcp; the server itself is covered by the mcp round-trip
// test (it blocks on stdio, so it is not unit-tested here directly).
func Serve(ctx context.Context, a *app.App) error {
	return mcp.Serve(ctx, a)
}
