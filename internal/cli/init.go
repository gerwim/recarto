package cli

import (
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
)

// Init sets up Claude Code integration for the repo rooted at dir, registering the
// recarto binary as `binary` (its own absolute path at runtime). It is idempotent.
func Init(out io.Writer, dir, binary string) error {
	res, err := claudecode.Install(dir, binary)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "recarto init (%s)\n", dir)
	fmt.Fprintf(out, "  mcp server:   %s\n", res.MCPStatus)
	fmt.Fprintf(out, "  hooks:        %s\n", res.HooksStatus)
	fmt.Fprintf(out, "  instructions: %s\n", res.InstructionsStatus)
	return nil
}
