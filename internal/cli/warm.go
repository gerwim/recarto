package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/detach"
	"github.com/gerwim/recarto/internal/graph"
)

// Warm brings the code graph of every served repo up to date, caches its code
// map, and drops the graph of dead and unused worktrees. Session start runs it
// in a detached process, so the hook never waits for an index. It leaves out
// the worktrees in skip, which the session start already made its attempt at,
// and a worktree that another process is indexing.
func Warm(ctx context.Context, a *app.App, skip []string) error {
	var errs []error
	for _, rc := range a.AllRepos() {
		if slices.Contains(skip, rc.Git.Worktree) {
			continue
		}
		if err := warmRepo(ctx, rc); err != nil {
			errs = append(errs, err)
		}
	}
	p, err := a.PrunableWorktrees(ctx)
	if err == nil {
		_, err = a.PruneWorktrees(ctx, p)
	}
	return errors.Join(append(errs, err, p.DaysErr)...)
}

// warmRepo brings rc's code graph up to date and caches its code map. A
// worktree that another process is indexing is left to that process.
func warmRepo(ctx context.Context, rc *app.RepoContext) error {
	wt := rc.Git.Worktree
	// The token is read under the index lock: a git call that hangs there keeps
	// the lock, so later runs leave this worktree instead of hanging in it too.
	err := rc.Graph.ReconcileIfIdle(ctx, wt, graph.NewSource(wt),
		func(attempt string) string { return graph.WorktreeToken(wt, attempt) })
	if errors.Is(err, graph.ErrIndexing) {
		// The process indexing this worktree finishes it, and the next
		// session start caches its map.
		return nil
	}
	if err != nil {
		return err
	}
	// Building the map here spares the next session start from building it.
	_, err = rc.Graph.Architecture(ctx, wt, "")
	return err
}

// warmOnly indexes the served worktree wt alone, as a session start's first
// index of it, without the sweep.
func warmOnly(ctx context.Context, a *app.App, wt string) error {
	for _, rc := range a.AllRepos() {
		if rc.Git.Worktree == wt {
			return warmRepo(ctx, rc)
		}
	}
	return fmt.Errorf("warm: %s is not a served worktree", wt)
}

// runWarm is the `recarto warm [--only=<wt> | --skip=<wt>…]` command.
func runWarm(ctx context.Context, a *app.App, _ io.Writer, args []string) error {
	fs := flag.NewFlagSet("warm", flag.ContinueOnError)
	only := fs.String("only", "", "index only this served worktree, without the sweep")
	var skip []string
	fs.Func("skip", "leave this worktree out (repeatable)", func(s string) error {
		skip = append(skip, s)
		return nil
	})
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	if *only != "" && len(skip) > 0 {
		return errors.New("warm: --only and --skip cannot be combined")
	}
	if *only != "" {
		return warmOnly(ctx, a, *only)
	}
	return Warm(ctx, a, skip)
}

// warmLimit bounds one `recarto warm` run.
const warmLimit = 30 * time.Minute

// armWarmLimit calls f once warmLimit has passed, unless the returned stop runs
// first. A var so a test can fire the limit at a moment it picks.
var armWarmLimit = func(f func()) (stop func() bool) { return time.AfterFunc(warmLimit, f).Stop }

// endWarm ends a warm run that reached warmLimit. A git call or file read that
// hangs ignores any context, so only ending the process bounds the run. The
// store and the index lock survive this the same way they survive a crash: what
// is committed stays, and the next run continues from it.
func endWarm() {
	fmt.Fprintf(os.Stderr, "recarto warm: stopped after %v; the next session start continues the index\n", warmLimit)
	os.Exit(1)
}

// startWarm starts `recarto warm` in a detached process, leaving out the
// worktrees in skip. A var so tests can keep a hook from starting the test
// binary.
var startWarm = func(skip []string) error {
	args := []string{"warm"}
	for _, wt := range skip {
		args = append(args, "--skip="+wt)
	}
	return detach.Start(binaryPath(), args...)
}

// startIndex starts `recarto warm --only=<worktree>` in a detached process and
// returns a channel that gets its exit. A var so tests can keep a hook from
// starting the test binary.
var startIndex = func(worktree string) (<-chan error, error) {
	return detach.StartWatched(binaryPath(), "warm", "--only="+worktree)
}
