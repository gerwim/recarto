package claudecode

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
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
func PostEdit(ctx context.Context, a *app.App, in io.Reader, _ io.Writer) error {
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
// injects the structured result as advisory additionalContext (non-blocking).
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
	res, err := a.Graph.Search(ctx, a.Git.Worktree, pattern, "", "", 8)
	if err != nil || len(res) == 0 {
		return err
	}
	var b strings.Builder
	b.WriteString("Recarto code graph — symbols matching your search:\n")
	for _, r := range res {
		fmt.Fprintf(&b, "- [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
	}
	b.WriteString("Use get_code_snippet to read one symbol instead of the whole file.")
	return writeHookContext(out, "PostToolUse", b.String())
}
