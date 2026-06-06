package graph

import (
	"context"
	"strings"
)

// maxReceiverLen caps the receiver disambiguator shown per hop. Receivers are
// hints ("called on `builder`"), not source to read — a multi-line lambda chain
// captured as a receiver (common in fluent builders) would otherwise dump
// kilobytes per hop. We collapse whitespace and truncate.
const maxReceiverLen = 48

// maxAmbiguousStarts bounds how many same-named definitions we aggregate when a
// bare (unqualified) symbol is traced. Output is already capped by fanout/budget;
// this just bounds the start-set SQL.
const maxAmbiguousStarts = 128

// compactReceiver renders a receiver expression as a single-line, length-capped
// disambiguator. Whitespace runs (incl. newlines) collapse to one space.
func compactReceiver(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxReceiverLen {
		s = string(r[:maxReceiverLen-1]) + "…"
	}
	return s
}

// CallNode is one hop in a call chain.
type CallNode struct {
	NodeID     string
	Name       string
	Path       string
	Line       int
	Resolution string // how the edge into this hop resolved: self|typed|static|iface|name ("" for root)
	Confidence string // high|medium|low ("" for root)
	Receiver   string // receiver expression for an ambiguous hop, else ""
}

// confLabel buckets a candidate confidence into a verbalizable band.
func confLabel(c float64) string {
	switch {
	case c >= 0.8:
		return "high"
	case c >= 0.4:
		return "medium"
	default:
		return "low"
	}
}

// TraceCallPath returns call chains starting at symbol. direction "callees"
// follows outgoing calls (what this calls); "callers" follows incoming calls
// (who calls this). Chains are bounded by depth and a per-node fan-out cap to
// avoid explosion. Edges resolved by dst_node_id are followed directly;
// unresolved edges fall back to matching dst_name to a node name.
func (e *Engine) TraceCallPath(ctx context.Context, worktree, symbol, direction string, depth int) ([][]CallNode, error) {
	chains, _, err := e.TraceCallPathFiltered(ctx, worktree, symbol, direction, depth, 0)
	return chains, err
}

// TraceCallPathFiltered is TraceCallPath with an explicit confidence floor:
// minConf 0 uses the adaptive default (each call site shows its high/medium
// candidates, or its low ones only when it has nothing better).
func (e *Engine) TraceCallPathFiltered(ctx context.Context, worktree, symbol, direction string, depth int, minConf float64) ([][]CallNode, []string, error) {
	if depth <= 0 {
		depth = 3
	}
	starts, err := e.nodesByName(ctx, worktree, symbol)
	if err != nil {
		return nil, nil, err
	}
	if len(starts) == 0 {
		// No exact match: offer lenient candidates (mirrors search_graph's hint)
		// instead of a silent empty result.
		var suggestions []string
		if res, serr := e.SearchGuided(ctx, worktree, symbol, []string{symbol}, "", "", 5); serr == nil {
			for _, r := range res {
				suggestions = append(suggestions, r.Name)
			}
		}
		return nil, suggestions, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	// Root frontier: a single match traces from that exact node (preserving
	// resolution precision for qualified names); multiple matches mean the bare
	// name is overloaded, so we aggregate the first hop across every definition
	// rather than arbitrarily picking one — "who calls X" must see every X.
	rootLabel := starts[0]
	rootIDs := make([]string, 0, len(starts))
	visited := map[string]bool{}
	for _, s := range starts {
		rootIDs = append(rootIDs, s.NodeID)
		visited[s.NodeID] = true
	}
	if len(starts) > 1 {
		rootLabel = CallNode{Name: symbol} // virtual: ambiguous, no single site
	}

	const traceNodeBudget = 40
	var chains [][]CallNode
	emitted := 0
	truncated := false
	// curIDs is the frontier: a single node for ordinary hops, or every
	// same-named definition for an aggregated ambiguous root.
	var walk func(curIDs []string, path []CallNode, visited map[string]bool, d int)
	walk = func(curIDs []string, path []CallNode, visited map[string]bool, d int) {
		if truncated {
			return
		}
		if d == 0 {
			chains = append(chains, path)
			return // depth limit reached; don't issue a needless neighbor query
		}
		next, err := e.callNeighbors(ctx, worktree, curIDs, direction, minConf)
		if err != nil || len(next) == 0 {
			chains = append(chains, path)
			return
		}
		for _, nb := range next {
			if visited[nb.NodeID] {
				continue
			}
			if emitted >= traceNodeBudget {
				truncated = true
				chains = append(chains, append(append([]CallNode{}, path...),
					CallNode{Name: "… (node budget reached; narrow the symbol or lower depth)"}))
				return
			}
			emitted++
			childPath := append(append([]CallNode{}, path...), nb)
			// Uncertainty is terminal: a low-confidence hop is reported as a leaf,
			// never expanded into a speculative cross-product.
			if nb.Confidence == "low" {
				chains = append(chains, childPath)
				continue
			}
			v2 := make(map[string]bool, len(visited)+1)
			for k := range visited {
				v2[k] = true
			}
			v2[nb.NodeID] = true
			walk([]string{nb.NodeID}, childPath, v2, d-1)
		}
	}
	walk(rootIDs, []CallNode{rootLabel}, visited, depth)
	return chains, nil, nil
}

// nodesByName resolves a trace start symbol to every matching definition. A
// qualified name (e.g. X.A.Handle) typically matches one node; a bare name
// (Handle) can match many overloads/definitions — all are returned (capped) so
// the caller can aggregate. Exact-qualified matches are preferred and returned
// alone, so a precise query never gets diluted by unrelated same-named nodes.
func (e *Engine) nodesByName(ctx context.Context, worktree, name string) ([]CallNode, error) {
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT n.id, n.name, f.path, n.start_line, (n.qualified_name = ?) AS exact
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ?)
 ORDER BY exact DESC, n.id
 LIMIT ?`, name, worktree, name, name, maxAmbiguousStarts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallNode
	var qualified bool
	for rows.Next() {
		var n CallNode
		var exact bool
		if err := rows.Scan(&n.NodeID, &n.Name, &n.Path, &n.Line, &exact); err != nil {
			return nil, err
		}
		if exact && !qualified {
			// First exact qualified-name hit: this is a precise query; discard any
			// looser same-bare-name matches and keep only the exact one(s).
			qualified = true
			out = out[:0]
		}
		if qualified && !exact {
			continue
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// callNeighbors returns next-hop nodes for cur in the given direction, reading
// the candidate set. Per call site (edge): if it has any candidate at/above the
// medium band keep those, else keep its low candidates (capped) — so a confident
// call yields its precise target and an ambiguous call yields its alternatives.
// minConf drops anything below the caller's floor. Capped by fanout.
func (e *Engine) callNeighbors(ctx context.Context, worktree string, curIDs []string, direction string, minConf float64) ([]CallNode, error) {
	const fanout = 25
	const lowCapPerEdge = 5
	if len(curIDs) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(curIDs)), ",")
	var q string
	if direction == "callers" {
		q = `
SELECT ed.id, n.id, n.name, f.path, n.start_line, ec.confidence, ec.method, ed.dst_recv
  FROM edge_candidates ec
  JOIN edges ed ON ed.id = ec.edge_id
  JOIN nodes n  ON n.id = ed.src_node_id
  JOIN files f  ON f.id = n.file_id
 WHERE ed.kind='calls' AND ec.dst_node_id IN (` + ph + `) AND f.worktree = ?
 ORDER BY ed.id, ec.confidence DESC, n.id`
	} else {
		q = `
SELECT ed.id, n.id, n.name, f.path, n.start_line, ec.confidence, ec.method, ed.dst_recv
  FROM edges ed
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes n ON n.id = ec.dst_node_id
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND ed.src_node_id IN (` + ph + `) AND f.worktree = ?
 ORDER BY ed.id, ec.confidence DESC, n.id`
	}
	args := make([]any, 0, len(curIDs)+1)
	for _, id := range curIDs {
		args = append(args, id)
	}
	args = append(args, worktree)
	rows, err := e.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type cand struct {
		node CallNode
		conf float64
	}
	byEdge := map[string][]cand{}
	var order []string
	for rows.Next() {
		var edgeID, recv, method string
		var n CallNode
		var conf float64
		if err := rows.Scan(&edgeID, &n.NodeID, &n.Name, &n.Path, &n.Line, &conf, &method, &recv); err != nil {
			return nil, err
		}
		n.Resolution, n.Confidence, n.Receiver = method, confLabel(conf), compactReceiver(recv)
		if _, seen := byEdge[edgeID]; !seen {
			order = append(order, edgeID)
		}
		byEdge[edgeID] = append(byEdge[edgeID], cand{n, conf})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []CallNode
	seen := map[string]bool{}
	for _, edgeID := range order {
		cs := byEdge[edgeID]
		hasBetter := cs[0].conf >= 0.4 // rows are confidence-desc within an edge
		kept := 0
		for _, c := range cs {
			if c.conf < minConf {
				continue
			}
			if hasBetter && c.conf < 0.4 {
				continue // per-site floor: drop low when a better candidate exists
			}
			if !hasBetter && kept >= lowCapPerEdge {
				break // cap the ambiguous candidate set
			}
			if seen[c.node.NodeID] {
				continue
			}
			seen[c.node.NodeID] = true
			out = append(out, c.node)
			kept++
			if len(out) >= fanout {
				return out, nil
			}
		}
	}
	return out, nil
}
