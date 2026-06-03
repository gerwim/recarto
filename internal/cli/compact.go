package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Compact runs a compaction/decay pass over this repo's memory store.
func Compact(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	force := fs.Bool("force", false, "ignore the once-per-interval gate")
	dry := fs.Bool("dry-run", false, "report planned changes without writing")
	budget := fs.Int("budget-tokens", 0, "live-set token budget (0 = default)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	rep, err := a.Engine.Compact(ctx, memory.CompactOptions{
		Force: *force, DryRun: *dry, BudgetTokens: *budget,
	})
	if err != nil {
		return err
	}
	if !rep.Ran {
		fmt.Fprintln(w, "compaction skipped (ran recently; use --force to override)")
		return nil
	}
	p := ""
	if *dry {
		p = "[dry-run] "
	}
	fmt.Fprintf(w, "%sepoch:        %d\n", p, rep.Epoch)
	fmt.Fprintf(w, "%ssuperseded:   %d gc'd\n", p, rep.SupersededGCd)
	fmt.Fprintf(w, "%sarchived:     %d\n", p, rep.Archived)
	fmt.Fprintf(w, "%sdeduped:      %d\n", p, rep.Deduped)
	fmt.Fprintf(w, "%sarchive gc:   %d\n", p, rep.ArchiveGCd)
	fmt.Fprintf(w, "%slive tokens:  %d -> %d (budget %d)\n", p, rep.LiveTokensBefore, rep.LiveTokensAfter, rep.Budget)
	return nil
}
