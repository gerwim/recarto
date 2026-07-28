package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Repair rebuilds a store file from whatever is still readable in it: the
// precious rows (memories, refs, minhash bands, counters, sessions, sync state,
// graph deltas) are salvaged into a freshly migrated database, the damaged file
// is quarantined next to it, and the code graph is left empty for the next lazy
// reconcile to rebuild from source.
//
// It runs BEFORE app.Open in the CLI dispatch, and deliberately so: the case it
// exists for is a store so damaged that opening it is what fails. It also has no
// corruption precondition, so it doubles as "rebuild my store, keep my memories".
//
// --path targets an explicit store file (another repo's cache, the workspace or
// feedback store); without it, this repo's store is repaired.
func Repair(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("path", "", "store.db to repair (default: this repo's store)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *path
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		gc, err := gitctx.Resolve(gitctx.Real{Dir: cwd}, cwd)
		if err != nil {
			return err
		}
		p, err := cache.StorePath(gc.RepoID)
		if err != nil {
			return err
		}
		target = p
	}

	fmt.Fprintf(out, "repairing %s\n", target)
	stats, err := store.Repair(target)
	if err != nil {
		return fmt.Errorf("repair %s: %w", target, err)
	}

	if !stats.SourceOpen {
		fmt.Fprintln(out, "  the old store could not be read at all — rebuilt empty")
	}
	names := make([]string, 0, len(stats.Tables))
	for t := range stats.Tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		ts := stats.Tables[t]
		line := fmt.Sprintf("  %-20s salvaged=%d", t, ts.Salvaged)
		if ts.Lost > 0 {
			line += fmt.Sprintf(" lost=%d", ts.Lost)
		}
		if ts.Note != "" {
			line += "  (" + ts.Note + ")"
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "  total salvaged rows: %d\n", stats.Salvaged())
	if stats.Quarantine != "" {
		fmt.Fprintf(out, "  quarantined old store: %s\n", stats.Quarantine)
	}
	fmt.Fprintln(out, "  the code graph was left empty; the next session reindexes it from source")
	return nil
}
