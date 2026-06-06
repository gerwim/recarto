package graph

import (
	"context"
	"strings"
)

// TypeRelation is one type related to a query type through an `extends` edge —
// either a subtype (a class/struct that implements or inherits the query type) or
// a supertype (a base/interface the query type extends).
type TypeRelation struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Path          string
	Line          int
	Relation      string // "implements" (down) | "extends" (up)
}

// Implementations answers "what implements/subclasses X" (direction "down",
// default) and "what does X extend" (direction "up") over the `extends` edges the
// indexer already records for `class C : Base, IFoo`. It is the structural answer
// to the most common interface/inheritance grep — "find every implementation of
// IRequestExecutor" — that plain symbol search cannot give, since implementers
// don't share the interface's name.
//
// The query type is matched by bare OR qualified name. Down-edges are matched on
// the edge's recorded dst_name too (not only the resolved dst_node_id), so the
// ~10% of inheritance edges that never resolved to a node are still returned. A
// lookup miss is an empty slice, not an error.
func (e *Engine) Implementations(ctx context.Context, worktree, symbol, direction string, limit int) ([]TypeRelation, error) {
	if limit <= 0 {
		limit = 50
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, nil
	}
	var sql string
	if direction == "up" {
		// Supertypes the named type extends: walk from the matched source node to
		// each edge's target. The target may be unresolved, so fall back to the
		// edge's dst_name and report it even without a node row.
		sql = `
SELECT DISTINCT COALESCE(dn.id, ''), COALESCE(dn.name, e.dst_name),
       COALESCE(dn.qualified_name, ''), COALESCE(dn.kind, ''),
       COALESCE(df.path, ''), COALESCE(dn.start_line, 0)
  FROM edges e
  JOIN nodes sn ON sn.id = e.src_node_id
  JOIN files sf ON sf.id = sn.file_id
  LEFT JOIN nodes dn ON dn.id = e.dst_node_id
  LEFT JOIN files df ON df.id = dn.file_id
 WHERE e.kind = 'extends' AND sf.worktree = ?
   AND (sn.name = ? OR sn.qualified_name = ?)
 ORDER BY 2 LIMIT ?`
	} else {
		// Subtypes/implementers of the named type: every source node whose extends
		// edge targets the type, matched by resolved node or by recorded name.
		sql = `
SELECT DISTINCT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line
  FROM edges e
  JOIN nodes n ON n.id = e.src_node_id
  JOIN files f ON f.id = n.file_id
 WHERE e.kind = 'extends' AND f.worktree = ?
   AND ( e.dst_name = ?
         OR e.dst_node_id IN (
              SELECT id FROM nodes
               WHERE name = ? OR qualified_name = ?) )
 ORDER BY n.qualified_name LIMIT ?`
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
	rel := "implements"
	if direction == "up" {
		rel = "extends"
	}
	var out []TypeRelation
	for rows.Next() {
		var r TypeRelation
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind, &r.Path, &r.Line); err != nil {
			return nil, err
		}
		r.Relation = rel
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
