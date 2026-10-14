package graph

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Reference is one place a symbol is used: a graph edge targeting it (kind
// calls|imports|extends|implements|annotates|references) or, for what the graph
// cannot see (comments, config, dynamically-bound sites, unparsed files), a
// word-boundary text match (kind "text"). Edge references carry the enclosing
// source symbol; text references carry the matching line.
type Reference struct {
	Path   string
	Line   int
	Kind   string
	From   string // qualified name of the referencing symbol, "" at file scope
	Text   string // matching line (text refs only)
	IsTest bool
}

// ReferencesMeta reports whether the symbol matched any graph node (Resolved)
// and whether the combined list was capped (Truncated).
type ReferencesMeta struct {
	Resolved  bool
	Truncated bool
}

// References answers "where is X used" in one call. Graph edges come first —
// matched by resolved dst node or by recorded dst_name, mirroring
// Implementations, so unresolved edges are still found — then a word-boundary
// text sweep over the worktree, deduplicated against the edge hits by
// path:line. A dotted symbol matches as a qualified-name suffix (the trace
// identifier form); the text sweep uses the leaf name. limit caps the combined
// list (default 50).
//
// NOTE: bare-name queries on common method names (Get, Close, String) can
// match unrelated same-named call sites among unresolved edges — pass a
// Type.Method form to narrow.
func (e *Engine) References(ctx context.Context, worktree, symbol string, limit int) ([]Reference, ReferencesMeta, error) {
	var meta ReferencesMeta
	if limit <= 0 {
		limit = 50
	}
	symbol = canonicalSymbol(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, meta, nil
	}
	leaf := symbol
	var recv string // non-empty only when input is dotted
	if i := lastDot(symbol); i >= 0 {
		leaf = symbol[i+1:]
		// receiver is the segment immediately before the leaf
		prefix := symbol[:i]
		if j := lastDot(prefix); j >= 0 {
			recv = prefix[j+1:]
		} else {
			recv = prefix
		}
	}
	suffix := "\x00" // sentinel: never matches when input has no dot (see SnippetByName)
	if strings.Contains(symbol, ".") {
		suffix = "%." + symbol
	}

	var nodeCount int
	if err := e.st.DB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?)`,
		worktree, symbol, symbol, suffix).Scan(&nodeCount); err != nil {
		return nil, meta, err
	}
	meta.Resolved = nodeCount > 0

	// Build the edge query as a UNION of two index-friendly arms so SQLite can
	// use idx_edges_dstname and idx_edges_dst rather than a full scan via
	// idx_edges_file. UNION (not UNION ALL) also collapses byte-identical rows
	// that would otherwise consume limit slots.
	//
	// When the input is dotted (e.g. "Cache.Get") we constrain the dst_name
	// arm with dst_recv = receiver so that unresolved calls on unrelated
	// receivers (x.Get where x is not Cache) are excluded. The resolved arm
	// (dst_node_id IN …) is receiver-agnostic because the node lookup already
	// identifies the exact symbol.
	const edgeSelect = `
SELECT f.path AS path, e2.line AS line, e2.kind AS kind, COALESCE(sn.qualified_name,'') AS fromq
  FROM edges e2
  JOIN files f ON f.id = e2.file_id
  LEFT JOIN nodes sn ON sn.id = e2.src_node_id`

	var (
		query string
		args  []any
	)
	if recv != "" {
		// dotted input: narrow dst_name arm by receiver
		query = `SELECT * FROM (` + edgeSelect + `
 WHERE f.worktree = ? AND e2.dst_name = ? AND e2.dst_recv = ?
UNION` + edgeSelect + `
 WHERE f.worktree = ? AND e2.dst_node_id IN (
       SELECT n.id FROM nodes n JOIN files f2 ON f2.id = n.file_id
        WHERE f2.worktree = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?))
) ORDER BY path, line LIMIT ?`
		args = []any{worktree, leaf, recv, worktree, worktree, symbol, symbol, suffix, limit + 1}
	} else {
		// bare input: breadth — match any unresolved call by name
		query = `SELECT * FROM (` + edgeSelect + `
 WHERE f.worktree = ? AND e2.dst_name = ?
UNION` + edgeSelect + `
 WHERE f.worktree = ? AND e2.dst_node_id IN (
       SELECT n.id FROM nodes n JOIN files f2 ON f2.id = n.file_id
        WHERE f2.worktree = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?))
) ORDER BY path, line LIMIT ?`
		args = []any{worktree, leaf, worktree, worktree, symbol, symbol, suffix, limit + 1}
	}

	rows, err := e.st.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, meta, err
	}
	var refs []Reference
	seen := map[string]bool{}
	for rows.Next() {
		var r Reference
		if err := rows.Scan(&r.Path, &r.Line, &r.Kind, &r.From); err != nil {
			rows.Close()
			return nil, meta, err
		}
		r.IsTest = isTestPath(r.Path)
		refs = append(refs, r)
		seen[fmt.Sprintf("%s:%d", r.Path, r.Line)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, meta, err
	}

	if len(refs) > limit {
		// Edge query returned more than limit: cap and skip sweep.
		refs = refs[:limit]
		meta.Truncated = true
	} else {
		// Always run the text sweep — even when edge hits == limit — so that
		// unseen text refs are discovered and Truncated is set honestly.
		pattern := `\b` + regexp.QuoteMeta(leaf) + `\b`
		matches, _, err := e.SearchContentWithMeta(ctx, worktree, pattern, ContentSearchOpts{
			Regex: true, Limit: limit + len(seen), MaxPerFile: 10,
		})
		if err != nil {
			return nil, meta, err
		}
		for _, m := range matches {
			if seen[fmt.Sprintf("%s:%d", m.Path, m.Line)] {
				continue
			}
			if len(refs) < limit {
				refs = append(refs, Reference{
					Path: m.Path, Line: m.Line, Kind: "text",
					From: m.Symbol, Text: m.Text, IsTest: isTestPath(m.Path),
				})
				seen[fmt.Sprintf("%s:%d", m.Path, m.Line)] = true
			} else {
				meta.Truncated = true
				break
			}
		}
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return refs, meta, nil
}
