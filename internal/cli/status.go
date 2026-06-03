package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
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
	} {
		v, err := a.Ledger.Get(ctx, "lifetime", m)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-24s %d\n", m, v)
	}
	return nil
}
