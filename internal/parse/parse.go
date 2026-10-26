package parse

import (
	"fmt"
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
	py := pyFile{bases: map[uint]string{}, types: map[pyName]string{}}
	matches := cur.Matches(q, tree.RootNode(), src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var name, recv, recvType, recvName, bindName, bindType string
		var bindAttr, bindFrom, superBase string
		var defKind, refKind string
		var sb, eb, sl, el uint
		var nameByte, nameLine uint
		var recvNameByte, bindByte, selfByte uint
		var superClass, superOf, bindClass uint
		var bindDef uintptr
		var isSelf, onLiteral bool
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
					onLiteral = pyLiterals[c.Node.Kind()]
					if recv = pyRecv(c.Node, src, recv); recv == "super()" {
						superOf, _ = pyScope(&c.Node)
					}
				}
			case cn == "recv.type":
				recvType = strings.TrimPrefix(c.Node.Utf8Text(src), "*")
			case cn == "recv.name":
				recvName = c.Node.Utf8Text(src)
				recvNameByte = c.Node.StartByte()
			case cn == "bind.name":
				bindName = c.Node.Utf8Text(src)
				bindByte = c.Node.StartByte()
				if lang == "python" {
					// A `x: T` / `x = T()` in a class body, outside any def, is what
					// methods read as self.x.
					if bindClass, bindDef = pyScope(&c.Node); bindClass != 0 && bindDef == 0 {
						bindAttr, bindName = bindName, ""
					}
				}
			case cn == "bind.attr":
				bindAttr = c.Node.Utf8Text(src)
				bindClass, bindDef = pyScope(&c.Node)
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
		case refKind != "" && name != "" && !onLiteral:
			if superOf != 0 {
				py.supers = append(py.supers, pySuper{edge: len(res.Edges), class: superOf})
			}
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
			py.bases[superClass] = superBase
		case bindAttr != "" && (bindType != "" || bindFrom != "") && bindClass != 0:
			py.attrs = append(py.attrs, pyAttr{name: bindAttr, typ: bindType, from: bindFrom, class: bindClass, def: bindDef})
		case bindName != "" && bindType != "" && bindType != "var":
			// C# `var x = …` yields an implicit_type "var"; skip it — the real type
			// comes from the object-creation binding pattern. ("var" is a contextual
			// keyword in C# and not a valid type name in Go either, so this is safe.)
			res.Bindings = append(res.Bindings,
				TypeBinding{Name: bindName, TypeName: bindType, StartByte: bindByte})
			if lang == "python" {
				py.types[pyName{bindDef, bindName}] = bindType
			}
		}
	}
	if lang == "python" {
		py.finish(&res)
	}
	return res, nil
}

// pyFile collects what the Python passes need from every match: each class's
// first positional base and each def's parameter and local types, for the
// super() calls and self.x bindings found along the way.
type pyFile struct {
	bases  map[uint]string // class name byte -> first positional base
	types  map[pyName]string
	supers []pySuper
	attrs  []pyAttr
}

// pyName is a name bound in a def (identified by its node id; 0 is module level).
type pyName struct {
	def  uintptr
	name string
}

// pySuper is a super().m() call edge and the name byte of its innermost class.
type pySuper struct {
	edge  int
	class uint
}

// pyAttr is a self.x binding of a class (its name byte), typed by a type
// expression (typ) or by the def's parameter or local it is assigned (from).
type pyAttr struct {
	name, typ, from string
	class           uint
	def             uintptr
}

// finish names each super() receiver after its class's first base, so the call
// binds Base.m like Base.m() would, and adds the self.x bindings, anchored at
// the class name so their scope is the whole class.
func (py *pyFile) finish(res *Result) {
	for _, s := range py.supers {
		if base := py.bases[s.class]; base != "" {
			res.Edges[s.edge].Recv = base
		}
	}
	for _, a := range py.attrs {
		typ := a.typ
		if a.from != "" {
			typ = py.types[pyName{a.def, a.from}]
		}
		if typ != "" {
			res.Bindings = append(res.Bindings, TypeBinding{Name: "self." + a.name, TypeName: typ, StartByte: a.class})
		}
	}
}

// pyScope returns the name byte of n's innermost enclosing class and the node id
// of its innermost def within that class (0 when there is none).
func pyScope(n *sitter.Node) (class uint, def uintptr) {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Kind() {
		case "class_definition":
			if nm := p.ChildByFieldName("name"); nm != nil {
				return nm.StartByte(), def
			}
		case "function_definition":
			if def == 0 {
				def = p.Id()
			}
		}
	}
	return 0, def
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

// pyLiterals are the receiver kinds whose methods are the builtins' (", ".join,
// [].append): a call on one gets no edge, which name matching would otherwise
// bind to any same-named method of the worktree.
var pyLiterals = map[string]bool{
	"string": true, "concatenated_string": true, "integer": true, "float": true,
	"list": true, "dictionary": true, "set": true, "tuple": true,
	"list_comprehension": true, "dictionary_comprehension": true, "set_comprehension": true,
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
