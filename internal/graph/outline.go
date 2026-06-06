package graph

import (
	"context"
)

// OutlineSymbol is one symbol defined in a file. The set of them gives an agent a
// cheap structural overview — kinds, names, signatures, line ranges — so it can
// understand a file's shape without reading the whole thing, then drill into one
// symbol with Snippet.
type OutlineSymbol struct {
	Kind          string
	Name          string
	QualifiedName string
	ParentName    string
	Signature     string
	StartLine     int
	EndLine       int
}

// Outline lists the symbols defined in a single file, ordered by source position.
// A missing/unindexed file yields an empty slice (not an error), matching the
// "miss is empty, not failure" convention of the other graph queries.
func (e *Engine) Outline(ctx context.Context, worktree, path string) ([]OutlineSymbol, error) {
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT n.kind, n.name, n.qualified_name, n.parent_name, n.signature, n.start_line, n.end_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ?
 ORDER BY n.start_line, n.end_line`, worktree, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutlineSymbol
	for rows.Next() {
		var s OutlineSymbol
		if err := rows.Scan(&s.Kind, &s.Name, &s.QualifiedName, &s.ParentName, &s.Signature, &s.StartLine, &s.EndLine); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
