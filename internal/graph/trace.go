package graph

import (
	"context"
	"database/sql"
	"errors"
)

// CallNode is one hop in a call chain.
type CallNode struct {
	NodeID string
	Name   string
	Path   string
	Line   int
}

// TraceCallPath returns call chains starting at symbol. direction "callees"
// follows outgoing calls (what this calls); "callers" follows incoming calls
// (who calls this). Chains are bounded by depth and a per-node fan-out cap to
// avoid explosion. Edges resolved by dst_node_id are followed directly;
// unresolved edges fall back to matching dst_name to a node name.
func (e *Engine) TraceCallPath(ctx context.Context, worktree, symbol, direction string, depth int) ([][]CallNode, error) {
	if depth <= 0 {
		depth = 3
	}
	start, err := e.nodeByName(ctx, worktree, symbol)
	if err != nil {
		return nil, err
	}
	if start.NodeID == "" {
		return nil, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	var chains [][]CallNode
	var walk func(cur CallNode, path []CallNode, visited map[string]bool, d int)
	walk = func(cur CallNode, path []CallNode, visited map[string]bool, d int) {
		if d == 0 {
			chains = append(chains, path)
			return // depth limit reached; don't issue a needless neighbor query
		}
		next, err := e.callNeighbors(ctx, worktree, cur, direction)
		if err != nil || len(next) == 0 {
			chains = append(chains, path)
			return
		}
		for _, nb := range next {
			if visited[nb.NodeID] {
				continue
			}
			v2 := make(map[string]bool, len(visited)+1)
			for k := range visited {
				v2[k] = true
			}
			v2[nb.NodeID] = true
			walk(nb, append(append([]CallNode{}, path...), nb), v2, d-1)
		}
	}
	walk(start, []CallNode{start}, map[string]bool{start.NodeID: true}, depth)
	return chains, nil
}

func (e *Engine) nodeByName(ctx context.Context, worktree, name string) (CallNode, error) {
	var n CallNode
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.id, n.name, f.path, n.start_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ?) LIMIT 1`,
		worktree, name, name).Scan(&n.NodeID, &n.Name, &n.Path, &n.Line)
	if errors.Is(err, sql.ErrNoRows) {
		return CallNode{}, nil
	}
	return n, err
}

// callNeighbors returns the next hop nodes for cur in the given direction, capped.
func (e *Engine) callNeighbors(ctx context.Context, worktree string, cur CallNode, direction string) ([]CallNode, error) {
	const fanout = 25
	var q string
	if direction == "callers" {
		q = `
SELECT DISTINCT n.id, n.name, f.path, n.start_line
  FROM edges ed
  JOIN nodes n ON n.id = ed.src_node_id
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND f.worktree = ?
   AND (ed.dst_node_id = ? OR ed.dst_name = ?)
 LIMIT ?`
	} else {
		q = `
SELECT DISTINCT n.id, n.name, f.path, n.start_line
  FROM edges ed
  JOIN nodes n ON (n.id = ed.dst_node_id OR (ed.dst_node_id IS NULL AND n.name = ed.dst_name))
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND ed.src_node_id = ? AND f.worktree = ?
 LIMIT ?`
	}
	var rows *sql.Rows
	var err error
	if direction == "callers" {
		rows, err = e.st.DB.QueryContext(ctx, q, worktree, cur.NodeID, cur.Name, fanout)
	} else {
		rows, err = e.st.DB.QueryContext(ctx, q, cur.NodeID, worktree, fanout)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallNode
	for rows.Next() {
		var n CallNode
		if err := rows.Scan(&n.NodeID, &n.Name, &n.Path, &n.Line); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
