package graph

import (
	"context"
	"strings"
)

// SearchResult is one symbol match.
type SearchResult struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Language      string
	Path          string
	Line          int
	Score         float64 // higher = more relevant
}

// ftsQuery turns a free-text query into a safe FTS5 MATCH expression: each
// whitespace-separated term is double-quoted (so identifiers with punctuation or
// FTS operators can't break the query) and ANDed together.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return `""`
	}
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

// Search returns symbols matching query (FTS over name/qualified_name), scoped to
// worktree, optionally filtered by kind and language, ranked by bm25.
func (e *Engine) Search(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	args := []any{ftsQuery(query), worktree}
	sql := `
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line,
       bm25(nodes_fts) AS score
  FROM nodes_fts
  JOIN nodes n ON n.id = nodes_fts.node_id
  JOIN files f ON f.id = n.file_id
 WHERE nodes_fts MATCH ? AND f.worktree = ?`
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	sql += " ORDER BY score ASC LIMIT ?" // bm25 is negative: more negative = better
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var bm float64
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line, &bm); err != nil {
			return nil, err
		}
		r.Score = -bm // flip sign so higher = better
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
