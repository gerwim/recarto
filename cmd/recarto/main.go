// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"fmt"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	// Full cobra CLI is wired in Task 8 / Phase 1C. For now, handle --version.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "recarto: no command (Phase 1A scaffold)")
}
