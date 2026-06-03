package parse

import (
	"fmt"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Symbol is a definition (function, method, class, …) with its byte/line span.
type Symbol struct {
	Kind          string
	Name          string
	QualifiedName string
	StartByte     uint
	EndByte       uint
	StartLine     uint // 1-based
	EndLine       uint // 1-based
}

// Edge is a reference (call/import/extends/implements) anchored at a byte offset
// so the graph engine can attribute it to its enclosing definition.
type Edge struct {
	Kind      string // calls | imports | extends | implements | references
	DstName   string
	StartByte uint
	Line      uint // 1-based
}

// Result is the extracted graph for one file.
type Result struct {
	Symbols []Symbol
	Edges   []Edge
}

// edgeKind maps a reference.* capture suffix to a graph edge kind.
func edgeKind(suffix string) string {
	switch suffix {
	case "call":
		return "calls"
	case "import":
		return "imports"
	case "extends":
		return "extends"
	case "implements":
		return "implements"
	default:
		return "references"
	}
}

// ParseFile parses src as lang and extracts symbols/edges using the language's
// compiled query. All C-allocating objects are closed before returning.
func (r *Registry) ParseFile(lang, path string, src []byte) (Result, error) {
	l, ok := r.langs[lang]
	if !ok {
		return Result{}, fmt.Errorf("parse: unsupported language %q", lang)
	}
	q := r.queries[lang]

	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(l)
	tree := p.Parse(src, nil)
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()

	captureNames := q.CaptureNames()
	var res Result
	matches := cur.Matches(q, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var name string
		var defKind, refKind string
		var sb, eb, sl, el uint
		for _, c := range m.Captures {
			cn := captureNames[c.Index]
			switch {
			case cn == "name":
				name = strings.Trim(c.Node.Utf8Text(src), `"`)
			case strings.HasPrefix(cn, "definition."):
				defKind = strings.TrimPrefix(cn, "definition.")
				sb, eb = c.Node.StartByte(), c.Node.EndByte()
				sl, el = c.Node.StartPosition().Row+1, c.Node.EndPosition().Row+1
			case strings.HasPrefix(cn, "reference."):
				refKind = strings.TrimPrefix(cn, "reference.")
				sb = c.Node.StartByte()
				sl = c.Node.StartPosition().Row + 1
			}
		}
		switch {
		case defKind != "" && name != "":
			res.Symbols = append(res.Symbols, Symbol{
				Kind: defKind, Name: name, QualifiedName: name,
				StartByte: sb, EndByte: eb, StartLine: sl, EndLine: el,
			})
		case refKind != "" && name != "":
			res.Edges = append(res.Edges, Edge{
				Kind: edgeKind(refKind), DstName: name, StartByte: sb, Line: sl,
			})
		}
	}
	return res, nil
}
