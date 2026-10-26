package parse

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

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

// macroCalls returns the calls written in a Rust macro's argument token tree
// (tree-sitter-rust leaves macro arguments unparsed) and in the trees nested one
// level in it. An identifier is a call when a parenthesized tree follows it
// directly or after a `::<..>` turbofish — `x, (1, 2)`, `vec![..]` and `a::b` are
// not calls — and a `recv.` or `Path::` prefix becomes the receiver, as for calls
// outside macros. Each tree's tokens are read once through cur and every `<` is
// matched to its closer in one stack pass, so the scan stays linear in the size
// of the macro however its angle brackets nest (an unbalanced `a::<a::<…` run
// must not rescan the rest of the tree per identifier).
func macroCalls(args sitter.Node, cur *sitter.TreeCursor, src []byte, nested bool) []Edge {
	toks := args.Children(cur)
	// closer: index of a `<` -> index of the token closing it, or -1 when it
	// closes inside a fused `>>` (in `a::<b::<T>>()` only the outer `a` is followed
	// by the call's arguments).
	closer := map[int]int{}
	var open []int
	pop := func(at int) {
		if len(open) > 0 {
			closer[open[len(open)-1]] = at
			open = open[:len(open)-1]
		}
	}
	for i := range toks {
		switch toks[i].Kind() {
		case "<":
			open = append(open, i)
		case ">":
			pop(i)
		case ">>": // the lexer fuses two closers into one token
			pop(-1)
			pop(i)
		}
	}
	var out []Edge
	for i := range toks {
		tok := toks[i]
		if tok.Kind() == "token_tree" && !nested {
			out = append(out, macroCalls(tok, cur, src, true)...)
			continue
		}
		if tok.Kind() != "identifier" {
			continue
		}
		next := i + 1
		if next+1 < len(toks) && toks[next].Kind() == "::" && toks[next+1].Kind() == "<" {
			c, ok := closer[next+1]
			if !ok || c < 0 {
				continue // unclosed, or closed inside a `>>`
			}
			next = c + 1
		}
		if next >= len(toks) || toks[next].Kind() != "token_tree" {
			continue
		}
		if open := toks[next].Child(0); open == nil || open.Kind() != "(" {
			continue
		}
		recv := ""
		if i >= 2 {
			sep, r := toks[i-1].Kind(), toks[i-2]
			if (sep == "." || sep == "::") && (r.Kind() == "identifier" || (sep == "." && r.Kind() == "self")) {
				recv = r.Utf8Text(src)
			}
		}
		out = append(out, Edge{
			Kind: "calls", DstName: tok.Utf8Text(src), Recv: recv,
			StartByte: tok.StartByte(), Line: tok.StartPosition().Row + 1,
		})
	}
	return out
}

// ParseFile parses src as lang and extracts symbols/edges using the language's
// compiled query. All C-allocating objects are closed before returning. path is
// currently unused but retained for future qualified-name construction.
func (r *Registry) ParseFile(lang, path string, src []byte) (Result, error) {
	l, ok := r.langs[lang]
	if !ok {
		return Result{}, fmt.Errorf("parse: unsupported language %q", lang)
	}
	q, err := r.query(lang)
	if err != nil {
		return Result{}, err
	}

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
	walker := tree.Walk() // reused by macroCalls
	defer walker.Close()

	captureNames := q.CaptureNames()
	var res Result
	var pyAttrs []pyAttr
	pyBases := map[uint]string{} // Python class name byte -> first positional base
	matches := cur.Matches(q, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var name, recv, recvType, recvName, bindName, bindType string
		var bindAttr, bindFrom, superBase string
		var defKind, refKind string
		var sb, eb, sl, el uint
		var nameByte, nameLine uint
		var recvNameByte, bindByte, selfByte, superClass uint
		var isSelf bool
		var defNode *sitter.Node
		for _, c := range m.Captures {
			cn := captureNames[c.Index]
			switch {
			case cn == "name":
				name = strings.Trim(c.Node.Utf8Text(src), `"`)
				nameByte = c.Node.StartByte()
				nameLine = c.Node.StartPosition().Row + 1
			case cn == "recv":
				recv = c.Node.Utf8Text(src)
				if lang == "python" {
					recv = pyRecv(c.Node, src, recv)
				}
			case cn == "recv.type":
				recvType = strings.TrimPrefix(c.Node.Utf8Text(src), "*")
			case cn == "recv.name":
				recvName = c.Node.Utf8Text(src)
				recvNameByte = c.Node.StartByte()
			case cn == "bind.name":
				bindName = c.Node.Utf8Text(src)
				bindByte = c.Node.StartByte()
				// A Python class-body `x: T` / `x = T()` is what methods read as self.x.
				if lang == "python" && pyClassLevel(c.Node) {
					bindAttr, bindName = bindName, ""
				}
			case cn == "bind.attr":
				bindAttr, bindByte = c.Node.Utf8Text(src), c.Node.StartByte()
			case cn == "bind.from":
				bindFrom = c.Node.Utf8Text(src)
			case cn == "bind.type":
				bindType = strings.TrimPrefix(c.Node.Utf8Text(src), "*")
			case cn == "bind.self":
				isSelf, selfByte = true, c.Node.StartByte()
			case cn == "super.base":
				superBase = c.Node.Utf8Text(src)
			case cn == "super.class":
				superClass = c.Node.StartByte()
			case cn == "macro.args":
				res.Edges = append(res.Edges, macroCalls(c.Node, walker, src, false)...)
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
			if lang == "python" && defNode != nil {
				// Decorators wrap the def/class in a decorated_definition instead of
				// nesting in it; widen the span over them for the same reason as Rust's.
				if p := defNode.Parent(); p != nil && p.Kind() == "decorated_definition" {
					sb, sl = p.StartByte(), p.StartPosition().Row+1
				}
				if defKind == "function" && pyInClass(defNode) {
					defKind = "method"
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
		case superBase != "":
			pyBases[superClass] = superBase
		case bindAttr != "" && (bindType != "" || bindFrom != ""):
			pyAttrs = append(pyAttrs, pyAttr{name: bindAttr, typ: bindType, from: bindFrom, at: bindByte})
		case bindName != "" && bindType != "" && bindType != "var":
			// C# `var x = …` yields an implicit_type "var"; skip it — the real type
			// comes from the object-creation binding pattern. ("var" is a contextual
			// keyword in C# and not a valid type name in Go either, so this is safe.)
			res.Bindings = append(res.Bindings,
				TypeBinding{Name: bindName, TypeName: bindType, StartByte: bindByte})
		}
	}
	if lang == "python" {
		pyScopeClasses(&res, pyAttrs, pyBases)
	}
	return res, nil
}

// pyAttr is a Python self.x binding waiting for its class: typed by a type
// expression (typ) or by the annotated parameter assigned to it (from).
type pyAttr struct {
	name, typ, from string
	at              uint
}

// pyInClass reports whether a def's nearest enclosing def or class is a class,
// which makes it a method.
func pyInClass(n *sitter.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Kind() {
		case "class_definition":
			return true
		case "function_definition":
			return false
		}
	}
	return false
}

// pyClassLevel reports whether a bound name is the target of a statement written
// directly in a class body.
func pyClassLevel(n sitter.Node) bool {
	p := n.Parent()
	for _, kind := range []string{"assignment", "expression_statement", "block", "class_definition"} {
		if p == nil || p.Kind() != kind {
			return false
		}
		p = p.Parent()
	}
	return true
}

// pyRecv normalizes a Python call receiver: super() and super(C, self) become
// "super()", named after the base once the classes are known, and m.Type (a
// CapWords last segment is a class, by PEP 8) becomes Type, the receiver static
// resolution binds, as for Rust's module::Type::f().
func pyRecv(n sitter.Node, src []byte, text string) string {
	switch n.Kind() {
	case "call":
		if f := n.ChildByFieldName("function"); f != nil && f.Kind() == "identifier" && f.Utf8Text(src) == "super" {
			return "super()"
		}
	case "attribute":
		if a := n.ChildByFieldName("attribute"); a != nil {
			if name := a.Utf8Text(src); name != "" && unicode.IsUpper(rune(name[0])) {
				return name
			}
		}
	}
	return text
}

// pyScopeClasses runs the Python passes that need the file's classes: a
// super().m() receiver becomes the innermost class's first base, so the call
// binds Base.m like Base.m() would, and each self.x binding is scoped to its
// innermost class (`self.x = param` taking the parameter's annotated type).
func pyScopeClasses(res *Result, attrs []pyAttr, bases map[uint]string) {
	for i := range res.Edges {
		e := &res.Edges[i]
		if e.Kind == "calls" && e.Recv == "super()" {
			if c := innermostSymbol(res.Symbols, e.StartByte, "class"); c != nil && bases[c.NameByte] != "" {
				e.Recv = bases[c.NameByte]
			}
		}
	}
	params := res.Bindings // the parameters and locals; self.x ones are appended below
	for _, a := range attrs {
		c := innermostSymbol(res.Symbols, a.at, "class")
		if c == nil {
			continue
		}
		typ := a.typ
		if a.from != "" {
			if fn := innermostSymbol(res.Symbols, a.at, "method", "function"); fn != nil {
				for _, b := range params {
					if b.Name == a.from && fn.StartByte <= b.StartByte && b.StartByte < fn.EndByte {
						typ = b.TypeName
						break
					}
				}
			}
		}
		if typ != "" {
			// Anchored at the class name, the binding's scope is the whole class.
			res.Bindings = append(res.Bindings, TypeBinding{Name: "self." + a.name, TypeName: typ, StartByte: c.NameByte})
		}
	}
}

// innermostSymbol returns the smallest symbol of one of kinds whose span holds b.
func innermostSymbol(syms []Symbol, b uint, kinds ...string) *Symbol {
	var best *Symbol
	for i := range syms {
		s := &syms[i]
		if s.StartByte <= b && b < s.EndByte && slices.Contains(kinds, s.Kind) &&
			(best == nil || s.EndByte-s.StartByte < best.EndByte-best.StartByte) {
			best = s
		}
	}
	return best
}
