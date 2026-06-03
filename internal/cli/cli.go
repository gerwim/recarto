package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// known reports whether cmd is a CLI command that needs an open App.
func known(cmd string) bool {
	switch cmd {
	case "status", "doctor", "remember", "recall", "bug", "memory", "serve", "hook":
		return true
	}
	return false
}

// Run dispatches a CLI invocation (args excludes the program name) and writes
// command output to out. It validates the command before opening the per-repo
// App, so unknown commands and `help` never touch the store. `init` configures
// the repo's files and needs no store. Hooks are non-blocking: a hook error is
// reported to stderr but Run still returns nil so the agent is never blocked.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage(out)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return usage(out)
	}

	// init operates on the working directory's files; no store needed.
	if cmd == "init" {
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return Init(out, dir, binaryPath())
	}

	if !known(cmd) {
		return fmt.Errorf("unknown command %q (try: init serve hook status doctor remember recall bug memory)", cmd)
	}

	a, err := app.Open(app.Config{})
	if err != nil {
		if cmd == "hook" {
			// Hooks must never block the agent, even if the store cannot open.
			fmt.Fprintln(os.Stderr, "recarto hook:", err)
			return nil
		}
		return err
	}
	defer a.Close()

	ctx := context.Background()
	switch cmd {
	case "status":
		return Status(ctx, a, out)
	case "doctor":
		return Doctor(ctx, a, out)
	case "remember":
		return Remember(ctx, a, out, rest)
	case "recall":
		return Recall(ctx, a, out, rest)
	case "bug":
		return Bug(ctx, a, out, rest)
	case "memory":
		return Memory(ctx, a, out, rest)
	case "serve":
		return Serve(ctx, a)
	case "hook":
		if err := Hook(ctx, a, os.Stdin, out, rest); err != nil {
			fmt.Fprintln(os.Stderr, "recarto hook:", err)
		}
		return nil // hooks never block
	}
	return nil // unreachable: known() gates the switch
}

// binaryPath returns the absolute path of the running recarto binary (so hooks
// and .mcp.json invoke the exact binary), falling back to the bare name.
func binaryPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "recarto"
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "recarto <command>")
	fmt.Fprintln(w, "  init              set up Claude Code integration (.mcp.json, hooks, RECARTO.md)")
	fmt.Fprintln(w, "  serve             run the MCP server over stdio")
	fmt.Fprintln(w, "  hook <event>      run a Claude Code hook (session-start|post-read|stop)")
	fmt.Fprintln(w, "  status            repo id, branch, memory counts, ledger")
	fmt.Fprintln(w, "  doctor            health checks (db, WAL, integrity, git)")
	fmt.Fprintln(w, "  remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)")
	fmt.Fprintln(w, "  recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)")
	fmt.Fprintln(w, "  bug add|search    record or search bug memories")
	fmt.Fprintln(w, "  memory show       render all active memory as markdown (--branch)")
	return nil
}
