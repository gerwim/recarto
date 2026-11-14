package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		return handleStop(ctx, a, ev)
	case KindShell:
		return handleShell(ctx, a, ev)
	}
	return Result{}, nil
}

// sessionStartIndexBudget bounds the synchronous graph warm-up inside the
// session-start hook. Most repos index (or no-op) well inside it; a huge first
// index makes bounded progress each session instead of blocking the host. A
// var so tests can shrink it.
var sessionStartIndexBudget = 15 * time.Second

// syncPullBudget / syncPushBudget bound the best-effort HTTP memory-sync pull
// (session-start, before recall) and push (Stop). Sync payloads are small
// text-only memory rows (never the code graph), so these are much shorter
// than the graph warm-up budget; a wedged or unreachable sync server must
// never delay the agent. Vars so tests can shrink them.
var syncPullBudget = 5 * time.Second
var syncPushBudget = 5 * time.Second

// boundedSync runs fn (a Syncer.Pull or Syncer.Push call) with a hard
// deadline, exactly like the graph warm-up goroutine below: abandoning a slow
// or unreachable server is safe because sync is best-effort and additive —
// nothing local depends on it completing.
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

// newGraphSource builds the file source for the session-start warm-up; a var so
// tests can substitute a controllable source.
var newGraphSource = graph.NewSource

// agentOrDefault stamps an unknown/empty agent as the legacy Claude Code value so
// the sessions.agent column always carries a host name.
func agentOrDefault(s string) string {
	if s == "" {
		return "claude-code"
	}
	return s
}

// owningRepo maps an absolute file path to the served repo containing it —
// the primary, or a RECARTO_REPOS extra — returning that repo's context and
// the repo-relative path. Longest worktree prefix wins (a nested checkout
// belongs to the inner repo). ok=false when no served worktree contains the
// file, or the path is not absolute.
func owningRepo(a *app.App, abs string) (rc *app.RepoContext, rel string, ok bool) {
	bestLen := -1
	for _, cand := range a.AllRepos() {
		wt := cand.Git.Worktree
		if r, in := within(wt, abs); in && len(wt) > bestLen {
			rc, rel, ok, bestLen = cand, r, true, len(wt)
		}
	}
	return
}

// Owned reports whether an absolute path lies in one of the worktrees — the
// store-free form of owningRepo, against app.Worktrees.
func Owned(worktrees []string, abs string) bool {
	for _, wt := range worktrees {
		if _, in := within(wt, abs); in {
			return true
		}
	}
	return false
}

// within returns abs relative to worktree wt (slash-separated), ok=false when
// abs is not absolute or lies outside wt.
func within(wt, abs string) (string, bool) {
	if wt == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	r, err := filepath.Rel(wt, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
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

// handleSessionStart opens/creates the session row, warms the code graph, runs
// gated compaction, recalls a token-budgeted slice for this branch, and returns it
// as injectable context plus a record nudge. A resume of a conversation that
// already got the block returns only the memories it has not been shown yet.
func handleSessionStart(ctx context.Context, a *app.App, ev Event) (Result, error) {
	now := a.Clock.Now().UnixMilli()
	if ev.SessionID != "" {
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO sessions(id, agent, branch, worktree, started_at)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`,
				ev.SessionID, agentOrDefault(ev.Agent), nullable(a.Git.Branch), a.Git.Worktree, now)
			return err
		}); err != nil {
			return Result{}, err
		}
	}
	_ = a.Ledger.Incr(ctx, "lifetime", "sessions", 1)
	scope := conversation(ev)
	// Forget before the upkeep, so a hook the host kills mid-upkeep still leaves a
	// compacted conversation's record cleared.
	resumed, err := applySource(ctx, a, scope, ev.Source)
	if err != nil {
		return Result{}, err
	}
	sessionStartUpkeep(ctx, a)

	budget := sessionBudget()
	repoBudget := budget
	// The workspace tier borrows a third of the session budget rather than
	// growing the block: RECARTO_SESSION_BUDGET stays the whole block's bound.
	var results, wsResults []memory.RecallResult
	if a.Features.On(features.SessionMemories) {
		if a.Workspace != nil {
			wsBudget := budget / 3
			repoBudget = budget - wsBudget
			if wsSet, err := a.Workspace.Engine.Recall(ctx, memory.RecallQuery{
				Baseline:    true,
				TokenBudget: wsBudget,
			}); err == nil {
				wsResults = wsSet.Results
			}
		}
		set, err := a.Engine.Recall(ctx, memory.RecallQuery{
			Branch:      a.Git.Branch,
			Worktree:    a.Git.Worktree,
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
	// "What changed since you were last here": baseline = the last session that
	// journaled deltas, so /clear and open-and-close sessions are transparent
	// (the same delta keeps showing until a context has lived alongside it).
	var deltas []graph.Delta
	var deltaTotal int
	var deltaBaseline int64
	if at, ok, err := a.Graph.DeltaBaseline(ctx, a.Git.Worktree, now); err == nil && ok {
		deltaBaseline = at
		if a.Features.On(features.RecentChanges) {
			deltas, deltaTotal, _ = a.Graph.DeltasSince(ctx, a.Git.Worktree, at, 200)
		}
	}
	// Retention: drop journal rows no baseline walk can reach (best-effort).
	// The walk is unbounded — it skips no-op sessions all the way back to the
	// last delta-bearing one — so the cutoff must never pass the current
	// baseline: any run of consecutive /clear or read-only sessions, however
	// long, must not erase the deltas the tier is still reporting. Past that,
	// rows older than the 10th-most-recent session are unreachable.
	if cutoff, err := tenthSessionStart(ctx, a); err == nil && cutoff > 0 {
		if deltaBaseline > 0 && deltaBaseline < cutoff {
			cutoff = deltaBaseline
		}
		_ = a.Graph.PruneDeltas(ctx, a.Git.Worktree, cutoff)
	}

	wsName := ""
	if a.Workspace != nil {
		wsName = a.Workspace.Name
	}
	if resumed {
		return resumeContext(ctx, a, scope, results, wsName, wsResults, repoBudget)
	}
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	var arch graph.Architecture
	if a.Features.On(features.CodeMap) {
		arch, _ = a.Graph.Architecture(ctx, a.Git.Worktree, "")
	}
	var bugs []*memory.Record
	if a.Features.On(features.RecentBugs) {
		bugs, _ = a.Engine.RecentBugs(ctx, a.Git.Branch, recentBugLimit)
	}

	var extraRepos []string
	for _, rc := range a.Repos {
		extraRepos = append(extraRepos, rc.Git.RepoName)
	}
	out, emitted := formatSessionContext(results, wsName, wsResults, extraRepos, arch, bugs, deltas, deltaTotal, deltaBaseline, repoBudget, a.Features)
	// Mark every memory rendered into this block as surfaced for the conversation, so the
	// just-in-time read/edit hooks won't re-inject a guardrail/bug/todo the agent can
	// already see here. Memories dropped under budget stay unmarked and still surface
	// when their code is touched.
	markSurfaced(ctx, a, scope, emitted)
	if scope != "" {
		// A lost mark only costs the next resume a full block, as before.
		_ = a.Ledger.Incr(ctx, scope, blockMetric, 1)
	}
	return Result{Context: out}, nil
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

// sessionStartUpkeep warms the code graph, syncs memories, imports the team
// snapshot and runs gated memory compaction. Every session start runs it,
// whatever its source.
func sessionStartUpkeep(ctx context.Context, a *app.App) {
	// Warm the code graph for this worktree — best-effort and budget-bounded: a
	// first index of a large repo would otherwise block the host's hook (and the
	// user's first message) for minutes. The budget is HARD: the reconcile runs
	// in a goroutine and is abandoned at the deadline, because parts of it
	// (git ls-files, filesystem walks, a wedged mount) are not context-aware.
	// Abandoning is safe — the hook is a one-shot process, committed batches
	// survive, the crash-safe pending marker records an unfinished resolve, and
	// a later reconcile (next session, or an MCP query's background refresh)
	// finishes the job.
	sctx, cancel := context.WithTimeout(ctx, sessionStartIndexBudget)
	defer cancel()
	warmed := make(chan struct{})
	go func() {
		defer close(warmed)
		defer recoverHook()
		_ = a.Graph.ReconcileIfStale(sctx, a.Git.Worktree, newGraphSource(a.Git.Worktree), graph.WorktreeToken(a.Git.Worktree))
		// Extra served repos (RECARTO_REPOS) reconcile inside the same hard
		// budget — usually a no-op, since sessions cd'd into those repos keep
		// their graphs fresh; a cold host makes bounded progress per session.
		for _, rc := range a.Repos {
			_ = rc.Graph.ReconcileIfStale(sctx, rc.Git.Worktree, newGraphSource(rc.Git.Worktree), graph.WorktreeToken(rc.Git.Worktree))
		}
		// Opportunistic worktree GC, inside the same hard budget: a deleted git
		// worktree leaves its entire graph copy in the shared store until swept.
		// Batched deletes commit progress, so abandonment at the budget is safe.
		if dead, err := a.DeadWorktrees(sctx); err == nil && len(dead) > 0 {
			for _, wt := range dead {
				_ = a.Graph.PruneWorktree(sctx, wt)
			}
			_ = a.Store.Checkpoint(sctx)
		}
	}()
	select {
	case <-warmed:
	case <-sctx.Done():
	}

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
		_, _ = snapshot.SyncOnSessionStart(ctx, a.Engine, a.Store, a.Git.Worktree)
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
		_, _ = a.Engine.Compact(ctx, memory.CompactOptions{})
	}
}

// resumeContext returns, rendered as the block renders them, the memories the
// full block would show now that scope's conversation has not been shown, and
// marks them shown. No context when there are none.
func resumeContext(ctx context.Context, a *app.App, scope string, results []memory.RecallResult,
	wsName string, wsResults []memory.RecallResult, budget int) (Result, error) {
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
	out, emitted := formatResumeContext(results, wsName, wsResults, budget)
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
	target, rel, owned := owningRepo(a, file)
	if !owned {
		target, rel = a.AllRepos()[0], memory.RelFileRef(a.Git.Worktree, file)
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
		a.Features.On(features.LargeReads) && a.Features.On(features.GraphTools) {
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
					msgs = append(msgs, fmt.Sprintf("Recarto: %s is large (~%d tokens read, %d symbols indexed). "+
						"For large indexed files, use get_file_outline for the structure or get_code_snippet to read one symbol, "+
						"instead of reading the whole file.", rel, tokens, len(syms)))
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
				msgs = append(msgs, formatBugWarnings(rel, found))
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
	target, rel, owned := owningRepo(a, abs)
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

// handleAugment runs a code-graph search matching the agent's Grep/Glob pattern and
// also surfaces any kind=bug memories matching that pattern.
func handleAugment(ctx context.Context, a *app.App, ev Event) (Result, error) {
	pattern := ev.Pattern
	if pattern == "" || !a.Features.On(features.SearchHints) {
		return Result{}, nil
	}

	// Session-scoped dedup: re-running the same Grep/Glob pattern would re-inject
	// a symbol/bug block already sitting earlier in the transcript — pure token
	// waste. Suppress the repeat (mirrors the repeated-read path) and nudge the
	// agent to rely on the earlier injection instead.
	scope := conversation(ev)
	metric := searchMetric + pattern
	if scope != "" {
		if n, err := a.Ledger.Get(ctx, scope, metric); err == nil && n > 0 {
			_ = a.Ledger.Incr(ctx, "lifetime", "repeated_augments_blocked", 1)
			return Result{Context: fmt.Sprintf("You already searched %q this session — the matching symbols/bugs were surfaced earlier. "+
				"Rely on that injection instead of re-searching.", pattern)}, nil
		}
	}

	// A Glob pattern (e.g. "*.cs") is a path query, not a symbol query: searching
	// symbols for its literal stem injects irrelevant matches (minified-bundle
	// functions named "cs", etc.). Reduce file globs to their identifier stem and
	// skip injection entirely for stemless ones; plain Grep terms pass through.
	term, ok := augmentSearchTerm(pattern)
	if !ok {
		return Result{}, nil
	}

	var b strings.Builder
	// Search every served repo's graph (primary first), capping the combined
	// page — a repo-less session's greps are about the sibling repos' code, so
	// primary-only search would inject nothing. Extra repos' paths are
	// prefixed with their repo name so hits stay unambiguous.
	var symLines []string
	for _, rc := range a.AllRepos() {
		if len(symLines) >= 8 {
			break
		}
		res, err := rc.Graph.Search(ctx, rc.Git.Worktree, term, "", "", 8-len(symLines))
		if err != nil {
			continue
		}
		prefix := ""
		if rc.Git.RepoID != a.Git.RepoID {
			prefix = rc.Git.RepoName + ":"
		}
		for _, r := range res {
			symLines = append(symLines, fmt.Sprintf("- [%s] %s  %s%s:%d", r.Kind, r.QualifiedName, prefix, r.Path, r.Line))
		}
	}
	if len(symLines) > 0 {
		b.WriteString("Recarto code graph — symbols matching your search:\n")
		b.WriteString(strings.Join(symLines, "\n"))
		if a.Features.On(features.GraphTools) {
			b.WriteString("\nUse get_code_snippet to read one symbol instead of the whole file.")
		}
	}

	var bugResults []memory.RecallResult
	for _, rc := range a.AllRepos() {
		if len(bugResults) >= 5 {
			break
		}
		set, err := rc.Engine.Recall(ctx, memory.RecallQuery{
			TaskHint: term, Kind: memory.KindBug, Branch: rc.Git.Branch, Worktree: rc.Git.Worktree,
			Limit: 5 - len(bugResults), LexicalOnly: true,
		})
		if err != nil {
			continue
		}
		for _, r := range set.Results {
			if r.Record.Kind == memory.KindBug { // recall always includes pinned do_not_repeat
				bugResults = append(bugResults, r)
			}
		}
	}
	{
		// Drop bugs already surfaced this session — at session-start, on a read/edit,
		// or by an earlier search — so the same bug block is never injected twice
		// (pure token waste). dedupSurfaced also marks the survivors, so a later
		// read/edit of the same code stays quiet about a bug this search just showed.
		var bugRecs []*memory.Record
		for _, r := range bugResults {
			bugRecs = append(bugRecs, r.Record)
		}
		fresh := make(map[string]bool, len(bugRecs))
		for _, r := range dedupSurfaced(ctx, a, scope, bugRecs) {
			fresh[r.ID] = true
		}
		var bugLines []string
		for _, r := range bugResults {
			if !fresh[r.Record.ID] {
				continue
			}
			if fix, _ := r.Record.Extra["fix"].(string); fix != "" {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s — fix: %s", r.Record.Title, fix))
			} else {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s", r.Record.Title))
			}
			if c := r.Caveat(); c != "" {
				bugLines = append(bugLines, "  "+c)
			}
		}
		if len(bugLines) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("Related bugs")
			if a.Features.On(features.MemoryTools) {
				b.WriteString(" (search_bugs for detail)")
			}
			b.WriteString(":\n")
			b.WriteString(strings.Join(bugLines, "\n"))
		}
	}

	out := b.String()
	// Mark the pattern as injected so a later repeat in this session is deduped.
	// Only record when content was actually injected and the session is scoped,
	// so the reminder never fires for an empty first result.
	if out != "" && scope != "" {
		_ = a.Ledger.Incr(ctx, scope, metric, 1)
	}
	return Result{Context: out}, nil
}

// shellSearchNudge points the agent at the MCP tools that beat a shell search.
// It rides along with a session's first shell search only.
const shellSearchNudge = "Recarto: next time, search with its MCP tools instead of the shell — search_content " +
	"(regex/text search of the worktree that honours .gitignore and names each hit's enclosing symbol) " +
	"and search_graph (symbol and concept search)."

// handleShell answers a shell tool call RunHook read as searches and file
// views. Each search gets the hint a Grep/Glob tool call with that pattern
// gets (caps, glob stems and the per-session repeat reminder included), and
// the session's first shell search adds shellSearchNudge. Each printed file in
// a served repo surfaces its anchored memories, deduped exactly like a Read —
// without a Read's counters, repeat warning or large-file nudge.
func handleShell(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if !a.Features.On(features.ShellHints) {
		return Result{}, nil
	}
	var msgs []string
	for _, p := range ev.Patterns {
		res, err := handleAugment(ctx, a, Event{Kind: KindAugment, SessionID: ev.SessionID, AgentID: ev.AgentID, Pattern: p})
		if err != nil {
			return Result{}, err
		}
		if res.Context != "" {
			msgs = append(msgs, res.Context)
		}
	}
	scope := conversation(ev)
	if len(ev.Patterns) > 0 && scope != "" && a.Features.On(features.GraphTools) {
		if n, err := a.Ledger.Get(ctx, scope, "shell_search_nudged"); err == nil && n == 0 {
			_ = a.Ledger.Incr(ctx, scope, "shell_search_nudged", 1)
			msgs = append(msgs, shellSearchNudge)
		}
	}
	for _, f := range ev.Files {
		if target, rel, ok := owningRepo(a, f); ok {
			msgs = append(msgs, anchoredNotes(ctx, a, scope, target, rel)...)
		}
	}
	return Result{Context: strings.Join(msgs, "\n")}, nil
}

// tenthSessionStart returns the started_at of the worktree's 10th-most-recent
// session, or 0 when fewer exist. It is the retention horizon for graph_deltas
// rows — but only after the caller clamps it to the current delta baseline,
// which an unbounded run of no-op sessions can push past this horizon.
func tenthSessionStart(ctx context.Context, a *app.App) (int64, error) {
	var at int64
	err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE worktree = ?
		  ORDER BY started_at DESC LIMIT 1 OFFSET 9`, a.Git.Worktree).Scan(&at)
	if err != nil {
		return 0, nil // fewer than 10 sessions: nothing to prune (sql.ErrNoRows folds here)
	}
	return at, nil
}

// stopNudgeEditThreshold is the minimum number of post-edit events in a session
// before an empty memory log warrants the block-once capture nudge.
const stopNudgeEditThreshold = 3

// stopCaptureReminder is the static human-facing Stop reminder — the no-block
// fallback (low-edit sessions, memories already recorded, or any fault in the
// nudge decision). Conversation-heavy low-edit sessions are exactly where
// preference corrections surface, so it must not disappear with the nudge.
const stopCaptureReminder = "recarto: before ending, record any new learnings (record_memory), bug fixes (record_bug), and preferences — e.g. if the user corrected your approach or stated a style/workflow choice."

// stopIssueReminder follows stopCaptureReminder while issue reports are on.
const stopIssueReminder = " And if Recarto itself errored, ranked the wrong result first, confused you, or lacked a capability you needed this session, call report_recarto_issue so it can be fixed."

// handleStop closes the session row and, at most once per session, blocks the
// stop with a capture nudge when the session edited code but recorded nothing.
// The nudge must reach the MODEL, not the human — a systemMessage is only shown
// to the user, so the only acting channel is a block-once with the reason fed
// back as an instruction. ev.StopActive is the host's loop guard: when Claude
// Code re-invokes Stop after our block, always pass through. Every fault in the
// nudge decision degrades to the static reminder — never to silence, and never
// to a false block.
func handleStop(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if ev.SessionID == "" {
		return Result{}, nil
	}
	now := a.Clock.Now().UnixMilli()
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
			now, ev.SessionID)
		return err
	}); err != nil {
		return Result{}, err
	}
	// Turn boundary: the agent is idle, so a TRUNCATE checkpoint can actually
	// win the WAL (mid-session it silently yields to concurrent readers).
	_ = a.Store.Checkpoint(ctx)
	// Flush anything recorded this session to the remote sync server
	// (best-effort, bounded). This is the linchpin for an ephemeral host (e.g. a
	// ForgeBoard microVM): without it, memories recorded in a session that is
	// about to be destroyed would never leave this machine.
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
	// Without the memory tools there is nothing to remind the agent to call.
	if ev.StopActive || !a.Features.On(features.MemoryTools) {
		return Result{}, nil
	}
	reminder := ""
	if a.Features.On(features.StopReminder) {
		reminder = stopCaptureReminder
		if a.Features.On(features.IssueReports) {
			reminder += stopIssueReminder
		}
	}
	edits, err := a.Ledger.Get(ctx, ev.SessionID, "edits")
	if err != nil || edits < stopNudgeEditThreshold || !a.Features.On(features.StopNudge) {
		return Result{SystemMessage: reminder}, nil
	}
	nudged, err := a.Ledger.Get(ctx, ev.SessionID, "stop_nudged")
	if err != nil {
		return Result{SystemMessage: reminder}, nil
	}
	if nudged > 0 {
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
	_ = a.Ledger.Incr(ctx, ev.SessionID, "stop_nudged", 1)
	reason := fmt.Sprintf("recarto: you made %d edits this session and recorded nothing. Record learnings, preferences (especially if the user corrected your approach), or bug fixes now (record_memory / record_bug)", edits)
	if a.Features.On(features.IssueReports) {
		reason += " — and report_recarto_issue if Recarto itself misbehaved"
	}
	return Result{Block: true, Reason: reason + ". Or finish if there's genuinely nothing to keep."}, nil
}
