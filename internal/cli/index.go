package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Index reconciles the code graph for the current worktree and prints a summary.
func Index(ctx context.Context, a *app.App, w io.Writer, _ []string) error {
	wt := a.Git.Worktree
	if err := a.Graph.ReconcileIfStale(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt)); err != nil {
		return err
	}
	var files, nodes int
	a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&files)
	a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE file_id IN (SELECT id FROM files WHERE worktree=?)`, wt).Scan(&nodes)
	fmt.Fprintf(w, "indexed %d files, %d symbols (worktree %s)\n", files, nodes, wt)
	return nil
}
