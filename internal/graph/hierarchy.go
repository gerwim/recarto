package graph

import (
	"context"
)

// TypeScript stores clauses separately so an upward lookup can distinguish them.
const typeEdgeKinds = `('extends', 'implements')`

// TypeRelation is one type related to a query type through an `extends` or
// `implements` edge — either a subtype or supertype. Relation is always
// "implements" for subtypes and is the edge kind for supertypes.
type TypeRelation struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Path          string
	Line          int
	Relation      string // "implements" (down) | edge kind (up)
}

// Implementations answers "what implements/subclasses X" (direction "down",
// default) and "what are X's supertypes" (direction "up") over type edges. C#
// `class C : Base, IFoo` stores both clauses as `extends`; TypeScript
// `class C extends Base implements IFoo` stores one of each. This is the
// structural answer to "find every implementation of IRequestExecutor" that
// plain symbol search cannot give, since implementers don't share the interface's
// name.
//
// The query type is matched by bare OR qualified name. Down-edges are matched on
// the edge's recorded dst_name too (not only the resolved dst_node_id), so the
// ~10% of inheritance edges that never resolved to a node are still returned. A
// lookup miss is an empty slice, not an error.
func (e *Engine) Implementations(ctx context.Context, worktree, symbol, direction string, limit int) ([]TypeRelation, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	symbol = ReferenceName(symbol)
	if symbol == "" {
		return nil, nil
	}
	var sql string
	if direction == "up" {
		// Supertypes of the named type: walk from the matched source node to
		// each edge's target. The target may be unresolved, so fall back to the
		// edge's dst_name and report it even without a node row.
		// The kind separates clauses that resolve to one target and orders ties
		// after name, path, and line.
		sql = `
SELECT DISTINCT COALESCE(dn.id, ''), COALESCE(dn.name, e.dst_name),
       COALESCE(dn.qualified_name, ''), COALESCE(dn.kind, ''),
       COALESCE(df.path, ''), COALESCE(dn.start_line, 0), e.kind
  FROM edges e
  JOIN nodes sn ON sn.id = e.src_node_id
  JOIN files sf ON sf.id = sn.file_id
  LEFT JOIN nodes dn ON dn.id = e.dst_node_id
  LEFT JOIN files df ON df.id = dn.file_id
 WHERE e.kind IN ` + typeEdgeKinds + ` AND sf.worktree = ?
    AND (sn.name = ? OR sn.qualified_name = ?)
  ORDER BY 2, 5, 6, 7 LIMIT ?`
	} else {
		// Subtypes/implementers of the named type match by resolved node or name.
		sql = `
SELECT DISTINCT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line, 'implements'
  FROM edges e
  JOIN nodes n ON n.id = e.src_node_id
  JOIN files f ON f.id = n.file_id
 WHERE e.kind IN ` + typeEdgeKinds + ` AND f.worktree = ?
   AND ( e.dst_name = ?
         OR e.dst_node_id IN (
              SELECT id FROM nodes
               WHERE name = ? OR qualified_name = ?) )
 ORDER BY n.qualified_name, f.path, n.start_line LIMIT ?`
	}

	var args []any
	if direction == "up" {
		args = []any{worktree, symbol, symbol, limit}
	} else {
		args = []any{worktree, symbol, symbol, symbol, limit}
	}
	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TypeRelation
	for rows.Next() {
		var r TypeRelation
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind, &r.Path, &r.Line, &r.Relation); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
