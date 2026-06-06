package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

// SnippetResult is the structured result of Snippet. A miss is Found=false (not
// an error); only genuine faults (stale byte range, vanished file) return an error.
type SnippetResult struct {
	Found         bool
	StartLine     int
	EndLine       int
	QualifiedName string
	Snippet       string
}

// Snippet returns the exact source for a symbol (by name or qualified_name) in a
// file, using its stored byte range. It records the tokens saved vs. reading the
// whole file into the ledger.
func (e *Engine) Snippet(ctx context.Context, worktree, path, symbol string) (SnippetResult, error) {
	var sb, eb int64
	var sl, el int
	var qn string
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte, n.start_line, n.end_line, n.qualified_name
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND (n.name = ? OR n.qualified_name = ?)
 LIMIT 1`, worktree, path, symbol, symbol).Scan(&sb, &eb, &sl, &el, &qn)
	if errors.Is(err, sql.ErrNoRows) {
		return SnippetResult{Found: false}, nil
	}
	if err != nil {
		return SnippetResult{}, err
	}
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if err != nil {
		return SnippetResult{}, err
	}
	if sb < 0 || eb > int64(len(content)) || sb > eb {
		return SnippetResult{}, fmt.Errorf("graph: stale byte range for %q (reindex needed)", symbol)
	}
	snippet := string(content[sb:eb])

	saved := ledger.EstimateTokens(string(content)) - ledger.EstimateTokens(snippet)
	if saved > 0 {
		_ = e.led.Incr(ctx, "lifetime", "graph_tokens_saved", int64(saved))
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return SnippetResult{Found: true, StartLine: sl, EndLine: el, QualifiedName: qn, Snippet: snippet}, nil
}
