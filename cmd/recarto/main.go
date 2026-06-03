// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "recarto: no command (Phase 1A scaffold)")
		os.Exit(0)
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(version.Version)
	case "debug-stress":
		if err := runDebugStress(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "debug-stress:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "recarto: unknown command", os.Args[1])
		os.Exit(2)
	}
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
