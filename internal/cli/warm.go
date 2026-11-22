package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gerwim/recarto/internal/agents"
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

// runWarm is the `recarto warm [--only=<wt> | --skip=<wt>… --if-code=<wt>… --session=<id>]` command.
func runWarm(ctx context.Context, a *app.App, _ io.Writer, args []string) error {
	fs := flag.NewFlagSet("warm", flag.ContinueOnError)
	only := fs.String("only", "", "index only this served worktree, without the sweep")
	session := fs.String("session", "", "record this conversation's missing sessions in the repos it serves")
	end := fs.String("end-session", "", "write the session ends a Stop of this conversation left pending, and nothing else")
	var skip, ifCode []string
	fs.Func("skip", "leave this worktree out (repeatable)", func(s string) error {
		skip = append(skip, s)
		return nil
	})
	fs.Func("if-code", "index this worktree only while it holds files an index would take in (repeatable)", func(s string) error {
		ifCode = append(ifCode, s)
		return nil
	})
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	if *end != "" {
		combined := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "only" || f.Name == "skip" || f.Name == "if-code" || f.Name == "session" {
				combined = true
			}
		})
		if combined {
			return errors.New("warm: --end-session cannot be combined with --only, --skip, --if-code or --session")
		}
		ctx, cancel := context.WithTimeout(ctx, warmLimit+endRunGrace)
		defer cancel()
		return agents.EndRosterSessions(ctx, a, *end)
	}
	if *only != "" && (len(skip) > 0 || len(ifCode) > 0 || *session != "") {
		return errors.New("warm: --only cannot be combined with --skip, --if-code or --session")
	}
	if *only != "" {
		return warmOnly(ctx, a, *only)
	}
	for _, wt := range ifCode {
		for _, rc := range a.AllRepos() {
			if rc.Git.Worktree != wt {
				continue
			}
			// A listing that fails proves nothing, so the worktree is then indexed.
			if code, err := graph.HasCode(ctx, wt, rc.Git.NonGit); err == nil && !code {
				skip = append(skip, wt)
			}
		}
	}
	// The sessions are written beside the index: a busy repo is tried again until
	// its row is written, which must not hold up the index.
	var sessionErr error
	var wg sync.WaitGroup
	if *session != "" {
		wg.Go(func() { sessionErr = agents.RecordRosterSessions(ctx, a, *session) })
	}
	err := Warm(ctx, a, skip)
	wg.Wait()
	return errors.Join(err, sessionErr)
}

// warmLimit bounds one `recarto warm` run.
const warmLimit = 30 * time.Minute

// armWarmLimit calls f once d has passed, unless the returned stop runs
// first. A var so a test can fire the limit at a moment it picks.
var armWarmLimit = func(d time.Duration, f func()) (stop func() bool) { return time.AfterFunc(d, f).Stop }

// endWarm ends a warm run that reached warmLimit. A git call or file read that
// hangs ignores any context, so only ending the process bounds the run. The
// store and the index lock survive this the same way they survive a crash: what
// is committed stays, and the next run continues from it.
func endWarm(d time.Duration) {
	fmt.Fprintf(os.Stderr, "recarto warm: stopped after %v; the next session start continues its work\n", d)
	os.Exit(1)
}

// warmRun is what a session start hands its background warm: the worktrees it
// already made its attempt at, those to index only while they hold code, and
// the conversation whose missing repo sessions it records.
type warmRun struct {
	skip, ifCode []string
	session      string
	end          string
}

func warmArgs(w warmRun) []string {
	if w.end != "" {
		return []string{"warm", "--end-session=" + w.end}
	}
	args := []string{"warm"}
	for _, wt := range w.skip {
		args = append(args, "--skip="+wt)
	}
	for _, wt := range w.ifCode {
		args = append(args, "--if-code="+wt)
	}
	if w.session != "" {
		args = append(args, "--session="+w.session)
	}
	return args
}

// endRunGrace lets an end run retry a writer released just before warmLimit.
const endRunGrace = 5 * time.Second

func warmRunLimit(args []string) time.Duration {
	for _, arg := range args {
		if arg == "--end-session" || strings.HasPrefix(arg, "--end-session=") {
			return warmLimit + 2*endRunGrace
		}
	}
	return warmLimit
}

// startWarm starts `recarto warm` for w in a detached process. A var so tests
// can keep a hook from starting the test binary.
var startWarm = func(w warmRun) error {
	return detach.Start(binaryPath(), warmArgs(w)...)
}

// startIndex starts `recarto warm --only=<worktree>` in a detached process and
// returns a channel that gets its exit. A var so tests can keep a hook from
// starting the test binary.
var startIndex = func(worktree string) (<-chan error, error) {
	return detach.StartWatched(binaryPath(), "warm", "--only="+worktree)
}
