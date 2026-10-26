package graph

import (
	"context"
	"strings"
)

// AnnotatedSymbol is one definition decorated with a queried attribute/annotation,
// e.g. a class carrying [ExtendObjectType] or a method carrying [GraphQLName].
type AnnotatedSymbol struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Path          string
	Line          int
	Attribute     string // the attribute name as written at the use site
}

// Annotated answers "find every symbol decorated with attribute X" — the
// structural form of the attribute/annotation grep that LLMs fall back to in
// C#/TS codebases ("find all [ExtendObjectType] classes", "every @Component").
// Plain symbol search can't answer it: the decorated class shares no name with
// the attribute, and a text grep also matches the attribute's own definition,
// doc comments, and string literals.
//
// The indexer records each attribute use as an `annotates` edge anchored inside
// the decorated declaration's span, so the edge's src_node is exactly the
// decorated symbol. Matching is suffix-tolerant: C# lets `[Obsolete]` stand for
// the `ObsoleteAttribute` class, so a query for either form matches the other.
// A lookup miss is an empty slice, not an error.
func (e *Engine) Annotated(ctx context.Context, worktree, attr string, limit int) ([]AnnotatedSymbol, error) {
	if limit <= 0 {
		limit = 50
	}
	// A use is recorded by its last name segment (@pytest.fixture as fixture,
	// #[tokio::main] as main), so the qualified form written at the site matches.
	attr = strings.TrimPrefix(strings.TrimSpace(attr), "@")
	attr = attr[strings.LastIndexAny(attr, ".:")+1:]
	if attr == "" {
		return nil, nil
	}
	// Normalise around the C# "Attribute" suffix convention so a query for the
	// short or long form finds uses written either way.
	bare := strings.TrimSuffix(attr, "Attribute")
	withSuffix := bare + "Attribute"

	const sql = `
SELECT DISTINCT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line, e.dst_name
  FROM edges e
  JOIN nodes n ON n.id = e.src_node_id
  JOIN files f ON f.id = n.file_id
 WHERE e.kind = 'annotates' AND f.worktree = ?
   AND (e.dst_name = ? OR e.dst_name = ? OR e.dst_name = ?)
 ORDER BY f.path, n.start_line LIMIT ?`

	rows, err := e.st.DB.QueryContext(ctx, sql, worktree, attr, bare, withSuffix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnnotatedSymbol
	for rows.Next() {
		var a AnnotatedSymbol
		if err := rows.Scan(&a.NodeID, &a.Name, &a.QualifiedName, &a.Kind, &a.Path, &a.Line, &a.Attribute); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
