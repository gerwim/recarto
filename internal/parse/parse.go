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
	RecvType      string // method receiver type, "" unless a Go method (e.g. "Saga")
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
	Recv      string // receiver expression text for a member/selector call, else ""
	StartByte uint
	Line      uint // 1-based
}

// TypeBinding records that, at a byte offset, identifier Name has static type
// TypeName. The indexer turns StartByte into a scope (the enclosing definition).
type TypeBinding struct {
	Name      string
	TypeName  string
	StartByte uint
}

// Result is the extracted graph for one file.
type Result struct {
	Symbols  []Symbol
	Edges    []Edge
	Bindings []TypeBinding
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
// compiled query. All C-allocating objects are closed before returning. path is
// currently unused but retained for future qualified-name construction.
func (r *Registry) ParseFile(lang, path string, src []byte) (Result, error) {
	l, ok := r.langs[lang]
	if !ok {
		return Result{}, fmt.Errorf("parse: unsupported language %q", lang)
	}
	q := r.queries[lang]

	p := sitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(l); err != nil {
		return Result{}, fmt.Errorf("parse: set %s language: %w", lang, err)
	}
	tree := p.Parse(src, nil)
	if tree == nil {
		return Result{}, fmt.Errorf("parse: tree-sitter returned nil tree for %s", lang)
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()

	captureNames := q.CaptureNames()
	var res Result
	matches := cur.Matches(q, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var name, recv, recvType, recvName, bindName, bindType string
		var defKind, refKind string
		var sb, eb, sl, el uint
		var recvNameByte, bindByte uint
		for _, c := range m.Captures {
			cn := captureNames[c.Index]
			switch {
			case cn == "name":
				name = strings.Trim(c.Node.Utf8Text(src), `"`)
			case cn == "recv":
				recv = c.Node.Utf8Text(src)
			case cn == "recv.type":
				recvType = strings.TrimPrefix(c.Node.Utf8Text(src), "*")
			case cn == "recv.name":
				recvName = c.Node.Utf8Text(src)
				recvNameByte = c.Node.StartByte()
			case cn == "bind.name":
				bindName = c.Node.Utf8Text(src)
				bindByte = c.Node.StartByte()
			case cn == "bind.type":
				bindType = strings.TrimPrefix(c.Node.Utf8Text(src), "*")
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
				Kind: defKind, Name: name, QualifiedName: name, RecvType: recvType,
				StartByte: sb, EndByte: eb, StartLine: sl, EndLine: el,
			})
			if recvType != "" && recvName != "" {
				res.Bindings = append(res.Bindings,
					TypeBinding{Name: recvName, TypeName: recvType, StartByte: recvNameByte})
			}
		case refKind != "" && name != "":
			res.Edges = append(res.Edges, Edge{
				Kind: edgeKind(refKind), DstName: name, Recv: recv, StartByte: sb, Line: sl,
			})
		case bindName != "" && bindType != "" && bindType != "var":
			// C# `var x = …` yields an implicit_type "var"; skip it — the real type
			// comes from the object-creation binding pattern. ("var" is a contextual
			// keyword in C# and not a valid type name in Go either, so this is safe.)
			res.Bindings = append(res.Bindings,
				TypeBinding{Name: bindName, TypeName: bindType, StartByte: bindByte})
		}
	}
	return res, nil
}
