package agents

import (
	"context"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

const (
	// firstIndexWait bounds how long a session start looks for code and waits
	// for the worktree's first index, together, so a small repo's first session
	// gets its code map.
	firstIndexWait = 2 * time.Second
	// indexPollInterval is how often a session start waiting for another
	// process's index checks whether it still runs.
	indexPollInterval = 50 * time.Millisecond
	// candidateFiles is how many stored files are checked first for whether the
	// worktree still holds code, sparing a listing of the whole worktree.
	candidateFiles = 8
	// writeGrace is how long a session start's skippable writes wait, before and
	// again after the first-index wait, for a writer another process holds.
	writeGrace = 300 * time.Millisecond
)

// firstIndexDeadline is the one deadline of the check for code and the
// first-index wait. A var so a test can end the wait at a moment it picks.
var firstIndexDeadline = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, firstIndexWait)
}

// codeCheck is graph.HasCode's answer.
type codeCheck struct {
	code bool
	err  error
}

// mapTier is what a full session-start block shows in the code map's place:
// the map, one line, or nothing. A nil tier shows nothing.
type mapTier struct {
	wt     string
	nonGit bool
	arch   graph.Architecture
	st     graph.MapStatus
	line   string

	check  chan codeCheck  // nil when no check runs
	wait   context.Context // the deadline of the check and the first-index wait together
	cancel context.CancelFunc
	joined bool
	got    codeCheck
}

// startMapTier reads the worktree's map and starts the check whether it still
// holds code, which runs while the session start does its upkeep. Nil when the
// map cannot be read.
func startMapTier(ctx, skip context.Context, a *app.App) *mapTier {
	wt := a.Git.Worktree
	arch, st, err := a.Graph.CodeMap(skip, wt, a.Git.NonGit)
	if err != nil {
		return nil
	}
	t := &mapTier{wt: wt, nonGit: a.Git.NonGit, arch: arch, st: st}
	present, listed, err := a.Graph.PresentFiles(ctx, wt, candidateFiles)
	if err != nil {
		// Without the stored files the check lists the whole worktree.
		present, listed = nil, true
	}
	if st.Done && !listed {
		return t // a finished index that found nothing to index
	}
	dctx, cancel := firstIndexDeadline(ctx)
	t.check, t.wait, t.cancel = make(chan codeCheck, 1), dctx, cancel
	go func() {
		defer recoverHook()
		code, err := graph.HasCode(dctx, wt, a.Git.NonGit, present...)
		t.check <- codeCheck{code, err}
	}()
	return t
}

// close ends the check if it still runs.
func (t *mapTier) close() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

// join waits for the check's answer.
func (t *mapTier) join() codeCheck {
	if !t.joined {
		t.got, t.joined = <-t.check, true
	}
	return t.got
}

// empty reports whether the check found the worktree holds no code. A listing
// that failed proves nothing, so the worktree then counts as holding code.
func (t *mapTier) empty() bool {
	c := t.join()
	return c.err == nil && !c.code
}

// settle decides an unfinished index's tier, waiting for a worktree's first
// index within the check's deadline. A finished index's tier is decided in result.
func (t *mapTier) settle(ctx, skip context.Context, a *app.App) {
	if t == nil || t.check == nil || t.st.Done {
		return
	}
	if t.empty() {
		t.arch, t.line = graph.Architecture{}, ""
		return
	}
	if !t.st.Built {
		awaitFirstIndex(t.wait, a, t.wt)
	}
	t.decide(ctx, skip, a)
}

// decide turns the index lock and the stored graph into the tier.
func (t *mapTier) decide(ctx, skip context.Context, a *app.App) {
	// The block says an index runs only when it has seen one; a probe that fails
	// sees none.
	running, err := a.Graph.IndexRunning(t.wt)
	if err != nil {
		running = false
	}
	// The probe comes before the read, so an index that ends between the two is
	// judged by its result.
	arch, st, err := a.Graph.CodeMap(skip, t.wt, t.nonGit)
	if err != nil {
		t.arch, t.line = graph.Architecture{}, ""
		return
	}
	t.arch, t.st, t.line = graph.Architecture{}, st, ""
	switch {
	case st.Done:
		t.arch = arch
	case running:
		t.line = indexingLine
	case !st.Built:
		t.line = unbuiltLine
	}
}

// recheck decides a tier that says an index is running again, and reports
// whether the tier changed. The block's conversation writes can wait for the
// writer of an index that then finishes.
func (t *mapTier) recheck(ctx, skip context.Context, a *app.App) bool {
	if t == nil || t.line != indexingLine {
		return false
	}
	t.decide(ctx, skip, a)
	return t.line != indexingLine
}

// result returns the map and the line the block shows.
func (t *mapTier) result() (graph.Architecture, string) {
	if t == nil {
		return graph.Architecture{}, ""
	}
	// A finished map stays when the check failed or ran out of time: the
	// worktree then counts as holding code.
	if t.st.Done && t.check != nil && t.empty() {
		return graph.Architecture{}, ""
	}
	return t.arch, t.line
}

// awaitFirstIndex starts the worktree's first index in another process, or
// joins the one already running, and waits until no index of it runs or ctx
// ends.
func awaitFirstIndex(ctx context.Context, a *app.App, wt string) {
	if a.StartIndex != nil {
		// A process that cannot start leaves the wait to the probe below: another
		// process may still be indexing the worktree.
		if done, err := a.StartIndex(wt); err == nil {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
	}
	for {
		running, err := a.Graph.IndexRunning(wt)
		// A failed probe ends the wait; decide probes again.
		if err != nil || !running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(indexPollInterval):
		}
	}
}
