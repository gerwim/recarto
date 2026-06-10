package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/mattn/go-isatty"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Index reconciles the code graph for the current worktree and prints a summary.
// Progress is reported to stderr (an animated bar on a TTY, periodic plain lines
// otherwise) so the summary on stdout stays clean and machine-readable.
// --force bypasses the freshness token and re-runs the secondary-index rebuild
// plus a full resolve — the explicit heal lever for a store damaged outside
// Recarto's view (e.g. by an aborted run under an older binary).
func Index(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	wt := a.Git.Worktree
	prog := newIndexProgress(os.Stderr, isatty.IsTerminal(os.Stderr.Fd()))
	opts := []graph.ReconcileOption{graph.WithProgress(prog)}
	if slices.Contains(args, "--force") {
		opts = append(opts, graph.WithForce())
	}
	if err := a.Graph.ReconcileIfStale(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt),
		opts...); err != nil {
		return err
	}
	var files, nodes int
	a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&files)
	a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE file_id IN (SELECT id FROM files WHERE worktree=?)`, wt).Scan(&nodes)
	fmt.Fprintf(w, "indexed %d files, %d symbols (worktree %s)\n", files, nodes, wt)
	return nil
}
