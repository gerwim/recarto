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
	vacuum := fs.Bool("vacuum", false, "after compaction, VACUUM the store so freed pages return to the OS (rewrites the file; needs comparable free disk)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	// Graph-side GC runs regardless of the memory gate: a deleted git worktree
	// leaves its entire graph copy (often the bulk of the store) until swept.
	if dead, derr := a.DeadWorktrees(ctx); derr == nil && len(dead) > 0 {
		if *dry {
			fmt.Fprintf(w, "[dry-run] would prune %d dead worktree(s): %v\n", len(dead), dead)
		} else {
			for _, wt := range dead {
				if err := a.Graph.PruneWorktree(ctx, wt); err != nil {
					return err
				}
			}
			_ = a.Store.Checkpoint(ctx)
			fmt.Fprintf(w, "worktrees pruned: %d (add --vacuum to return the space to the OS)\n", len(dead))
		}
	}
	if *vacuum && !*dry {
		if _, err := a.Store.DB.ExecContext(ctx, `VACUUM`); err != nil {
			return err
		}
		_ = a.Store.Checkpoint(ctx)
		fmt.Fprintln(w, "vacuumed: freed pages returned to the OS")
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
