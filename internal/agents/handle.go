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

	// Opportunistic memory compaction (best-effort, gated to once per interval,
	// single-flight across worktrees). Runs before recall so this session's
	// recalls stamp the freshly-advanced epoch. Never blocks the agent.
	_, _ = a.Engine.Compact(ctx, memory.CompactOptions{})

	budget := sessionBudget()
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		Baseline:    true,
		TokenBudget: budget,
	})
	if err != nil {
		return Result{}, err
	}
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	arch, _ := a.Graph.Architecture(ctx, a.Git.Worktree, "")
	bugs, _ := a.Engine.RecentBugs(ctx, a.Git.Branch, recentBugLimit)
	return Result{Context: formatSessionContext(results, arch, bugs, budget)}, nil
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
	if found, err := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil && len(found) > 0 {
		msgs = append(msgs, formatBugWarnings(rel, found))
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
	// Surface any prior bug tied to this file before re-indexing (best-effort).
	// This matches stored refs by path and does not require the file on disk.
	if found, berr := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil && len(found) > 0 {
		res.Context = formatBugWarnings(rel, found)
	}
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

	var b strings.Builder
	if res, err := a.Graph.Search(ctx, a.Git.Worktree, pattern, "", "", 8); err == nil && len(res) > 0 {
		b.WriteString("Recarto code graph — symbols matching your search:\n")
		for _, r := range res {
			fmt.Fprintf(&b, "- [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
		}
		b.WriteString("Use get_code_snippet to read one symbol instead of the whole file.")
	}

	if bugRes, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: pattern, Kind: memory.KindBug, Branch: a.Git.Branch, Limit: 5,
	}); err == nil {
		var bugLines []string
		for _, r := range bugRes {
			if r.Record.Kind != memory.KindBug { // recall always includes pinned do_not_repeat
				continue
			}
			if fix, _ := r.Record.Extra["fix"].(string); fix != "" {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s — fix: %s", r.Record.Title, fix))
			} else {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s", r.Record.Title))
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

	return Result{Context: b.String()}, nil
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
	return Result{SystemMessage: "recarto: record any new learnings (record_memory) and bug fixes (record_bug) before ending."}, nil
}
