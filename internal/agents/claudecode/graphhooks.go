package claudecode

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// editInput is the subset of PostToolUse(Edit|Write) we use.
type editInput struct {
	SessionID string `json:"session_id"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// PostEdit incrementally re-indexes the file the agent just edited, then resolves
// references. Exit-0 advisory: any error is returned for the caller to log, never
// to block. Unsupported files are skipped (IndexFile no-ops on them).
func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi editInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	abs := hi.ToolInput.FilePath
	if abs == "" {
		return nil
	}
	wt := a.Git.Worktree
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // outside the worktree; ignore
	}
	rel = filepath.ToSlash(rel)
	// Surface any prior bug tied to this file before re-indexing (best-effort).
	// This matches stored refs by path and does not require the file on disk.
	if found, berr := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil && len(found) > 0 {
		_ = writeHookContext(out, "PostToolUse", formatBugWarnings(rel, found))
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil // file vanished between edit and hook; ignore
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	if err := a.Graph.IndexFile(ctx, wt, rel, content, info.ModTime().UnixMilli(), info.Size()); err != nil {
		return err
	}
	return a.Graph.Resolve(ctx, wt)
}

// augmentInput is the subset of PostToolUse(Grep|Glob) we use.
type augmentInput struct {
	ToolInput struct {
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
	} `json:"tool_input"`
}

// Augment runs a code-graph search matching the agent's Grep/Glob pattern and
// also surfaces any kind=bug memories matching that pattern, injecting both as
// advisory additionalContext (non-blocking).
func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi augmentInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	pattern := hi.ToolInput.Pattern
	if pattern == "" {
		pattern = hi.ToolInput.Query
	}
	if pattern == "" {
		return nil
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

	if b.Len() == 0 {
		return nil
	}
	return writeHookContext(out, "PostToolUse", b.String())
}
