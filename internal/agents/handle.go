package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/snapshot"
	"git.gerwim.nl/gerwim/recarto/internal/store"
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
	}
	return Result{}, nil
}

// agentOrDefault stamps an unknown/empty agent as the legacy Claude Code value so
// the sessions.agent column always carries a host name.
func agentOrDefault(s string) string {
	if s == "" {
		return "claude-code"
	}
	return s
}

// handleSessionStart opens/creates the session row, warms the code graph, runs
// gated compaction, recalls a token-budgeted slice for this branch, and returns it
// as injectable context plus a record nudge.
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

	// Warm the code graph for this worktree (best-effort, non-blocking).
	_ = a.Graph.ReconcileIfStale(ctx, a.Git.Worktree, graph.NewSource(a.Git.Worktree), graph.WorktreeToken(a.Git.Worktree))

	// Absorb a teammate's committed memory snapshot when it changed (best-effort).
	_, _ = snapshot.SyncOnSessionStart(ctx, a.Engine, a.Store, a.Git.Worktree)

	// Opportunistic memory compaction (best-effort, gated to once per interval,
	// single-flight across worktrees). Runs before recall so this session's
	// recalls stamp the freshly-advanced epoch. Never blocks the agent.
	_, _ = a.Engine.Compact(ctx, memory.CompactOptions{})

	budget := sessionBudget()
	set, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		Worktree:    a.Git.Worktree,
		Baseline:    true,
		TokenBudget: budget,
	})
	if err != nil {
		return Result{}, err
	}
	results := set.Results
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	arch, _ := a.Graph.Architecture(ctx, a.Git.Worktree, "")
	bugs, _ := a.Engine.RecentBugs(ctx, a.Git.Branch, recentBugLimit)
	out, emitted := formatSessionContext(results, arch, bugs, budget)
	// Mark every memory rendered into this block as surfaced for the session, so the
	// just-in-time read/edit hooks won't re-inject a guardrail/bug/todo the agent can
	// already see here. Memories dropped under budget stay unmarked and still surface
	// when their code is touched.
	markSurfaced(ctx, a, ev.SessionID, emitted)
	return Result{Context: out}, nil
}

// markSurfaced records each memory id as surfaced for the session, mirroring the
// per-id key dedupSurfaced consults so a later just-in-time hook treats them as
// already shown. A no-op without a session (matching dedupSurfaced's passthrough).
func markSurfaced(ctx context.Context, a *app.App, sessionID string, ids []string) {
	if sessionID == "" {
		return
	}
	scope := "session:" + sessionID
	for _, id := range ids {
		_ = a.Ledger.Incr(ctx, scope, "surfaced:"+id, 1)
	}
}

// dedupSurfaced filters recs to those not yet surfaced in this session, marking the
// returned ones as surfaced. A file/symbol-anchored memory re-found on every read or
// edit of the same code would otherwise re-inject a block already sitting in the
// transcript — pure token waste; this keeps each memory's just-in-time surfacing to
// once per session. It is keyed by memory id, so a memory anchored to several files
// surfaces at most once regardless of which file triggers it. With no session to
// scope to it is a passthrough (mirrors the augment dedup).
func dedupSurfaced(ctx context.Context, a *app.App, sessionID string, recs []*memory.Record) []*memory.Record {
	if sessionID == "" || len(recs) == 0 {
		return recs
	}
	scope := "session:" + sessionID
	out := make([]*memory.Record, 0, len(recs))
	for _, r := range recs {
		metric := "surfaced:" + r.ID
		if n, err := a.Ledger.Get(ctx, scope, metric); err == nil && n > 0 {
			continue
		}
		_ = a.Ledger.Incr(ctx, scope, metric, 1)
		out = append(out, r)
	}
	return out
}

// handlePostRead logs the read to the ledger and warns (non-blocking) when the same
// file has already been read this session, plus surfaces any prior bug tied to it.
func handlePostRead(ctx context.Context, a *app.App, ev Event) (Result, error) {
	file := ev.FilePath
	if file == "" {
		return Result{}, nil
	}
	scope := "session:" + ev.SessionID
	tokens := int64(ledger.EstimateTokens(ev.ToolResult))

	_ = a.Ledger.Incr(ctx, "lifetime", "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "read:"+file, 1)

	n, err := a.Ledger.Get(ctx, scope, "read:"+file)
	if err != nil {
		return Result{}, err
	}

	var msgs []string
	if n > 1 {
		_ = a.Ledger.Incr(ctx, "lifetime", "repeated_reads_blocked", 1)
		msgs = append(msgs, fmt.Sprintf("You have already read %s in this session (%d times, ~%d tokens each). "+
			"Rely on the earlier read instead of reading it again.", file, n, tokens))
	}
	rel := memory.RelFileRef(a.Git.Worktree, file)
	// First whole-file read of a large, graph-indexed file: nudge toward the cheaper
	// navigation tools. Whole-file reads of big files are the dominant token sink, and
	// the graph offers a structural alternative only when the file actually has symbols.
	// Gated to the first read (n == 1) because a repeat already gets the stronger
	// "rely on the earlier read" warning above — so this fires at most once per file.
	if n == 1 && tokens >= int64(largeReadThreshold()) {
		if syms, err := a.Graph.Outline(ctx, a.Git.Worktree, rel); err == nil && len(syms) > 0 {
			_ = a.Ledger.Incr(ctx, "lifetime", "large_reads_nudged", 1)
			msgs = append(msgs, fmt.Sprintf("Recarto: %s is large (~%d tokens, %d symbols indexed). "+
				"Next time use get_file_outline for its structure or get_code_snippet to read one symbol, "+
				"instead of reading the whole file.", rel, tokens, len(syms)))
		}
	}
	if found, err := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatBugWarnings(rel, found))
		}
	}
	if found, err := a.Engine.GuidanceForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatGuidance(rel, found))
		}
	}
	if found, err := a.Engine.TodosForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatTodoReminders(rel, found))
		}
	}
	if len(msgs) == 0 {
		return Result{}, nil
	}
	return Result{Context: strings.Join(msgs, "\n")}, nil
}

// handlePostEdit surfaces any prior bug tied to the edited file, then incrementally
// re-indexes it and resolves references. The bug context is built before the
// (best-effort) reindex so RunHook can emit it even if reindex errors.
func handlePostEdit(ctx context.Context, a *app.App, ev Event) (Result, error) {
	abs := ev.FilePath
	if abs == "" {
		return Result{}, nil
	}
	wt := a.Git.Worktree
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return Result{}, nil // outside the worktree; ignore
	}
	rel = filepath.ToSlash(rel)

	var res Result
	// Surface any prior bug/guidance/todo tied to this file before re-indexing
	// (best-effort). This matches stored refs by path and does not require the file
	// on disk. Each is deduped per session so repeated edits don't re-inject.
	var msgs []string
	if found, berr := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatBugWarnings(rel, found))
		}
	}
	if found, berr := a.Engine.GuidanceForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatGuidance(rel, found))
		}
	}
	if found, berr := a.Engine.TodosForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil {
		if found = dedupSurfaced(ctx, a, ev.SessionID, found); len(found) > 0 {
			msgs = append(msgs, formatTodoReminders(rel, found))
		}
	}
	res.Context = strings.Join(msgs, "\n")
	content, err := os.ReadFile(abs)
	if err != nil {
		return res, nil // file vanished between edit and hook; ignore
	}
	info, err := os.Stat(abs)
	if err != nil {
		return res, nil
	}
	if err := a.Graph.IndexFile(ctx, wt, rel, content, info.ModTime().UnixMilli(), info.Size()); err != nil {
		return res, err
	}
	return res, a.Graph.Resolve(ctx, wt)
}

// handleAugment runs a code-graph search matching the agent's Grep/Glob pattern and
// also surfaces any kind=bug memories matching that pattern.
func handleAugment(ctx context.Context, a *app.App, ev Event) (Result, error) {
	pattern := ev.Pattern
	if pattern == "" {
		return Result{}, nil
	}

	// Session-scoped dedup: re-running the same Grep/Glob pattern would re-inject
	// a symbol/bug block already sitting earlier in the transcript — pure token
	// waste. Suppress the repeat (mirrors the repeated-read path) and nudge the
	// agent to rely on the earlier injection instead.
	scope := "session:" + ev.SessionID
	metric := "augment:" + pattern
	if ev.SessionID != "" {
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
	if res, err := a.Graph.Search(ctx, a.Git.Worktree, term, "", "", 8); err == nil && len(res) > 0 {
		b.WriteString("Recarto code graph — symbols matching your search:\n")
		for _, r := range res {
			fmt.Fprintf(&b, "- [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
		}
		b.WriteString("Use get_code_snippet to read one symbol instead of the whole file.")
	}

	if set, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: term, Kind: memory.KindBug, Branch: a.Git.Branch, Worktree: a.Git.Worktree, Limit: 5,
	}); err == nil {
		// Drop bugs already surfaced this session — at session-start, on a read/edit,
		// or by an earlier search — so the same bug block is never injected twice
		// (pure token waste). dedupSurfaced also marks the survivors, so a later
		// read/edit of the same code stays quiet about a bug this search just showed.
		var bugRecs []*memory.Record
		for _, r := range set.Results {
			if r.Record.Kind == memory.KindBug { // recall always includes pinned do_not_repeat
				bugRecs = append(bugRecs, r.Record)
			}
		}
		fresh := make(map[string]bool, len(bugRecs))
		for _, r := range dedupSurfaced(ctx, a, ev.SessionID, bugRecs) {
			fresh[r.ID] = true
		}
		var bugLines []string
		for _, r := range set.Results {
			if r.Record.Kind != memory.KindBug || !fresh[r.Record.ID] {
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
			b.WriteString("Related bugs (search_bugs for detail):\n")
			b.WriteString(strings.Join(bugLines, "\n"))
		}
	}

	out := b.String()
	// Mark the pattern as injected so a later repeat in this session is deduped.
	// Only record when content was actually injected and the session is scoped,
	// so the reminder never fires for an empty first result.
	if out != "" && ev.SessionID != "" {
		_ = a.Ledger.Incr(ctx, scope, metric, 1)
	}
	return Result{Context: out}, nil
}

// handleStop finalizes the session row and returns an advisory reminder.
func handleStop(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if ev.SessionID != "" {
		now := a.Clock.Now().UnixMilli()
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
				now, ev.SessionID)
			return err
		}); err != nil {
			return Result{}, err
		}
	}
	return Result{SystemMessage: "recarto: before ending, record any new learnings (record_memory), bug fixes (record_bug), and preferences — e.g. if the user corrected your approach or stated a style/workflow choice. And if Recarto itself errored, ranked the wrong result first, confused you, or lacked a capability you needed this session, call report_recarto_issue so it can be fixed."}, nil
}
