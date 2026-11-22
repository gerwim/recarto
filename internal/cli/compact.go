package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/memory"
)

// Compact runs a compaction/decay pass over this repo's memory store.
func Compact(ctx context.Context, a *app.App, w io.Writer, args []string) (rerr error) {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	force := fs.Bool("force", false, "ignore the once-per-interval gate")
	dry := fs.Bool("dry-run", false, "report planned changes without writing")
	budget := fs.Int("budget-tokens", 0, "live-set token budget (0 = default)")
	vacuum := fs.Bool("vacuum", false, "after compaction, optimize the FTS indexes, then VACUUM the store so freed pages return to the OS (rewrites the file; needs comparable free disk)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	// Graph-side GC runs regardless of the memory gate: a deleted or unused git
	// worktree keeps its entire graph copy (often the bulk of the store) until swept.
	wts, err := a.PrunableWorktrees(ctx)
	// A bad days value must not stop the sweep of deleted worktrees or the memory
	// pass, so it is reported after both.
	defer func() { rerr = errors.Join(rerr, wts.DaysErr) }()
	if err != nil {
		return err
	}
	if *dry {
		if len(wts.Dead) > 0 {
			if _, err := fmt.Fprintf(w, "[dry-run] would prune %d dead worktree(s): %v\n", len(wts.Dead), wts.Dead); err != nil {
				return err
			}
		}
		if len(wts.Unused) > 0 {
			if _, err := fmt.Fprintf(w, "[dry-run] would prune %d worktree(s) not used for more than %d days:\n", len(wts.Unused), wts.Days); err != nil {
				return err
			}
			for _, u := range wts.Unused {
				if _, err := fmt.Fprintf(w, "  %s  last used %s\n", u.Worktree, u.LastUsed.Format("2006-01-02 15:04")); err != nil {
					return err
				}
			}
		}
	} else {
		n, err := a.PruneWorktrees(ctx, wts)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := fmt.Fprintf(w, "worktrees pruned: %d (add --vacuum to return the space to the OS)\n", n); err != nil {
				return err
			}
		}
	}
	rep, memErr := a.Engine.Compact(ctx, memory.CompactOptions{
		Force: *force, DryRun: *dry, BudgetTokens: *budget,
	})
	if memErr == nil {
		if err := writeCompactReport(w, rep, *dry); err != nil {
			return err
		}
	}
	if *vacuum && !*dry {
		// Run last so pages freed by the prune and memory pass are returned too.
		// The prune alone can free most of the store, so reclaim even when the
		// memory pass is skipped or fails.
		if err := reclaimSpace(ctx, a, w); err != nil {
			return errors.Join(memErr, err)
		}
	}
	return memErr
}

func writeCompactReport(w io.Writer, rep memory.CompactReport, dry bool) error {
	if !rep.Ran {
		_, err := fmt.Fprintln(w, "compaction skipped (ran recently; use --force to override)")
		return err
	}
	p := ""
	if dry {
		p = "[dry-run] "
	}
	_, err := fmt.Fprintf(w,
		"%sepoch:        %d\n%ssuperseded:   %d gc'd\n%sarchived:     %d\n%sdeduped:      %d\n%sarchive gc:   %d\n%slive tokens:  %d -> %d (budget %d)\n",
		p, rep.Epoch, p, rep.SupersededGCd, p, rep.Archived, p, rep.Deduped, p, rep.ArchiveGCd,
		p, rep.LiveTokensBefore, rep.LiveTokensAfter, rep.Budget)
	return err
}

func reclaimSpace(ctx context.Context, a *app.App, w io.Writer) error {
	// Incremental writes leave FTS5 segments and delete markers; optimize merges
	// them, then VACUUM returns the freed pages to the OS.
	tables, err := a.Store.OptimizeFTS(ctx)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "fts optimized: %d index(es)\n", len(tables)); err != nil {
		return err
	}
	if _, err := a.Store.DB.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	// VACUUM is committed; if this checkpoint cannot finish now, a later one can truncate the WAL.
	_ = a.Store.Checkpoint(ctx)
	_, err = fmt.Fprintln(w, "vacuumed: freed pages returned to the OS")
	return err
}
