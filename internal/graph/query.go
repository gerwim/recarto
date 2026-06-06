package graph

import (
	"context"
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
	b.WriteString(" ORDER BY n.qualified_name LIMIT ?")
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
