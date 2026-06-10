package graph

import (
	"context"
	"fmt"
	"strings"
)

// EdgeFilter restricts the result to nodes participating in an edge of Kind in
// Direction ("out" = node is the edge source, "in" = node is the edge target).
type EdgeFilter struct {
	Kind      string
	Direction string // "out" | "in"
}

// QuerySpec is the constrained structured query for query_graph (NOT raw SQL).
type QuerySpec struct {
	Kind     string
	NameLike string // substring match on name/qualified_name
	Path     string // exact file path
	Language string
	Edge     *EdgeFilter
	Limit    int
}

// IsEmpty reports whether the spec carries no selective constraint — no name,
// kind, path, language, or edge filter (a bare Limit does not count, since it
// still selects the whole graph). Such a query returns the first page of every
// node ordered by qualified name, which on a real repo is whatever sorts first —
// typically vendored/generated symbols (e.g. yarn .cjs `$` functions) — never an
// answer to a question. Both query surfaces reject it rather than burn tokens.
func (s QuerySpec) IsEmpty() bool {
	return s.Kind == "" && s.NameLike == "" && s.Path == "" &&
		s.Language == "" && (s.Edge == nil || s.Edge.Kind == "")
}

// QueryNode is one node in a query_graph result.
type QueryNode struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Path          string
	Signature     string
	Line          int
}

// Query runs a constrained structured query over the worktree's graph.
func (e *Engine) Query(ctx context.Context, worktree string, spec QuerySpec) ([]QueryNode, error) {
	if spec.IsEmpty() {
		return nil, fmt.Errorf("query needs at least one filter (name, kind, language, path, or edge); listing the whole graph is never the answer — use search for concepts, or query with a filter")
	}
	limit := spec.Limit
	if limit <= 0 {
		limit = 50
	}
	var b strings.Builder
	b.WriteString(`
SELECT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line, n.signature
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?`)
	args := []any{worktree}
	if spec.Kind != "" {
		b.WriteString(" AND n.kind = ?")
		args = append(args, spec.Kind)
	}
	if spec.NameLike != "" {
		b.WriteString(" AND (n.name LIKE ? OR n.qualified_name LIKE ?)")
		like := "%" + spec.NameLike + "%"
		args = append(args, like, like)
	}
	if spec.Path != "" {
		b.WriteString(" AND f.path = ?")
		args = append(args, spec.Path)
	}
	if spec.Language != "" {
		b.WriteString(" AND f.language = ?")
		args = append(args, spec.Language)
	}
	if spec.Edge != nil && spec.Edge.Kind != "" {
		if spec.Edge.Direction == "in" {
			b.WriteString(` AND n.id IN (SELECT dst_node_id FROM edges WHERE kind = ? AND dst_node_id IS NOT NULL)`)
		} else {
			b.WriteString(` AND n.id IN (SELECT src_node_id FROM edges WHERE kind = ? AND src_node_id IS NOT NULL)`)
		}
		args = append(args, spec.Edge.Kind)
	}
	// path+line break qualified-name ties (partial classes) so the page is
	// stable across reindexes instead of following insertion order.
	b.WriteString(" ORDER BY n.qualified_name, f.path, n.start_line LIMIT ?")
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryNode
	for rows.Next() {
		var n QueryNode
		if err := rows.Scan(&n.NodeID, &n.Name, &n.QualifiedName, &n.Kind, &n.Path, &n.Line, &n.Signature); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
