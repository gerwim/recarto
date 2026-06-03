package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Session-start tier sizing. sessionStartBudget is the default token budget for
// the injected block (override via RECARTO_SESSION_BUDGET). The map/bug tiers are
// bounded by line caps; cerebrumReserve keeps headroom for them.
const (
	sessionStartBudget = 1500
	recentBugLimit     = 5
	archModuleLimit    = 8
	archDepLimit       = 8
	cerebrumReserve    = 200
	cerebrumBodyMax    = 160
)

// sessionBudget is the session-start token budget: RECARTO_SESSION_BUDGET if set
// to a positive integer, else the default.
func sessionBudget() int {
	if v := os.Getenv("RECARTO_SESSION_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return sessionStartBudget
}

// sessionStartInput is the subset of the Claude Code SessionStart payload we use.
type sessionStartInput struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

// SessionStart opens/creates the session row, recalls a token-budgeted slice for
// this branch, and emits it as additionalContext plus a record nudge. It always
// exits 0 (the caller treats any returned error as non-blocking).
func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi sessionStartInput
	if err := decode(in, &hi); err != nil {
		return err
	}

	now := a.Clock.Now().UnixMilli()
	if hi.SessionID != "" {
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO sessions(id, agent, branch, worktree, started_at)
				 VALUES (?, 'claude-code', ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`,
				hi.SessionID, nullable(a.Git.Branch), a.Git.Worktree, now)
			return err
		}); err != nil {
			return err
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
		return err
	}
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	arch, _ := a.Graph.Architecture(ctx, a.Git.Worktree, "")
	bugs, _ := a.Engine.RecentBugs(ctx, a.Git.Branch, recentBugLimit)
	return writeHookContext(out, "SessionStart", formatSessionContext(results, arch, bugs, budget))
}

// postReadInput is the subset of the Claude Code PostToolUse(Read) payload we use.
type postReadInput struct {
	SessionID string `json:"session_id"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
	ToolResult json.RawMessage `json:"tool_result"`
}

// PostRead logs the read to the ledger and warns (non-blocking) when the same file
// has already been read this session, surfacing the tokens that re-read would cost.
func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi postReadInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	file := hi.ToolInput.FilePath
	if file == "" {
		return nil
	}
	scope := "session:" + hi.SessionID
	tokens := int64(ledger.EstimateTokens(string(hi.ToolResult)))

	_ = a.Ledger.Incr(ctx, "lifetime", "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "read:"+file, 1)

	n, err := a.Ledger.Get(ctx, scope, "read:"+file)
	if err != nil {
		return err
	}
	if n <= 1 {
		return nil // first read this session: nothing to warn about
	}
	_ = a.Ledger.Incr(ctx, "lifetime", "repeated_reads_blocked", 1)
	msg := fmt.Sprintf("You have already read %s in this session (%d times, ~%d tokens each). "+
		"Rely on the earlier read instead of reading it again.", file, n, tokens)
	return writeHookContext(out, "PostToolUse", msg)
}

// stopInput is the subset of the Claude Code Stop payload we use.
type stopInput struct {
	SessionID      string `json:"session_id"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// Stop finalizes the session row and emits an advisory reminder. It never blocks
// (spec §13: Stop is exit-0 advisory in Phase 1).
func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi stopInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	if hi.SessionID != "" {
		now := a.Clock.Now().UnixMilli()
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
				now, hi.SessionID)
			return err
		}); err != nil {
			return err
		}
	}
	payload := map[string]any{
		"systemMessage": "recarto: record any new learnings (record_memory) and bug fixes (record_bug) before ending.",
	}
	return json.NewEncoder(out).Encode(payload)
}

// decode reads a single JSON object from r, tolerating an empty body.
func decode(r io.Reader, v any) error {
	if err := json.NewDecoder(r).Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// writeHookContext emits the Claude Code hookSpecificOutput.additionalContext shape.
func writeHookContext(out io.Writer, event, additionalContext string) error {
	payload := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": additionalContext,
		},
	}
	return json.NewEncoder(out).Encode(payload)
}

// formatSessionContext renders the tiered session-start block: pinned do_not_repeat
// (always, full body), cerebrum memories (title + trimmed body, degrading to
// titles-only then dropping under budget pressure), a bounded module map, and a
// titles-only recent-bugs tier — followed by the record nudge.
func formatSessionContext(results []memory.RecallResult, arch graph.Architecture, bugs []*memory.Record, budget int) string {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")

	budgetCap := budget - cerebrumReserve
	if budgetCap < 0 {
		budgetCap = budget
	}
	used := 0
	wroteAny := false
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		if r.Record.Kind == memory.KindDoNotRepeat && r.Record.Pinned {
			line := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, r.Record.Body)
			b.WriteString(line)
			used += ledger.EstimateTokens(line)
			wroteAny = true
			continue
		}
		fullDisplay := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, trimOneLine(r.Record.Body))
		fullCost := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, r.Record.Body)
		titleOnly := fmt.Sprintf("- [%s] %s\n", r.Record.Kind, title)
		switch {
		case used+ledger.EstimateTokens(fullCost) <= budgetCap:
			b.WriteString(fullDisplay)
			used += ledger.EstimateTokens(fullDisplay)
			wroteAny = true
		case used+ledger.EstimateTokens(titleOnly) <= budgetCap:
			b.WriteString(titleOnly)
			used += ledger.EstimateTokens(titleOnly)
			wroteAny = true
		}
	}
	if !wroteAny {
		b.WriteString("(no prior memories yet)\n")
	}

	if len(arch.Modules) > 0 {
		b.WriteString("\nCode map (modules):\n")
		for i, m := range arch.Modules {
			if i >= archModuleLimit {
				break
			}
			fmt.Fprintf(&b, "- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for i, d := range arch.Deps {
			if i >= archDepLimit {
				break
			}
			fmt.Fprintf(&b, "  %s → %s (%d)\n", d.From, d.To, d.Count)
		}
		b.WriteString("Query the code graph (search/snippet/callers) instead of reading whole files; grep/glob auto-injects matching symbols.\n")
	}

	if len(bugs) > 0 {
		b.WriteString("\nRecent bugs (search_bugs for detail):\n")
		for _, r := range bugs {
			t := r.Title
			if t == "" {
				t = "(untitled)"
			}
			fmt.Fprintf(&b, "- %s\n", t)
		}
	}

	b.WriteString("\nRecord new learnings with the record_memory tool and bug fixes with record_bug.")
	return b.String()
}

// trimOneLine collapses a memory body to its first line, capped at cerebrumBodyMax
// runes, for the budget-conscious cerebrum tier.
func trimOneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > cerebrumBodyMax {
		s = s[:cerebrumBodyMax] + "…"
	}
	return s
}

// nullable maps "" to a SQL NULL so repo-global rows store NULL, not "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
