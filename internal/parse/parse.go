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
	StartLine     uint // 1-based, first line of the full span (attributes/decorators included)
	EndLine       uint // 1-based
	// NameByte/NameLine locate the declared identifier itself. A C# attribute or
	// TS decorator is part of the declaration node, so StartLine points at the
	// attribute line — but "where is this symbol" questions (outline, query,
	// search) want the line that names it.
	NameByte uint
	NameLine uint // 1-based
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

// countNamedChildrenOfKind counts p's named children of one kind. Comments are
// named nodes in the Go grammar, so a bare NamedChildCount over a grouped
// `type ( … )` block would miscount; filtering by kind is exact.
func countNamedChildrenOfKind(p *sitter.Node, kind string) int {
	n := 0
	for i := uint(0); i < p.NamedChildCount(); i++ {
		if c := p.NamedChild(i); c != nil && c.Kind() == kind {
			n++
		}
	}
	return n
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
	case "annotation":
		return "annotates"
	default:
		return "references"
	}
}

// macroArgCall decides whether an identifier the Rust query proposes inside a
// macro's token tree (tree-sitter-rust leaves macro arguments unparsed) is a
// call: the arguments must follow it directly, or after a `::<..>` turbofish, as
// a parenthesized tree — `x, (1, 2)`, `vec![..]` and `a::b` are not calls. A
// `recv.` or `Path::` prefix becomes the receiver, as for calls outside macros.
// Names outside token trees pass untouched.
func macroArgCall(name *sitter.Node, src []byte, recv *string) bool {
	if name == nil {
		return true
	}
	if p := name.Parent(); p == nil || p.Kind() != "token_tree" {
		return true
	}
	next := name.NextSibling()
	if next != nil && next.Kind() == "::" {
		// Turbofish f::<T, Vec<U>>(..): step past the balanced angle brackets
		// (the lexer fuses a closing `>>` into one token).
		lt := next.NextSibling()
		if lt == nil || lt.Kind() != "<" {
			return false
		}
		depth := 0
		for next = lt; next != nil; next = next.NextSibling() {
			switch next.Kind() {
			case "<":
				depth++
			case ">":
				depth--
			case ">>":
				depth -= 2
			}
			if depth <= 0 {
				break
			}
		}
		if next != nil {
			next = next.NextSibling()
		}
	}
	if next == nil || next.Kind() != "token_tree" || next.Child(0) == nil || next.Child(0).Kind() != "(" {
		return false
	}
	if sep := name.PrevSibling(); sep != nil && (sep.Kind() == "." || sep.Kind() == "::") {
		if r := sep.PrevSibling(); r != nil && (r.Kind() == "identifier" || (sep.Kind() == "." && r.Kind() == "self")) {
			*recv = r.Utf8Text(src)
		}
	}
	return true
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
		var nameByte, nameLine uint
		var recvNameByte, bindByte, selfByte uint
		var isSelf bool
		var defNode, nameNode *sitter.Node
		for _, c := range m.Captures {
			cn := captureNames[c.Index]
			switch {
			case cn == "name":
				name = strings.Trim(c.Node.Utf8Text(src), `"`)
				nameByte = c.Node.StartByte()
				nameLine = c.Node.StartPosition().Row + 1
				n := c.Node
				nameNode = &n
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
			case cn == "bind.self":
				isSelf, selfByte = true, c.Node.StartByte()
			case strings.HasPrefix(cn, "definition."):
				defKind = strings.TrimPrefix(cn, "definition.")
				n := c.Node
				defNode = &n
				sb, eb = c.Node.StartByte(), c.Node.EndByte()
				sl, el = c.Node.StartPosition().Row+1, c.Node.EndPosition().Row+1
			case strings.HasPrefix(cn, "reference."):
				refKind = strings.TrimPrefix(cn, "reference.")
				sb = c.Node.StartByte()
				sl = c.Node.StartPosition().Row + 1
			}
		}
		if refKind == "call" && lang == "rust" && !macroArgCall(nameNode, src, &recv) {
			continue
		}
		switch {
		case defKind != "" && name != "":
			// A Go type capture is the type_spec, whose span starts at the identifier —
			// the `type` keyword lives on the parent type_declaration. Widen single-spec
			// declarations to the parent so the snippet is byte-exact source; grouped
			// `type ( A …; B … )` blocks keep per-spec spans so each name stays a
			// distinct location.
			if lang == "go" && defKind == "type" && defNode != nil {
				if p := defNode.Parent(); p != nil && p.Kind() == "type_declaration" &&
					countNamedChildrenOfKind(p, "type_spec") == 1 {
					sb, eb = p.StartByte(), p.EndByte()
					sl, el = p.StartPosition().Row+1, p.EndPosition().Row+1
				}
			}
			// Rust attributes (#[derive(..)], #[test]) precede their item as
			// siblings rather than nesting in it like a C# attribute list. Widen the
			// span over them so snippets show them and annotation edges land on the
			// item; comments between attribute and item are stepped over.
			if lang == "rust" && defNode != nil {
				for p := defNode.PrevNamedSibling(); p != nil; p = p.PrevNamedSibling() {
					if k := p.Kind(); k == "attribute_item" {
						sb, sl = p.StartByte(), p.StartPosition().Row+1
					} else if k != "line_comment" && k != "block_comment" {
						break
					}
				}
			}
			res.Symbols = append(res.Symbols, Symbol{
				Kind: defKind, Name: name, QualifiedName: name, RecvType: recvType,
				StartByte: sb, EndByte: eb, StartLine: sl, EndLine: el,
				NameByte: nameByte, NameLine: nameLine,
			})
			if recvType != "" && recvName != "" {
				res.Bindings = append(res.Bindings,
					TypeBinding{Name: recvName, TypeName: recvType, StartByte: recvNameByte})
			}
		case refKind != "" && name != "":
			res.Edges = append(res.Edges, Edge{
				Kind: edgeKind(refKind), DstName: name, Recv: recv, StartByte: sb, Line: sl,
			})
		case isSelf && bindType != "":
			// Rust impl/trait body: `self` and `Self` both denote the implementing
			// type, scoped (via the enclosing impl/trait symbol) to the whole body.
			res.Bindings = append(res.Bindings,
				TypeBinding{Name: "self", TypeName: bindType, StartByte: selfByte},
				TypeBinding{Name: "Self", TypeName: bindType, StartByte: selfByte})
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
