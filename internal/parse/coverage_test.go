package parse

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// TestUpstreamTagsCoverage is the trust ratchet for our extraction queries: it
// compares each language's query (queries/<lang>.scm) against the CANONICAL
// tags.scm shipped INSIDE the same grammar module we link (vendored verbatim in
// testdata/upstream/, so the comparison is pinned to the exact grammar version
// — a query from another version may not even compile). tags.scm is what
// GitHub code navigation runs, so it is the community's distilled list of
// "constructs that define or reference symbols" for the language.
//
// Two checks per language:
//
//  1. roots: every upstream definition/reference pattern's root node type must
//     be a node type our query mentions — a miss means a whole construct
//     (e.g. record_declaration) is invisible to us.
//  2. descendants: every named node type mentioned anywhere in an upstream
//     definition/reference pattern must appear somewhere in our query — a miss
//     means a variant shape (e.g. a parenthesized call) slips through a
//     construct we otherwise cover.
//
// Divergence is allowed ONLY via the allowlist below, where each entry states
// why we deliberately differ. When a grammar module is upgraded, re-vendor its
// tags.scm (cp $(go env GOMODCACHE)/github.com/tree-sitter/<grammar>@<ver>/queries/tags.scm
// testdata/upstream/<lang>-tags.scm) so the ratchet tracks upstream's learning.
//
// The reverse direction (our queries are richer: C# records, structs, enums,
// delegates, properties, constructors, attributes, receivers, type bindings)
// is intentional and proven by the integration suite, not by this test.
func TestUpstreamTagsCoverage(t *testing.T) {
	// Leaf/name-ish node types that appear inside nearly every pattern and carry
	// no construct information of their own.
	ignore := map[string]bool{
		"identifier": true, "property_identifier": true, "field_identifier": true,
		"package_identifier": true, "string": true, "number": true, "comment": true,
		"undefined": true, "null": true,
	}

	// allow lists the upstream node types we DELIBERATELY do not mirror.
	allow := map[string]map[string]string{
		"csharp": {
			"type_parameter_constraints_clause": "`where T : IFace` constraint mentions: textual type usages, answered by search_content; no graph tool consumes constraint edges",
			"type_parameter_constraint":         "see type_parameter_constraints_clause",
			"type":                              "wrapper inside type_parameter_constraint; same rationale",
			"variable_declaration":              "covered more richly by type BINDINGS (name→type with scope), which feed receiver resolution; textual type references are answered by search_content",
		},
		"go": {
			"type_identifier": "upstream tags every bare type mention as a reference; for us textual type usages are answered by search_content and typed dispatch by bindings — an edge per mention would bloat the graph with data no tool reads",
		},
		"typescript": {
			"type_annotation":      "`: Foo` type annotations: covered by type bindings + search_content (same rationale as Go's bare type_identifier)",
			"lexical_declaration":  "our function-valued-variable defs anchor at variable_declarator, which both lexical_declaration and variable_declaration contain — same matches, tighter anchor",
			"variable_declaration": "see lexical_declaration",
			"export_statement":     "upstream's `module.exports`-style exported-constant defs; constants are not in our symbol taxonomy (search_content finds them)",
			"binary_expression":    "value position of the exported-constant pattern; see export_statement",
		},
		"javascript": {
			"lexical_declaration":  "our function-valued-variable defs anchor at variable_declarator, which both lexical_declaration and variable_declaration contain — same matches, tighter anchor",
			"variable_declaration": "see lexical_declaration",
			"export_statement":     "upstream's exported-constant defs; constants are not in our symbol taxonomy (search_content finds them)",
			"binary_expression":    "value position of the exported-constant pattern; see export_statement",
		},
	}

	// Which upstream files feed which of our queries. TypeScript's grammar
	// extends JavaScript's, and upstream tooling stacks both tag files when
	// tagging TS — so our typescript.scm must cover the union.
	upstreamFor := map[string][]string{
		"csharp":     {"csharp-tags.scm"},
		"go":         {"go-tags.scm"},
		"typescript": {"typescript-tags.scm", "javascript-tags.scm"},
		"javascript": {"javascript-tags.scm"},
	}

	for lang, files := range upstreamFor {
		t.Run(lang, func(t *testing.T) {
			oursSrc := querySources()[lang]
			if oursSrc == "" {
				t.Fatalf("no query source for %s", lang)
			}
			ourTypes := map[string]bool{}
			for _, p := range parseSCM(t, oursSrc) {
				for n := range p.allTypes {
					ourTypes[n] = true
				}
			}
			for _, f := range files {
				raw, err := os.ReadFile(filepath.Join("testdata", "upstream", f))
				if err != nil {
					t.Fatalf("read %s: %v", f, err)
				}
				for _, p := range parseSCM(t, string(raw)) {
					if !p.isDef && !p.isRef {
						continue // doc/locals helper patterns without a def/ref capture
					}
					check := func(n, where string) {
						if ignore[n] || ourTypes[n] || allow[lang][n] != "" {
							return
						}
						t.Errorf("%s (%s): upstream %s pattern uses node type %q (%s) — our %s.scm never mentions it; cover it or allowlist it with a justification",
							lang, f, p.class(), n, where, lang)
					}
					for n := range p.rootTypes {
						check(n, "pattern root")
					}
					for n := range p.allTypes {
						check(n, "descendant")
					}
				}
			}
		})
	}
}

// scmPattern is one top-level pattern of a .scm file: the named node types it
// mentions (roots = first level of named nodes) and whether any capture
// classifies it as a definition or reference pattern.
type scmPattern struct {
	rootTypes map[string]bool
	allTypes  map[string]bool
	isDef     bool
	isRef     bool
}

func (p scmPattern) class() string {
	if p.isDef {
		return "definition"
	}
	return "reference"
}

// parseSCM splits a tree-sitter query file into top-level patterns and
// extracts, per pattern, the named node types and capture classes. It
// understands the subset of query syntax the tag files use: named nodes,
// grouping parens, alternations [..], anchors ., field names `f:`, captures
// @x.y, predicates (#pred ...), quantifiers * + ?, wildcards (_), strings, and
// ; comments.
func parseSCM(t *testing.T, src string) []scmPattern {
	t.Helper()
	var patterns []scmPattern
	cur := scmPattern{rootTypes: map[string]bool{}, allTypes: map[string]bool{}}
	nodeDepth := 0 // depth in NAMED nodes only (grouping parens don't count)
	var stack []bool
	flush := func() {
		if len(cur.allTypes) > 0 || cur.isDef || cur.isRef {
			patterns = append(patterns, cur)
		}
		cur = scmPattern{rootTypes: map[string]bool{}, allTypes: map[string]bool{}}
	}
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ';': // comment to end of line
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"': // string literal (may contain parens/escapes)
			i++
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '(':
			i++
			for i < len(src) && unicode.IsSpace(rune(src[i])) {
				i++
			}
			isNode := false
			if i < len(src) && (isIdentByte(src[i]) || src[i] == '_') {
				start := i
				for i < len(src) && isIdentByte(src[i]) {
					i++
				}
				name := src[start:i]
				if name != "" && name != "_" {
					isNode = true
					cur.allTypes[name] = true
					if nodeDepth == 0 {
						cur.rootTypes[name] = true
					}
				}
			} else if i < len(src) && src[i] == '#' { // predicate: skip its name
				for i < len(src) && src[i] != ' ' && src[i] != ')' {
					i++
				}
			}
			if isNode {
				nodeDepth++
			}
			stack = append(stack, isNode)
		case c == ')':
			if len(stack) == 0 {
				t.Fatalf("unbalanced ')' at byte %d", i)
			}
			if stack[len(stack)-1] {
				nodeDepth--
			}
			stack = stack[:len(stack)-1]
			i++
			if len(stack) == 0 {
				// Top-level pattern closed — unless an alternation/group sibling
				// follows immediately; captures after ')' belong to this pattern, so
				// only flush at the next top-level '(' or '[' (handled lazily below).
				// Simplest correct rule: flush when we next see a top-level opener.
			}
		case c == '[' || c == ']':
			i++
		case c == '@': // capture
			start := i
			i++
			for i < len(src) && (isIdentByte(src[i]) || src[i] == '.') {
				i++
			}
			capture := src[start:i]
			if strings.HasPrefix(capture, "@definition.") {
				cur.isDef = true
			}
			if strings.HasPrefix(capture, "@reference.") {
				cur.isRef = true
			}
		default:
			if len(stack) == 0 && (c == '(' || c == '[') {
				flush()
			}
			i++
		}
		// A new top-level opener starts the next pattern.
		if len(stack) == 0 && i < len(src) {
			j := i
			for j < len(src) && unicode.IsSpace(rune(src[j])) {
				j++
			}
			if j < len(src) && (src[j] == '(' || src[j] == '[') {
				flush()
				i = j
			}
		}
	}
	flush()
	if len(stack) != 0 {
		t.Fatal("unbalanced '(' at end of query file")
	}
	// Drop empty placeholder patterns.
	kept := patterns[:0]
	for _, p := range patterns {
		if len(p.allTypes) > 0 {
			kept = append(kept, p)
		}
	}
	return kept
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '!' || b == '?' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// TestParseSCMSelfCheck pins the mini-parser on a representative snippet so
// ratchet failures are attributable to coverage, not parser drift.
func TestParseSCMSelfCheck(t *testing.T) {
	src := `
; comment with (parens)
(class_declaration name: (identifier) @name) @definition.class
(
  (comment)* @doc
  .
  (function_declaration name: (identifier) @name) @definition.function
  (#strip! @doc "^//\\s*")
)
(call_expression function: [(identifier) @name (parenthesized_expression (selector_expression field: (field_identifier) @name))]) @reference.call
(package_clause "package" (package_identifier) @name)
`
	ps := parseSCM(t, src)
	if len(ps) != 4 {
		t.Fatalf("got %d patterns, want 4", len(ps))
	}
	var roots []string
	for n := range ps[1].rootTypes {
		roots = append(roots, n)
	}
	sort.Strings(roots)
	if strings.Join(roots, ",") != "comment,function_declaration" {
		t.Errorf("doc-wrapped pattern roots = %v", roots)
	}
	if !ps[0].isDef || ps[0].isRef {
		t.Errorf("pattern 0 classes = def:%v ref:%v", ps[0].isDef, ps[0].isRef)
	}
	if !ps[2].isRef || !ps[2].allTypes["parenthesized_expression"] {
		t.Errorf("pattern 2 = %+v", ps[2])
	}
	if ps[3].isDef || ps[3].isRef {
		t.Errorf("captureless pattern misclassified: %+v", ps[3])
	}
}
