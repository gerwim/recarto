package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// known reports whether cmd is a CLI command that needs an open App.
func known(cmd string) bool {
	switch cmd {
	case "status", "doctor", "remember", "recall", "bug", "memory":
		return true
	}
	return false
}

// Run dispatches a CLI invocation (args excludes the program name) and writes
// command output to out. It validates the command before opening the per-repo
// App, so unknown commands and `help` never touch the store.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage(out)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return usage(out)
	}
	if !known(cmd) {
		return fmt.Errorf("unknown command %q (try: status doctor remember recall bug memory)", cmd)
	}

	a, err := app.Open(app.Config{})
	if err != nil {
		return err
	}
	defer a.Close()

	ctx := context.Background()
	w := out
	switch cmd {
	case "status":
		return Status(ctx, a, w)
	case "doctor":
		return Doctor(ctx, a, w)
	case "remember":
		return Remember(ctx, a, w, rest)
	case "recall":
		return Recall(ctx, a, w, rest)
	case "bug":
		return Bug(ctx, a, w, rest)
	case "memory":
		return Memory(ctx, a, w, rest)
	}
	return nil // unreachable: known() gates the switch
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "recarto <command>")
	fmt.Fprintln(w, "  status            repo id, branch, memory counts, ledger")
	fmt.Fprintln(w, "  doctor            health checks (db, WAL, integrity, git)")
	fmt.Fprintln(w, "  remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)")
	fmt.Fprintln(w, "  recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)")
	fmt.Fprintln(w, "  bug add|search    record or search bug memories")
	fmt.Fprintln(w, "  memory show       render all active memory as markdown (--branch)")
	return nil
}
