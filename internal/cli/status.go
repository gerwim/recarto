package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Status prints repo identity, per-kind active memory counts, and lifetime
// ledger counters.
func Status(ctx context.Context, a *app.App, w io.Writer) error {
	fmt.Fprintf(w, "repo:     %s (%s)\n", a.Git.RepoName, a.Git.RepoID)
	fmt.Fprintf(w, "branch:   %s\n", a.Git.Branch)
	fmt.Fprintf(w, "worktree: %s\n", a.Git.Worktree)

	rows, err := a.Store.DB.QueryContext(ctx,
		`SELECT kind, count(*) FROM memories WHERE status='active' GROUP BY kind ORDER BY kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Fprintln(w, "memories:")
	total := 0
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-14s %d\n", kind, n)
		total += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-14s %d\n", "total", total)

	fmt.Fprintln(w, "ledger (lifetime):")
	for _, m := range []string{
		"memories_recorded", "memories_recalled", "recall_tokens_injected",
		"recall_misses", "reads", "repeated_reads_blocked",
		"repeated_augments_blocked", "large_reads_nudged",
	} {
		v, err := a.Ledger.Get(ctx, "lifetime", m)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-24s %d\n", m, v)
	}

	// Compaction / decay state.
	epoch, err := a.Engine.Epoch(ctx)
	if err != nil {
		return err
	}
	live, err := a.Engine.LiveTokens(ctx)
	if err != nil {
		return err
	}
	var archived int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE status='archived'`).Scan(&archived); err != nil {
		return err
	}
	fmt.Fprintln(w, "compaction:")
	fmt.Fprintf(w, "  %-14s %d\n", "epoch", epoch)
	fmt.Fprintf(w, "  %-14s %d\n", "archived", archived)
	fmt.Fprintf(w, "  %-14s %d / %d\n", "live tokens", live, memory.DefaultBudgetTokens)

	fmt.Fprintln(w, "ledger (compaction):")
	for _, m := range []string{"compactions", "memories_archived", "memories_deduped", "memories_gc"} {
		v, err := a.Ledger.Get(ctx, "lifetime", m)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-24s %d\n", m, v)
	}

	fmt.Fprintln(w, "sync:")
	if a.Sync == nil {
		fmt.Fprintln(w, "  disabled (set RECARTO_SYNC_URL/RECARTO_SYNC_KEY; repo needs a root commit or RECARTO_SYNC_REPO)")
		return nil
	}
	var cursor string
	if err := a.Store.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='sync_cursor'`).Scan(&cursor); err != nil {
		cursor = "0"
	}
	var pending int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_sync_outbox`).Scan(&pending); err != nil {
		return err
	}
	var pendingTombs int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_tombstones WHERE pushed=0`).Scan(&pendingTombs); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-24s %s\n", "repo key", a.Sync.RepoKeyValue())
	fmt.Fprintf(w, "  %-24s %s\n", "cursor", cursor)
	fmt.Fprintf(w, "  %-24s %d\n", "pending push", pending)
	fmt.Fprintf(w, "  %-24s %d\n", "pending tombstones", pendingTombs)
	return nil
}
