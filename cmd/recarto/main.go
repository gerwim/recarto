// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"git.gerwim.nl/gerwim/recarto/internal/cli"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		if err := cli.Run(nil, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(version.Version)
	case "debug-stress":
		if err := runDebugStress(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "debug-stress:", err)
			os.Exit(1)
		}
	case "update":
		isTTY := isatty.IsTerminal(os.Stdin.Fd())
		err := cli.Update(os.Stdout, os.Stdin, isTTY, os.Args[2:])
		if errors.Is(err, cli.ErrUpdateAvailable) {
			os.Exit(10)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
	case "init":
		// Prompt for install scope when interactive and --scope was not given,
		// then hand off to the normal CLI dispatch.
		args := append([]string{"init"}, maybePromptScope(os.Args[2:])...)
		if err := cli.Run(args, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
	default:
		if err := cli.Run(os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
	}
}

// maybePromptScope asks the user for the install scope (repo or user) when stdin
// is a TTY and --scope was not already supplied, appending the chosen flag. In a
// non-interactive context it leaves args untouched (init defaults to repo scope).
func maybePromptScope(args []string) []string {
	for _, a := range args {
		if a == "--scope" || a == "-scope" || strings.HasPrefix(a, "--scope=") || strings.HasPrefix(a, "-scope=") {
			return args
		}
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return args
	}
	fmt.Fprint(os.Stderr, "Install scope — [r]epo (this project) or [u]ser (global)? [r/u]: ")
	var line string
	fmt.Fscanln(os.Stdin, &line)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "u") {
		return append(args, "--scope", "user")
	}
	return append(args, "--scope", "repo")
}

// runDebugStress performs --iter atomic increments to counters[lifetime,stress].
// It exists to drive the cross-process concurrency test and is not a public command.
func runDebugStress(args []string) error {
	fs := flag.NewFlagSet("debug-stress", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to store.db")
	iter := fs.Int("iter", 100, "increments to perform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("--db is required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	l := ledger.New(st)
	ctx := context.Background()
	for i := 0; i < *iter; i++ {
		if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
			return err
		}
	}
	return nil
}
