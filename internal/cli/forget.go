package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Forget hard-deletes a memory by id — the CLI counterpart of the forget_memory
// MCP tool and the inverse of `remember`. Any trailing words are an advisory
// reason (matching the tool's Reason arg); it is not persisted, since the row is
// gone. Deleting an unknown id is a no-op, reported as such.
//
//	recarto forget <id> [reason...]
func Forget(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("forget: id required")
	}
	id := args[0]
	reason := strings.Join(args[1:], " ")
	ok, err := a.Engine.Forget(ctx, id, reason)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(w, "no memory with id %s\n", id)
		return nil
	}
	fmt.Fprintf(w, "forgotten %s\n", id)
	return nil
}
