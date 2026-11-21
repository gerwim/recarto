package agents

import (
	"context"
	"sync"
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

// startWait is a session start's one first-index deadline, created at its
// first use and shared by every tier and check of that start.
type startWait struct {
	parent context.Context
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
}

// get returns the deadline, starting it on the first call.
func (w *startWait) get() context.Context {
	w.once.Do(func() { w.ctx, w.cancel = firstIndexDeadline(w.parent) })
	return w.ctx
}

// close ends the deadline if it was started.
func (w *startWait) close() {
	w.once.Do(func() {})
	if w.cancel != nil {
		w.cancel()
	}
}

// codeCheck is graph.HasCode's answer.
type codeCheck struct {
	code bool
	err  error
}

// mapTier is what a full session-start block shows in the code map's place:
// the map, one line, or nothing. A nil tier shows nothing.
type mapTier struct {
	rc    *app.RepoContext
	start func(worktree string) (<-chan error, error) // nil: no first-index attempt
	arch  graph.Architecture
	st    graph.MapStatus
	line  string

	check  chan codeCheck  // nil when no check runs
	wait   context.Context // the deadline of the check and the first-index wait together
	joined bool
	got    codeCheck
}

// startMapTier reads rc's map and, unless a finished index decides from the
// stored files on disk, starts the check whether the worktree still holds code.
// With wait nil (a repo no longer served) nothing is checked, started or
// waited for: an unfinished map is decided at once. Nil when the map cannot be
// read.
func startMapTier(ctx, skip context.Context, wait *startWait, rc *app.RepoContext,
	start func(string) (<-chan error, error)) *mapTier {
	wt := rc.Git.Worktree
	arch, st, err := rc.Graph.CodeMap(skip, wt, rc.Git.NonGit)
	if err != nil {
		return nil
	}
	t := &mapTier{rc: rc, start: start, arch: arch, st: st}
	if wait == nil {
		if !st.Done {
			t.decide(ctx, skip)
		}
		return t
	}
	present, listed, err := rc.Graph.PresentFiles(ctx, wt, candidateFiles)
	if err != nil {
		// Without the stored files the check lists the whole worktree.
		present, listed = nil, true
	}
	// A stored code file still on disk keeps a finished map without a git process,
	// which would add about a millisecond; the background index drops a file git stopped listing.
	if st.Done && (!listed || len(present) > 0) {
		return t
	}
	dctx := wait.get()
	t.check, t.wait = make(chan codeCheck, 1), dctx
	go func() {
		defer recoverHook()
		code, err := graph.HasCode(dctx, wt, rc.Git.NonGit, present...)
		t.check <- codeCheck{code, err}
	}()
	return t
}

// mapTiers reads and settles one tier per repo side by side: each repo's map
// read, code check, first-index start and wait run in a goroutine of their
// own under the shared deadline, so no repo waits for another. Once every wait
// is over each tier that says an index runs is decided again: an already
// indexed repo never waits for its refresh, so a refresh that ended while
// other repos' first indexes were waited for shows its finished map.
func mapTiers(ctx, skip context.Context, wait *startWait, covered []coveredRepo,
	start func(string) (<-chan error, error)) []*mapTier {
	tiers := make([]*mapTier, len(covered))
	var wg sync.WaitGroup
	for i, c := range covered {
		w, s := wait, start
		if !c.served {
			w, s = nil, nil
		}
		wg.Go(func() {
			defer recoverHook()
			t := startMapTier(ctx, skip, w, c.RepoContext, s)
			t.settle(ctx, skip)
			tiers[i] = t
		})
	}
	wg.Wait()
	for _, t := range tiers {
		t.recheck(ctx, skip)
	}
	return tiers
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
func (t *mapTier) settle(ctx, skip context.Context) {
	if t == nil || t.check == nil || t.st.Done {
		return
	}
	if t.empty() {
		t.arch, t.line = graph.Architecture{}, ""
		return
	}
	if !t.st.Built {
		awaitFirstIndex(t.wait, t.start, t.rc.Graph, t.rc.Git.Worktree)
	}
	t.decide(ctx, skip)
}

// decide turns the index lock and the stored graph into the tier.
func (t *mapTier) decide(ctx, skip context.Context) {
	wt := t.rc.Git.Worktree
	// The block says an index runs only when it has seen one; a probe that fails
	// sees none.
	running, err := t.rc.Graph.IndexRunning(wt)
	if err != nil {
		running = false
	}
	// The probe comes before the read, so an index that ends between the two is
	// judged by its result.
	arch, st, err := t.rc.Graph.CodeMap(skip, wt, t.rc.Git.NonGit)
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
	if tierDecided != nil {
		tierDecided(t)
	}
}

// tierDecided, when set, runs after decide settles a tier: a test seam to act
// between a tier's decision and the end of the shared wait. Nil in production.
var tierDecided func(t *mapTier)

// mapped reports whether the tier's file and symbol counts are known: its
// map is finished, or its worktree holds nothing an index would take in.
func (t *mapTier) mapped() bool {
	return t != nil && (t.st.Done || t.check != nil && t.empty())
}

// recheck decides a tier that says an index is running again, and reports
// whether the tier changed. The block's conversation writes can wait for the
// writer of an index that then finishes.
func (t *mapTier) recheck(ctx, skip context.Context) bool {
	if t == nil || t.line != indexingLine {
		return false
	}
	t.decide(ctx, skip)
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
func awaitFirstIndex(ctx context.Context, start func(string) (<-chan error, error), g *graph.Engine, wt string) {
	if start != nil {
		// A process that cannot start leaves the wait to the probe below: another
		// process may still be indexing the worktree.
		if done, err := start(wt); err == nil {
			select {
			case err := <-done:
				// decide reads the outcome from the index lock and the stored graph.
				_ = err
			case <-ctx.Done():
				return
			}
		}
	}
	for {
		running, err := g.IndexRunning(wt)
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
