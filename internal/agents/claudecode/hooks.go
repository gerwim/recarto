package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// sessionStartBudget is the token budget for the session-start recall slice (spec §9.2).
const sessionStartBudget = 1500

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

	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		TokenBudget: sessionStartBudget,
	})
	if err != nil {
		return err
	}
	return writeHookContext(out, "SessionStart", formatSessionContext(results))
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

// formatSessionContext renders recalled memories plus a record nudge.
func formatSessionContext(results []memory.RecallResult) string {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")
	if len(results) == 0 {
		b.WriteString("(no prior memories yet)\n")
	} else {
		for _, r := range results {
			title := r.Record.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", r.Record.Kind, title)
			if r.Record.Body != "" {
				fmt.Fprintf(&b, "  %s\n", r.Record.Body)
			}
		}
	}
	b.WriteString("\nRecord new learnings with the record_memory tool and bug fixes with record_bug.")
	return b.String()
}

// nullable maps "" to a SQL NULL so repo-global rows store NULL, not "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
