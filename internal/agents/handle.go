package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/snapshot"
	"github.com/gerwim/recarto/internal/store"
)

// Handle runs the agent-neutral logic for one lifecycle event and returns the
// neutral result. It is exit-0 advisory: callers treat any returned error as
// non-blocking. RunHook always encodes the (possibly partial) Result first.
func Handle(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if ev.SessionID != "" {
		ca, done, err := conversationApp(ctx, a, ev.SessionID)
		if err != nil {
			return Result{}, err
		}
		defer done()
		a = ca
	}
	// Every hook that reaches the store counts as use of its worktree; NoteUsed
	// writes only an hour-old mark, so a hook that otherwise reads takes no write
	// lock. A lost mark only lets the sweep drop this graph sooner; the next use
	// re-indexes it. So a session start does not wait for it behind another
	// process's writer, such as an index finishing.
	usedCtx := ctx
	var roster []rosterRepo
	var rosterErr error
	switch ev.Kind {
	case KindSessionStart:
		usedCtx = store.WithWriteDeadline(ctx, time.Now().Add(writeGrace))
	case KindStop:
		usedCtx, roster, rosterErr = stopWrites(ctx, a, ev, a.Clock.Now().Add(stopLockBudget))
	}
	_ = a.Graph.NoteUsed(usedCtx, a.Git.Worktree)
	switch ev.Kind {
	case KindSessionStart:
		return handleSessionStart(ctx, a, ev)
	case KindPostRead:
		return handlePostRead(ctx, a, ev)
	case KindPostEdit:
		return handlePostEdit(ctx, a, ev)
	case KindAugment:
		return handleAugment(ctx, a, ev)
	case KindStop:
		return handleStop(ctx, usedCtx, a, ev, roster, rosterErr)
	case KindShell:
		return handleShell(ctx, a, ev)
	}
	return Result{}, nil
}

// syncPullBudget / syncPushBudget bound the best-effort HTTP memory-sync pull
// (session-start, before recall) and push (Stop). Sync payloads are small
// text-only memory rows (never the code graph), so these are short; a wedged
// or unreachable sync server must never delay the agent. Vars so tests can
// shrink them.
var syncPullBudget = 5 * time.Second
var syncPushBudget = 5 * time.Second

// stopLockBudget bounds how long a Stop of a conversation started in a folder
// without code waits, in total over all its stores, for writers other processes
// hold. Its sync pushes are outside it and keep syncPushBudget each.
const stopLockBudget = 500 * time.Millisecond

// stopWrites reads the roster of ev's conversation and returns the context of
// its Stop's writes. With a roster, the write deadline is the same instant
// across all stores. Without one, or when the roster cannot be read, writes
// wait as every write does.
func stopWrites(ctx context.Context, a *app.App, ev Event, deadline time.Time) (writes context.Context, roster []rosterRepo, err error) {
	if ev.SessionID == "" {
		return ctx, nil, nil
	}
	roster, err = readRoster(ctx, a.Ledger, ev.SessionID)
	if err != nil || len(roster) == 0 {
		return ctx, nil, err
	}
	return store.WithWriteDeadline(ctx, deadline), roster, nil
}

// boundedSync runs fn (a Syncer.Pull or Syncer.Push call) with a hard
// deadline: abandoning a slow or unreachable server is safe because sync is
// best-effort and additive — nothing local depends on it completing.
func boundedSync(ctx context.Context, budget time.Duration, fn func(context.Context) error) {
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverHook()
		_ = fn(sctx)
	}()
	select {
	case <-done:
	case <-sctx.Done():
	}
}

// recoverHook swallows a panic in a hook's background goroutine: an unrecovered
// one kills the process with exit 2, which Claude Code reads as "block". Like
// any other best-effort failure, it just ends that goroutine's work.
func recoverHook() { _ = recover() }

// agentOrDefault stamps an unknown/empty agent as the legacy Claude Code value so
// the sessions.agent column always carries a host name.
func agentOrDefault(s string) string {
	if s == "" {
		return "claude-code"
	}
	return s
}

// owningRepo maps an absolute file path to the served repo among repos containing it —
// the primary, or a RECARTO_REPOS extra — returning that repo's context and
// the repo-relative path. Longest worktree prefix wins (a nested checkout
// belongs to the inner repo). ok=false when no served worktree contains the
// file, or the path is not absolute.
func owningRepo(repos []*app.RepoContext, abs string) (rc *app.RepoContext, rel string, ok bool) {
	wts := make([]string, len(repos))
	for i, cand := range repos {
		wts[i] = cand.Git.Worktree
	}
	i := app.Innermost(wts, abs)
	if i < 0 {
		return nil, "", false
	}
	rel, _ = app.Within(wts[i], abs)
	return repos[i], rel, true
}

// Owned reports whether an absolute path lies in one of the worktrees — the
// store-free form of owningRepo, against app.Worktrees.
func Owned(worktrees []string, abs string) bool {
	for _, wt := range worktrees {
		if _, in := app.Within(wt, abs); in {
			return true
		}
	}
	return false
}

// Reaches reports whether a search over abs reaches one of the worktrees: abs
// lies in one or contains one. abs is where the search leads, as
// Event.SearchPaths holds it; the worktrees are resolved here. It is the
// store-free form of searchRepos, against app.Worktrees: true exactly when
// searchRepos finds a repo.
func Reaches(worktrees []string, abs string) bool {
	for _, wt := range worktrees {
		wt = realPath(wt)
		_, in := app.Within(wt, abs)
		_, contains := app.Within(abs, wt)
		if in || contains {
			return true
		}
	}
	return false
}

// maxLinks bounds the dangling links realPath follows, so a link loop ends. It
// matches Linux's limit on the links one lookup follows.
const maxLinks = 40

// realPath returns where a search over abs leads: abs with its symlinks
// resolved in path order, so "link/.." is the parent of the link's target.
// Search scope compares paths and worktrees in this form, because git names a
// worktree by its real path while the host names searched paths, and a non-git
// worktree, as it reached them. Past the last part that exists the path stays
// as named, except that a dangling link there still leads to its target.
func realPath(abs string) string {
	links := maxLinks
	var resolve func(string) string
	resolve = func(abs string) string {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r
		}
		// The parent is resolved on its own, so the parts that exist still count
		// where they lead.
		root := len(filepath.VolumeName(abs)) + 1
		i := strings.LastIndexAny(abs, "/"+string(filepath.Separator))
		if len(abs) <= root || i < 0 {
			return abs
		}
		dir := resolve(abs[:max(i, root)])
		p := filepath.Join(dir, abs[i+1:])
		// A dangling link still leads to its target; anything else that does not
		// resolve (missing, unreadable, a loop past maxLinks) stays as named.
		if t, err := os.Readlink(p); err == nil && links > 0 {
			links--
			if !filepath.IsAbs(t) {
				t = dir + string(filepath.Separator) + t
			}
			return resolve(t)
		}
		return p
	}
	return resolve(abs)
}

// Metric prefixes of what a conversation has in its context. A compaction
// forgets them; the one-time nudges and blockMetric stay.
const (
	readMetric   = "lines:"
	shownMetric  = "surfaced:"
	searchMetric = "augment:"
)

// blockMetric marks a conversation that got the full session-start block.
const blockMetric = "session_block"

// toolFeatures returns a's features as a hook applies them to its pointers at
// MCP tools. While no Recarto MCP server runs for the repo (app.ServerRunning)
// the agent cannot call those tools, so the features that register them count
// as off: a hook keeps its facts and leaves out what points at a tool.
func toolFeatures(a *app.App) features.Set {
	if app.ServerRunning(a.Git.RepoID) {
		return a.Features
	}
	return withoutTools(a.Features)
}

// withoutTools returns f with the features that point at MCP tools off.
func withoutTools(f features.Set) features.Set {
	out := features.Set{}
	maps.Copy(out, f)
	out[features.MemoryTools] = false
	out[features.GraphTools] = false
	out[features.IssueReports] = false
	return out
}

// conversation returns the ledger scope of what ev's conversation was shown:
// the session's main agent, or the subagent ev names. "" without a session.
func conversation(ev Event) string {
	if ev.SessionID == "" {
		return ""
	}
	if ev.AgentID == "" {
		return "session:" + ev.SessionID
	}
	return "session:" + ev.SessionID + "/agent:" + ev.AgentID
}

// handleSessionStart opens/creates the session row, runs gated compaction,
// recalls a token-budgeted slice for this branch, and returns it as injectable
// context plus a record nudge. A resume of a conversation that
// already got the block returns only the memories it has not been shown yet.
// A full block waits up to firstIndexWait for the worktree's first index, and
// ranks its memories against the graph as it stands when the wait ends.
//
// Writes that can be skipped (statistics, sync, snapshot import, compaction,
// recall stamps, the map cache) run under skip: while another process holds
// the store's writer, such as an index finishing, they wait at most writeGrace
// and are skipped, to be redone by the next session start or Stop. Conversation
// state (a new conversation's row, a clear or compact reset, the block mark
// with the journal marks the block read, the shown marks) waits for the writer.
func handleSessionStart(ctx context.Context, a *app.App, ev Event) (res Result, err error) {
	now := a.Clock.Now().UnixMilli()
	skip := store.WithWriteDeadline(ctx, time.Now().Add(writeGrace))
	// Before the blocks, so an end it writes no longer holds back this start's pruning.
	repoEndErr := endPendingRepoEnds(ctx, a)
	wait := &startWait{parent: ctx}
	defer wait.close()
	conv, err := startConversation(ctx, a, ev, now, wait)
	if err != nil {
		return Result{}, errors.Join(err, repoEndErr)
	}
	var ends sync.WaitGroup
	var endErr error
	ends.Go(func() {
		defer recoverInto(&endErr, "write pending session ends")
		endErr = endPendingFolderEnds(ctx, a, a.Git.NonGit || conv.roster != nil)
	})
	defer func() {
		ends.Wait()
		endErr = errors.Join(repoEndErr, endErr)
		if err != nil {
			err = errors.Join(err, endErr)
		} else {
			res.Failure = errors.Join(res.Failure, endErr)
		}
	}()
	_ = a.Ledger.Incr(skip, "lifetime", "sessions", 1)
	scope := conversation(ev)
	// Forget before the upkeep, so a hook the host kills mid-upkeep still leaves a
	// compacted conversation's record cleared.
	resumed, err := applySource(ctx, a, scope, ev.Source)
	if err != nil {
		return Result{}, err
	}
	if conv.roster == nil {
		var names []string
		for _, rc := range a.Repos {
			names = append(names, rc.Git.RepoName)
		}
		return repoBlock(ctx, skip, a, now, scope, resumed, wait, subject{
			rc: a.AllRepos()[0], served: true, tools: true, header: memoryHeader, reposLine: alsoServing(names),
			marks: blockMarks{session: ev.SessionID}})
	}
	// A later start of a known conversation resumes it, whether or not its first
	// block was delivered.
	resumed = ev.Source == "resume" && !conv.first
	if a.IndexIfCode != nil {
		// The background index checks the folder as it then is, so no hook lists it.
		a.IndexIfCode(a.Git.Worktree)
	}
	covered, gone, done := cover(a, conv.roster)
	defer done()
	// The block does not need the repos' session rows (it counts this session's
	// changes as if they were written), so they are written beside the wait.
	var bg sync.WaitGroup
	var rowErr error
	if ev.SessionID != "" {
		bg.Go(func() {
			defer recoverHook()
			rowErr = ensureRosterSessions(skip, covered, ev, conv)
		})
	}
	switch len(covered) {
	case 0:
		res, err = goneBlock(ctx, skip, a, scope, resumed, gone)
	case 1:
		res, err = repoBlock(ctx, skip, a, now, scope, resumed, wait, covered[0].subject(ev.SessionID))
	default:
		res, err = sharedBlock(ctx, skip, a, now, scope, resumed, wait, ev.SessionID, covered)
	}
	bg.Wait()
	if err != nil {
		return Result{}, errors.Join(err, rowErr, conv.markErr)
	}
	// A session row that could not be written does not spoil the block: the
	// block is delivered and the failure reported.
	res.Failure = errors.Join(rowErr, conv.markErr)
	return res, nil
}

// subject is the repo a one-repo block is about.
type subject struct {
	rc        *app.RepoContext
	served    bool       // RECARTO_REPOS serves it: it gets the first-index wait
	tools     bool       // the repo argument reaches it: the block points at tools
	header    string     // the memory tier's header
	reposLine string     // the line naming served repos, "" for none
	marks     blockMarks // where the block records the journal mark it read
}

// blockMarks is where a full block records the journal marks it read:
// session's row in the block's own worktree, or, with roster, session's
// roster marks in the folder's ledger. An empty session records none.
type blockMarks struct {
	session string
	roster  bool
}

// repoRead is the journal mark a full block read one repo's range at.
type repoRead struct {
	wt   string
	mark int64
}

// recordBlock records, in one write on a's store, that scope's conversation
// got its full block and the journal marks the block read, so a mark is only
// raised for a block that read its range.
func recordBlock(ctx context.Context, a *app.App, scope string, to blockMarks, reads []repoRead) error {
	if scope == "" && (to.session == "" || len(reads) == 0) {
		return nil
	}
	return a.Store.WriteTx(ctx, func(c store.Conn) error {
		if scope != "" {
			if err := ledger.IncrIn(ctx, c, scope, blockMetric, 1); err != nil {
				return err
			}
		}
		if to.session == "" {
			return nil
		}
		if to.roster {
			roster := make([]rosterRepo, len(reads))
			for i, r := range reads {
				roster[i] = rosterRepo{wt: r.wt, blockMark: sql.NullInt64{Int64: r.mark, Valid: true}}
			}
			return putRosterMarks(ctx, c, to.session, roster)
		}
		for _, r := range reads {
			if err := raiseMark(ctx, c, to.session, r.mark); err != nil {
				return err
			}
		}
		return nil
	})
}

// repoBlock builds the session-start block of one repo, s: the block a session
// started in that repo shows.
func repoBlock(ctx, skip context.Context, a *app.App, now int64, scope string, resumed bool,
	wait *startWait, s subject) (Result, error) {
	rc := s.rc
	var tier *mapTier
	if !resumed && a.Features.On(features.CodeMap) {
		w, start := wait, a.StartIndex
		if !s.served {
			w, start = nil, nil
		}
		tier = startMapTier(ctx, skip, w, rc, start)
	}
	// The first-index wait comes before upkeep, so a slow sync does not use up its budget.
	tier.settle(ctx, skip)
	// The wait used up the first bound; the writes after it get their own.
	skip = store.WithWriteDeadline(ctx, time.Now().Add(writeGrace))
	// Writes that miss skip's bound are skipped; the next session start or Stop
	// redoes them.
	sessionStartUpkeep(skip, a, []*app.RepoContext{rc})

	budget := sessionBudget()
	repoBudget := budget
	// The workspace tier borrows a third of the session budget rather than
	// growing the block: RECARTO_SESSION_BUDGET stays the whole block's bound.
	var results, wsResults []memory.RecallResult
	if a.Features.On(features.SessionMemories) {
		if a.Workspace != nil {
			wsBudget := budget / 3
			repoBudget = budget - wsBudget
			if wsSet, err := a.Workspace.Engine.Recall(skip, memory.RecallQuery{
				Baseline:    true,
				TokenBudget: wsBudget,
			}); err == nil {
				wsResults = wsSet.Results
			}
		}
		set, err := rc.Engine.Recall(skip, memory.RecallQuery{
			Branch:      rc.Git.Branch,
			Worktree:    rc.Git.Worktree,
			Baseline:    true,
			TokenBudget: repoBudget,
		})
		if err != nil {
			return Result{}, err
		}
		results = set.Results
	}
	if !a.Features.On(features.Todos) {
		isTodo := func(r memory.RecallResult) bool { return r.Record.Kind == memory.KindTodo }
		results, wsResults = slices.DeleteFunc(results, isTodo), slices.DeleteFunc(wsResults, isTodo)
	}
	r, read := recentChanges(ctx, skip, a, rc, now)
	deltas, deltaTotal, deltaBaseline := r.Deltas, r.Total, r.Since
	var reads []repoRead
	if read && r.Base != s.marks.session {
		reads = []repoRead{{wt: rc.Git.Worktree, mark: r.Read}}
	}

	wsName := ""
	if a.Workspace != nil {
		wsName = a.Workspace.Name
	}
	if resumed {
		return resumeContext(ctx, a, scope, results, s.header, wsName, wsResults, repoBudget)
	}
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	var bugs []*memory.Record
	if a.Features.On(features.RecentBugs) {
		bugs, _ = rc.Engine.RecentBugs(ctx, rc.Git.Branch, recentBugLimit)
	}

	f := toolFeatures(a)
	if !s.tools {
		f = withoutTools(f)
	}
	arch, line := tier.result()
	out, emitted := formatSessionContext(s.header, s.reposLine, results, wsName, wsResults, arch, line, bugs, deltas, deltaTotal, deltaBaseline, repoBudget, f)
	// Mark every memory rendered into this block as surfaced for the conversation, so the
	// just-in-time read/edit hooks won't re-inject a guardrail/bug/todo the agent can
	// already see here. Memories dropped under budget stay unmarked and still surface
	// when their code is touched.
	markSurfaced(ctx, a, scope, emitted)
	// A lost write only costs the next resume a full block, and the next start
	// may list a change of this block again; it never hides one.
	_ = recordBlock(ctx, a, scope, s.marks, reads)
	// An index can finish while the marks wait for its writer, and the block must
	// not then say the graph is still being indexed. The tier takes no part of
	// the memory budget, so the marks just written match the block rendered again.
	if tier.recheck(ctx, skip) {
		arch, line = tier.result()
		out, _ = formatSessionContext(s.header, s.reposLine, results, wsName, wsResults, arch, line, bugs, deltas, deltaTotal, deltaBaseline, repoBudget, f)
	}
	return Result{Context: out}, nil
}

// recentChanges reads the range a block built now in rc reports, counting the
// sessions pendingIn adds, after pruning rc's journal past what a baseline
// walk over those sessions can reach. read is false when the range could not
// be read. With the recent-changes feature off the range carries no changes,
// but it is still read: the block has had it all the same.
func recentChanges(ctx, skip context.Context, a *app.App, rc *app.RepoContext, now int64) (r graph.ChangeRange, read bool) {
	// Pruned before the read, so every row the block reports stays for
	// get_graph_delta in this session. A prune that fails or is skipped only keeps
	// rows longer; a later start prunes them. Recent changes are a best-effort
	// tier like the map and bugs: a journal or pending-ends folder that cannot be
	// read leaves the tier out of the block.
	pending, err := pendingIn(ctx, a, rc)
	if err != nil {
		return graph.ChangeRange{}, false
	}
	_ = rc.Graph.PruneUnreachableDeltas(skip, rc.Git.Worktree, pending, now, endsHoldFor(ctx, rc))
	r, ok, err := rc.Graph.RecentRange(ctx, rc.Git.Worktree, pending, now, 200)
	if err != nil || !ok {
		return graph.ChangeRange{}, false
	}
	if !a.Features.On(features.RecentChanges) {
		r.Deltas, r.Total = nil, 0
	}
	return r, true
}

// pendingIn returns the sessions a range in rc counts beside its rows: those
// whose ends wait in rc's pending ends, and, when a runs in a folder outside
// git, the folder's conversations whose roster covers rc. A session in both
// is one entry: its pending end's start and end, the higher of the two marks
// for a missing or open row, and the roster mark for an ended one. Every
// reader of rc in one folder gets the same list.
func pendingIn(ctx context.Context, a *app.App, rc *app.RepoContext) ([]graph.Session, error) {
	pending, err := pendingSessions(rc)
	if err != nil {
		return nil, err
	}
	folder, err := folderSessions(ctx, a, rc)
	if err != nil {
		return nil, err
	}
	for _, f := range folder {
		i := slices.IndexFunc(pending, func(s graph.Session) bool { return s.ID == f.ID })
		if i < 0 {
			pending = append(pending, f)
			continue
		}
		if f.Mark.Valid && (!pending[i].Mark.Valid || f.Mark.Int64 > pending[i].Mark.Int64) {
			pending[i].Mark = f.Mark
		}
		pending[i].EndedMark = f.EndedMark
	}
	return pending, nil
}

// LiveRange returns what get_graph_delta reports in rc, up to limit changes:
// the range a full block built now in a's folder or worktree would report
// there, read as the journal stands. ok=false when rc has no baseline. It
// writes nothing.
func LiveRange(ctx context.Context, a *app.App, rc *app.RepoContext, limit int) (graph.ChangeRange, bool, error) {
	pending, err := pendingIn(ctx, a, rc)
	if err != nil {
		return graph.ChangeRange{}, false, err
	}
	return rc.Graph.RecentRange(ctx, rc.Git.Worktree, pending, a.Clock.Now().UnixMilli(), limit)
}

// applySource applies a session start's source to the record of what scope's
// conversation was shown, and reports whether it resumes a conversation that
// already got the block.
func applySource(ctx context.Context, a *app.App, scope, source string) (resumed bool, err error) {
	if scope == "" {
		return false, nil
	}
	switch source {
	case "compact", "clear":
		return false, a.Ledger.Delete(ctx, scope, readMetric, shownMetric, searchMetric)
	case "resume":
		n, err := a.Ledger.Get(ctx, scope, blockMetric)
		return n > 0, err
	}
	return false, nil
}

// sessionStartUpkeep syncs memories, imports the team snapshot of each of repos
// and runs their gated memory compaction. Every session start runs it, whatever
// its source.
func sessionStartUpkeep(ctx context.Context, a *app.App, repos []*app.RepoContext) {
	// Pull remote memory changes before recall, so this session's injected
	// context reflects other machines' work (best-effort, bounded — a wedged or
	// unreachable sync server must never delay the agent). This runs BEFORE the
	// snapshot import below so a snapshot-imported row (which also marks the
	// outbox dirty) has a chance to be pushed in the same hook, not just pulled
	// state that would otherwise be overwritten by an older local snapshot.
	if a.Sync != nil {
		boundedSync(ctx, syncPullBudget, a.Sync.Pull)
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			boundedSync(ctx, syncPullBudget, rc.Sync.Pull)
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		boundedSync(ctx, syncPullBudget, a.Workspace.Sync.Pull)
	}

	// Absorb a teammate's committed memory snapshot when it changed (best-effort).
	if a.Features.On(features.TeamSnapshot) {
		for _, rc := range repos {
			_, _ = snapshot.SyncOnSessionStart(ctx, rc.Engine, rc.Store, rc.Git.Worktree)
		}
	}

	// Push anything now sitting in the local outbox/tombstone queue — a prior
	// session on this machine that recorded something but never reached its own
	// Stop hook (crash, forced kill), AND anything the snapshot import above
	// just queued — so a snapshot-imported memory converges over HTTP sync too,
	// in this same hook, rather than waiting for a later Stop/background/manual
	// sync. No-op when sync is unconfigured (a.Sync == nil).
	if a.Sync != nil {
		boundedSync(ctx, syncPushBudget, a.Sync.Push)
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			boundedSync(ctx, syncPushBudget, rc.Sync.Push)
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		boundedSync(ctx, syncPushBudget, a.Workspace.Sync.Push)
	}

	// Opportunistic memory compaction (best-effort, gated to once per interval,
	// single-flight across worktrees). Runs before recall so this session's
	// recalls stamp the freshly-advanced epoch. Never blocks the agent.
	if a.Features.On(features.AutoCompact) {
		for _, rc := range repos {
			_, _ = rc.Engine.Compact(ctx, memory.CompactOptions{})
		}
	}
}

// resumeContext returns, rendered as the block renders them, the memories the
// full block would show now that scope's conversation has not been shown, and
// marks them shown. No context when there are none.
func resumeContext(ctx context.Context, a *app.App, scope string, results []memory.RecallResult,
	header, wsName string, wsResults []memory.RecallResult, budget int) (Result, error) {
	// A memory the block drops for its budget stays out on a resume too, as it
	// did at startup; otherwise every resume would inject it.
	_, inBlock := formatMemories(results, budget)
	results = slices.DeleteFunc(results, func(r memory.RecallResult) bool { return !slices.Contains(inBlock, r.Record.ID) })
	results, err := unshown(ctx, a, scope, results)
	if err != nil {
		return Result{}, err
	}
	wsResults, err = unshown(ctx, a, scope, wsResults)
	if err != nil {
		return Result{}, err
	}
	out, emitted := formatResumeContext(header, results, wsName, wsResults, budget)
	markSurfaced(ctx, a, scope, emitted)
	return Result{Context: out}, nil
}

// unshown returns the results whose memory scope's conversation has not been shown.
func unshown(ctx context.Context, a *app.App, scope string, rs []memory.RecallResult) ([]memory.RecallResult, error) {
	var out []memory.RecallResult
	for _, r := range rs {
		n, err := a.Ledger.Get(ctx, scope, shownMetric+r.Record.ID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// markSurfaced records each memory id as shown to scope's conversation, mirroring the
// per-id key dedupSurfaced consults so a later just-in-time hook treats them as
// already shown. A no-op without a conversation (matching dedupSurfaced's passthrough).
func markSurfaced(ctx context.Context, a *app.App, scope string, ids []string) {
	if scope == "" {
		return
	}
	for _, id := range ids {
		_ = a.Ledger.Incr(ctx, scope, shownMetric+id, 1)
	}
}

// dedupSurfaced filters recs to those not yet shown to scope's conversation, marking the
// returned ones as shown. A file/symbol-anchored memory re-found on every read or
// edit of the same code would otherwise re-inject a block already sitting in the
// transcript — pure token waste; this keeps each memory's just-in-time surfacing to
// once per conversation. It is keyed by memory id, so a memory anchored to several files
// surfaces at most once regardless of which file triggers it. With no conversation to
// scope to it is a passthrough (mirrors the augment dedup).
func dedupSurfaced(ctx context.Context, a *app.App, scope string, recs []*memory.Record) []*memory.Record {
	if scope == "" || len(recs) == 0 {
		return recs
	}
	out := make([]*memory.Record, 0, len(recs))
	for _, r := range recs {
		metric := shownMetric + r.ID
		if n, err := a.Ledger.Get(ctx, scope, metric); err == nil && n > 0 {
			continue
		}
		_ = a.Ledger.Incr(ctx, scope, metric, 1)
		out = append(out, r)
	}
	return out
}

// handlePostRead logs the read to the ledger and adds, besides the notes
// anchored to the file, at most two advisory notices. The repeat-read notice
// fires when earlier reads in this session returned every line this one did and
// the file has not changed since; a read whose lines are unknown gets none. The
// large-file nudge fires once per session, on a large whole-file read of a file
// with indexed symbols; a read of part of a file never gets it.
func handlePostRead(ctx context.Context, a *app.App, ev Event) (Result, error) {
	file := ev.FilePath
	if file == "" {
		return Result{}, nil
	}
	scope := conversation(ev)
	tokens := int64(ledger.EstimateTokens(ev.ToolResult))

	_ = a.Ledger.Incr(ctx, "lifetime", "reads", 1)
	if scope != "" {
		_ = a.Ledger.Incr(ctx, scope, "reads", 1)
	}

	// Route the file to the served repo that owns it — a session reading a
	// sibling repo's file (a verifier in a scratch dir, RECARTO_REPOS) must
	// surface THAT repo's anchored memories, with the path made relative to
	// that repo's root so it matches how the anchors were recorded.
	all := a.AllRepos()
	target, rel, owned := owningRepo(all, file)
	if !owned {
		target, rel = all[0], memory.RelFileRef(a.Git.Worktree, file)
	}

	first, last := ev.ReadFirst, ev.ReadLast
	var msgs []string
	if scope != "" && first >= 1 && last >= first && a.Features.On(features.RepeatedReads) {
		repeat, err := readBefore(ctx, a, scope, file, first, last)
		if err != nil {
			return Result{}, err
		}
		if repeat {
			_ = a.Ledger.Incr(ctx, "lifetime", "repeated_reads_blocked", 1)
			if coversFile(file, first, last) {
				msgs = append(msgs, fmt.Sprintf("Recarto: you already have all of %s from an earlier read in this session, "+
					"and it has not changed. Use that read instead of reading it again.", rel))
			} else {
				msgs = append(msgs, fmt.Sprintf("Recarto: you already have lines %d-%d of %s from an earlier read in this session, "+
					"and they have not changed. Use that read instead of reading them again.", first, last, rel))
			}
		}
	}
	// A read starting past line 1 asked for a range, whatever the host reported.
	ranged := ev.ReadRanged || first > 1
	if scope != "" && tokens >= int64(largeReadThreshold()) &&
		a.Features.On(features.LargeReads) && toolFeatures(a).On(features.GraphTools) {
		// The Get only skips work once the session's nudge is given; Claim decides.
		nudged, err := a.Ledger.Get(ctx, scope, "large_read_nudged")
		if err != nil {
			return Result{}, err
		}
		if nudged == 0 && (!ranged || coversFile(file, first, last)) {
			// No symbols (or an outline error) means the graph has no cheaper view to offer.
			if syms, err := target.Graph.Outline(ctx, target.Git.Worktree, rel); err == nil && len(syms) > 0 {
				// Claimed last, so a read that can't be nudged never uses up the session's nudge.
				won, err := a.Ledger.Claim(ctx, scope, "large_read_nudged")
				if err != nil {
					return Result{}, err
				}
				if won {
					_ = a.Ledger.Incr(ctx, "lifetime", "large_reads_nudged", 1)
					msgs = append(msgs, fmt.Sprintf(largeReadNudge, rel, tokens, len(syms)))
				}
			}
		}
	}
	msgs = append(msgs, anchoredNotes(ctx, a, scope, target, rel)...)
	if len(msgs) == 0 {
		return Result{}, nil
	}
	return Result{Context: strings.Join(msgs, "\n")}, nil
}

// anchoredNotes returns the bug, guidance and todo blocks anchored to rel in
// target that scope's conversation has not seen yet, marking them seen. It matches
// stored refs by path, so the file need not exist on disk.
func anchoredNotes(ctx context.Context, a *app.App, scope string, target *app.RepoContext, rel string) []string {
	var msgs []string
	if a.Features.On(features.BugWarnings) {
		if found, err := target.Engine.BugsForCode(ctx, target.Git.Branch, []string{rel}, nil); err == nil {
			if found = dedupSurfaced(ctx, a, scope, found); len(found) > 0 {
				msgs = append(msgs, formatBugWarnings(rel, found, toolFeatures(a).On(features.MemoryTools)))
			}
		}
	}
	if a.Features.On(features.Guidance) {
		if found, err := target.Engine.GuidanceForCode(ctx, target.Git.Branch, []string{rel}, nil); err == nil {
			if found = dedupSurfaced(ctx, a, scope, found); len(found) > 0 {
				msgs = append(msgs, formatGuidance(rel, found))
			}
		}
	}
	if a.Features.On(features.Todos) {
		if found, err := target.Engine.TodosForCode(ctx, target.Git.Branch, []string{rel}, nil); err == nil {
			if found = dedupSurfaced(ctx, a, scope, found); len(found) > 0 {
				msgs = append(msgs, formatTodoReminders(rel, found))
			}
		}
	}
	return msgs
}

// handlePostEdit surfaces any prior bug tied to the edited file, then incrementally
// re-indexes it and resolves references. The bug context is built before the
// (best-effort) reindex so RunHook can emit it even if reindex errors.
func handlePostEdit(ctx context.Context, a *app.App, ev Event) (Result, error) {
	abs := ev.FilePath
	if abs == "" {
		return Result{}, nil
	}
	// Route the edit to the served repo that owns it (primary, or a
	// RECARTO_REPOS extra) so its anchored memories fire and ITS graph is
	// reindexed — with the path relative to that repo's root, matching how
	// anchors and graph files are keyed.
	target, rel, owned := owningRepo(a.AllRepos(), abs)
	if !owned {
		return Result{}, nil // outside every served worktree; ignore
	}

	if ev.SessionID != "" {
		// Per-session edit counter; the Stop hook's capture nudge keys on it.
		_ = a.Ledger.Incr(ctx, ev.SessionID, "edits", 1)
	}

	// Surface any prior bug/guidance/todo tied to this file before re-indexing
	// (best-effort), each deduped per session so repeated edits don't re-inject.
	res := Result{Context: strings.Join(anchoredNotes(ctx, a, conversation(ev), target, rel), "\n")}
	content, err := os.ReadFile(abs)
	if err != nil {
		return res, nil // file vanished between edit and hook; ignore
	}
	info, err := os.Stat(abs)
	if err != nil {
		return res, nil
	}
	// Incremental: rewrite this file's graph and re-resolve only the edges the
	// edit can affect (a full resolve only when a Python base or a module others
	// name changes). The previous unconditional full Resolve cost ~14s per edit on
	// a large repo — paid inside the agent's edit hook.
	return res, target.Graph.ReindexFileIncremental(ctx, target.Git.Worktree, rel, content, info.ModTime().UnixMilli(), info.Size())
}

// handleAugment answers a Grep/Glob call with the search hint for its pattern
// over the served repos the call's path reaches (see searchHint).
func handleAugment(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if ev.Pattern == "" || !a.Features.On(features.SearchHints) {
		return Result{}, nil
	}
	hint, err := searchHint(ctx, a, conversation(ev), searchRepos(a, ev.SearchPaths), []string{ev.Pattern})
	return Result{Context: hint}, err
}

// shellSearchNudge points the agent at the MCP tools that beat a shell search.
// It rides along with a session's first shell search only.
const shellSearchNudge = "Recarto: next time, search with the recarto MCP tools instead of the shell: `search_content` " +
	"(regex/text search of the worktree that honours .gitignore and names each hit's enclosing symbol) " +
	"and `search_graph` (symbol and concept search)."

// largeReadNudge points a session's first large whole-file read at the
// cheaper views of an indexed file.
const largeReadNudge = "Recarto: %s is large (~%d tokens read, %d symbols indexed). " +
	"For large indexed files, use the recarto MCP tools `get_file_outline` for the structure or `get_code_snippet` to read one symbol, " +
	"instead of reading the whole file."

// handleShell answers a shell tool call RunHook read as searches and file
// views. The line's searches share one search hint, capped like a Grep/Glob
// call's, and the session's first shell search adds shellSearchNudge. Each printed file in
// a served repo surfaces its anchored memories, deduped exactly like a Read —
// without a Read's counters, repeat warning or large-file nudge. A search whose
// paths all lie outside the served repos adds no hint and no nudge.
func handleShell(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if !a.Features.On(features.ShellHints) {
		return Result{}, nil
	}
	var msgs []string
	scope := conversation(ev)
	repos := searchRepos(a, ev.SearchPaths)
	if a.Features.On(features.SearchHints) {
		hint, err := searchHint(ctx, a, scope, repos, ev.Patterns)
		if err != nil {
			return Result{}, err
		}
		if hint != "" {
			msgs = append(msgs, hint)
		}
	}
	if len(ev.Patterns) > 0 && len(repos) > 0 && scope != "" && toolFeatures(a).On(features.GraphTools) {
		if n, err := a.Ledger.Get(ctx, scope, "shell_search_nudged"); err == nil && n == 0 {
			_ = a.Ledger.Incr(ctx, scope, "shell_search_nudged", 1)
			msgs = append(msgs, shellSearchNudge)
		}
	}
	all := a.AllRepos()
	for _, f := range ev.Files {
		if target, rel, ok := owningRepo(all, f); ok {
			msgs = append(msgs, anchoredNotes(ctx, a, scope, target, rel)...)
		}
	}
	return Result{Context: strings.Join(msgs, "\n")}, nil
}

// stopNudgeEditThreshold is the minimum number of post-edit events in a session
// before an empty memory log warrants the block-once capture nudge.
const stopNudgeEditThreshold = 3

// stopCaptureReminder is the static human-facing Stop reminder — the no-block
// fallback (low-edit sessions, memories already recorded, or any fault in the
// nudge decision). Conversation-heavy low-edit sessions are exactly where
// preference corrections surface, so it must not disappear with the nudge.
const stopCaptureReminder = "recarto: before ending, record any new learnings, bug fixes and preferences with the recarto MCP tools `record_memory` and `record_bug` — e.g. if the user corrected your approach or stated a style/workflow choice."

// stopIssueReminder follows stopCaptureReminder while issue reports are on.
const stopIssueReminder = " And if Recarto itself errored, ranked the wrong result first, confused you, or lacked a capability you needed this session, call the recarto MCP tool `report_recarto_issue` so it can be fixed."

func endStopSession(ctx, writes context.Context, a *app.App, ev Event, roster []rosterRepo, now int64) (conversationEnd, error) {
	if len(roster) == 0 {
		err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`, now, ev.SessionID)
			return err
		})
		return conversationEnd{}, err
	}
	covered, _, done := cover(a, roster)
	defer done()
	targets, err := endTargets(roster, covered)
	e, endErr := endConversation(writes, a, ev.SessionID, targets, true)
	return e, errors.Join(err, endErr)
}

func pushStopSync(ctx context.Context, a *app.App) {
	if a.Sync != nil {
		boundedSync(ctx, syncPushBudget, a.Sync.Push)
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			boundedSync(ctx, syncPushBudget, rc.Sync.Push)
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		boundedSync(ctx, syncPushBudget, a.Workspace.Sync.Push)
	}
}

// handleStop closes the session row and, at most once per session, blocks the
// stop with a capture nudge when the session edited code but recorded nothing.
// The nudge must reach the MODEL, not the human — a systemMessage is only shown
// to the user, so the only acting channel is a block-once with the reason fed
// back as an instruction. ev.StopActive is the host's loop guard: when Claude
// Code re-invokes Stop after our block, always pass through. Every fault in the
// nudge decision degrades to the static reminder — never to silence, and never
// to a false block.
func handleStop(ctx context.Context, writes context.Context, a *app.App, ev Event, roster []rosterRepo, rosterErr error) (res Result, err error) {
	if ev.SessionID == "" {
		return Result{}, nil
	}
	now := a.Clock.Now().UnixMilli()
	e, endErr := endStopSession(ctx, writes, a, ev, roster, now)
	if len(roster) == 0 {
		if endErr != nil {
			return Result{}, endErr
		}
	} else {
		rosterErr = endErr
		if e.launch && a.EndLater != nil {
			a.EndLater(ev.SessionID)
		}
	}
	defer func() { res.Failure = errors.Join(rosterErr, res.Failure) }()
	// Turn boundary: the agent is idle, so a TRUNCATE checkpoint can actually
	// win the WAL (mid-session it silently yields to concurrent readers).
	_ = a.Store.Checkpoint(ctx)
	// Flush anything recorded this session to the remote sync server
	// (best-effort, bounded). This is the linchpin for an ephemeral host (e.g. a
	// ForgeBoard microVM): without it, memories recorded in a session that is
	// about to be destroyed would never leave this machine.
	pushStopSync(ctx, a)
	if ev.StopActive {
		return Result{}, nil
	}
	// Without the memory tools, or without a server to call them on, there is
	// nothing to remind the agent to call.
	f := toolFeatures(a)
	if !f.On(features.MemoryTools) {
		return Result{}, nil
	}
	reminder := ""
	if a.Features.On(features.StopReminder) {
		reminder = stopCaptureReminder
		if f.On(features.IssueReports) {
			reminder += stopIssueReminder
		}
	}
	edits, err := a.Ledger.Get(ctx, ev.SessionID, "edits")
	if err != nil || edits < stopNudgeEditThreshold || !a.Features.On(features.StopNudge) {
		return Result{SystemMessage: reminder}, nil
	}
	nudged, err := conversationNudged(ctx, a, ev.SessionID)
	if err != nil {
		return Result{SystemMessage: reminder}, nil
	}
	if nudged {
		// The model just saw the block reason; re-showing the reminder is noise.
		return Result{}, nil
	}
	var startedAt int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE id = ?`, ev.SessionID).Scan(&startedAt); err != nil {
		return Result{SystemMessage: reminder}, nil
	}
	// "Recorded anything" must count reinforcements too: Record on a near-dup
	// bumps only updated_at (created_at stays with the original), and the
	// contract is "record freely — repeated learnings converge", so a session
	// whose record converged must not be told it recorded nothing. Inserts stamp
	// updated_at = created_at, so they count through the same predicate; the
	// status filter keeps archive/supersede/resolve/done bumps out. Known benign
	// noise, both failing quiet (nudge skipped, never falsely fired): a
	// session-start compact absorb, or a concurrent session recording on this
	// branch.
	var recorded int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories
		  WHERE updated_at >= ? AND status = 'active' AND (branch IS NULL OR branch = ?)`,
		startedAt, a.Git.Branch).Scan(&recorded); err != nil || recorded > 0 {
		return Result{SystemMessage: reminder}, nil
	}
	// A session that recorded only workspace-scoped or served-repo memories
	// recorded something: the nudge must not fire on it. Workspace rows are
	// branch-less (NULL); a served repo's rows count regardless of branch.
	if a.Workspace != nil {
		if err := a.Workspace.Store.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories WHERE updated_at >= ? AND status = 'active'`,
			startedAt).Scan(&recorded); err != nil || recorded > 0 {
			return Result{SystemMessage: reminder}, nil
		}
	}
	for _, rc := range a.Repos {
		if err := rc.Store.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories WHERE updated_at >= ? AND status = 'active'`,
			startedAt).Scan(&recorded); err != nil || recorded > 0 {
			return Result{SystemMessage: reminder}, nil
		}
	}
	markErr := markNudged(writes, a, ev.SessionID)
	reason := fmt.Sprintf("recarto: you made %d edits this session and recorded nothing. Record learnings, preferences (especially if the user corrected your approach), or bug fixes now with the recarto MCP tools `record_memory` and `record_bug`", edits)
	if f.On(features.IssueReports) {
		reason += " — and `report_recarto_issue` if Recarto itself misbehaved"
	}
	return Result{Block: true, Reason: reason + ". Or finish if there's genuinely nothing to keep.", Failure: markErr}, nil
}

// conversationNudged reports whether sessionID's conversation already got the
// capture nudge: its ledger counter is set or its mark file exists.
func conversationNudged(ctx context.Context, a *app.App, sessionID string) (bool, error) {
	nudged, err := a.Ledger.Get(ctx, sessionID, "stop_nudged")
	if err != nil || nudged > 0 {
		return nudged > 0, err
	}
	_, err = os.Stat(nudgeMarkPath(a.Store, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// markNudged records that sessionID's conversation got the capture nudge.
func markNudged(writes context.Context, a *app.App, sessionID string) error {
	err := a.Ledger.Incr(writes, sessionID, "stop_nudged", 1)
	if err == nil {
		return nil
	}
	var markErr error
	// A Stop of a folder conversation gives a busy store no more than its write
	// deadline: the mark file then keeps the nudge once per conversation.
	if !store.IsBusy(err) {
		markErr = fmt.Errorf("record the capture nudge: %w", err)
	}
	mark := nudgeMarkPath(a.Store, sessionID)
	if err := os.MkdirAll(filepath.Dir(mark), 0o755); err != nil {
		return errors.Join(markErr, err)
	}
	return errors.Join(markErr, os.WriteFile(mark, nil, 0o644))
}

// nudgeMarkPath marks a conversation nudged when its ledger counter could not
// be written. It is never removed.
func nudgeMarkPath(st *store.Store, sessionID string) string {
	return filepath.Join(storeDir(st), "stop-nudged", endName(sessionID))
}
