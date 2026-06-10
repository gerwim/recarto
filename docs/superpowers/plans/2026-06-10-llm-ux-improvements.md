# LLM-UX Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the tool-output gaps an LLM user hit in real use (snippet missing `path`, trimmed Go type spans, undifferentiated test callers) and add the four capabilities it missed most (first-class `find_references`, batch snippets, `offset` paging, all-test-callers hint).

**Architecture:** All changes live in the graph query surface: engine capabilities in `internal/graph` (SDK-free, unit-tested), thin MCP mappings in `internal/mcp` (handlers are transport-agnostic functions tested directly), one parser-level span fix in `internal/parse`, and agent-guidance updates in `internal/agents/instructions.go`. No schema migration is needed — every feature reads existing tables (`nodes`, `edges`, `files`).

**Tech Stack:** Go, tree-sitter (`github.com/tree-sitter/go-tree-sitter`), SQLite (modernc), MCP go-sdk (confined to `internal/mcp/server.go`).

**Origin:** This plan implements the feedback filed as recarto issues `019eb160-aa50-…` (snippet path/span) and `019eb160-c5b2-…` (test-caller annotation), plus the "what I miss most" list from the same session (batch snippets, find_references, offset paging).

---

## Out of scope — follow-up plans

Each of these needs its own brainstorm/design pass first; do NOT fold them in here:

1. **Graph delta on resume** ("what changed since my last session") — needs a snapshot/ref-comparison design.
2. **Memory contradiction detection at record time** — the near-duplicate reinforcement machinery detects similarity, not polarity; needs design and likely LLM-side judgment.
3. **Session-end capture nudge** — host-adapter (hook) work in `internal/agents`, not a query tool.
4. **CLI parity** (`recarto graph refs`, batch/offset flags, path header on `graph snippet`) — additive; the MCP surface is what the LLM uses, CLI follows later.

---

## Conventions used by every task

- Engine tests live in `internal/graph/*_test.go`, package `graph`. Use the existing helpers: `newTestEngine(t)` (graph_test.go:14) and `indexInto(t, e, wt, rel, content)` (content_test.go:37). `indexInto` parses+indexes one file but does **not** run cross-file resolve.
- MCP handler tests live in `internal/mcp/*_test.go`, package `mcp`. Use `newTestApp(t, "main")`, set `a.Git.Worktree = t.TempDir()`, write files with `os.WriteFile`, then `a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt})` (full index + resolve). See `TestSearchContentHandler` (graph_test.go:56) for the exact shape.
- Run a single test: `go test ./internal/graph/ -run TestName -v` (or `./internal/mcp/`).
- Commit after every task. End every commit message with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

---

### Task 1: Export `IsTestPath`

The MCP layer (Task 5) needs the test-path classifier that `internal/graph/testpaths.go` keeps unexported.

**Files:**
- Modify: `internal/graph/testpaths.go`
- Test: `internal/graph/testpaths_test.go`

- [ ] **Step 1: Write the failing test** — append to `internal/graph/testpaths_test.go`:

```go
func TestIsTestPathExported(t *testing.T) {
	if !IsTestPath("pkg/svc/handler_test.go") {
		t.Error("IsTestPath(handler_test.go) = false, want true")
	}
	if IsTestPath("pkg/svc/handler.go") {
		t.Error("IsTestPath(handler.go) = true, want false")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/graph/ -run TestIsTestPathExported -v`
Expected: FAIL — `undefined: IsTestPath`

- [ ] **Step 3: Implement** — append to `internal/graph/testpaths.go`:

```go
// IsTestPath reports whether a repo-relative path looks like test code. Exported
// for the MCP layer, which flags trace hops and references with it so an LLM can
// tell test callers from production at a glance.
func IsTestPath(p string) bool { return isTestPath(p) }
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/graph/ -run TestIsTestPath -v`
Expected: PASS (both the old and new test)

- [ ] **Step 5: Commit**

```bash
git add internal/graph/testpaths.go internal/graph/testpaths_test.go
git commit -m "feat(graph): export IsTestPath for MCP-layer annotation"
```

---

### Task 2: Snippet results carry `path`

`SnippetResult` (internal/graph/snippet.go:17) has no `Path`; the MCP output (internal/mcp/graph.go:358) drops it too. After by-name resolution the caller doesn't know which file the code is in — no clickable `file:line`, no targeted edit.

**Files:**
- Modify: `internal/graph/snippet.go`
- Modify: `internal/mcp/graph.go:358-396`
- Modify: `internal/mcp/server.go:58-60` (tool description)
- Test: `internal/graph/snippet_test.go`, `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing engine tests** — append to `internal/graph/snippet_test.go`:

```go
func TestSnippetCarriesPath(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")

	r, cands, err := e.SnippetByName(ctx, wt, "Target")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found || len(cands) != 0 {
		t.Fatalf("want unique hit, got found=%v cands=%+v", r.Found, cands)
	}
	if r.Path != "a.go" {
		t.Errorf("Path = %q, want a.go", r.Path)
	}
}

func TestOverloadSnippetCarriesPath(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "C.cs", "class C {\n  void Run(int a) {}\n  void Run(string b) {}\n}\n")

	r, cands, err := e.SnippetByName(ctx, wt, "C.Run")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found || len(cands) != 0 {
		t.Fatalf("want merged overload set, got found=%v cands=%+v", r.Found, cands)
	}
	if r.Path != "C.cs" {
		t.Errorf("Path = %q, want C.cs", r.Path)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/graph/ -run 'TestSnippetCarriesPath|TestOverloadSnippetCarriesPath' -v`
Expected: FAIL — `r.Path undefined (type SnippetResult has no field or method Path)`

- [ ] **Step 3: Implement the engine change** in `internal/graph/snippet.go`:

Add the field (after `Found`):

```go
type SnippetResult struct {
	Found         bool
	Path          string // repo-relative file of the returned definition ("" on a miss)
	StartLine     int
	EndLine       int
	QualifiedName string
	Snippet       string
}
```

In `Engine.Snippet` (line ~291), include the path in the success return:

```go
	return SnippetResult{Found: true, Path: path, StartLine: sl, EndLine: el, QualifiedName: qn, Snippet: snippet}, nil
```

In `Engine.overloadSnippet` (line ~174), include it too (isOverloadSet guarantees one file):

```go
	return SnippetResult{
		Found: true, Path: cands[0].Path, StartLine: startLine, EndLine: endLine,
		QualifiedName: cands[0].QualifiedName, Snippet: b.String(),
	}, nil, nil
```

(`SnippetByName`'s unique-hit case delegates to `e.Snippet`, so it inherits the path.)

- [ ] **Step 4: Run engine tests**

Run: `go test ./internal/graph/ -run 'TestSnippetCarriesPath|TestOverloadSnippetCarriesPath' -v`
Expected: PASS

- [ ] **Step 5: Write the failing MCP test** — append to `internal/mcp/graph_test.go`:

```go
func TestGetCodeSnippetHandlerReturnsPath(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbol: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Found || out.Path != "a.go" {
		t.Fatalf("got found=%v path=%q, want found=true path=a.go", out.Found, out.Path)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/mcp/ -run TestGetCodeSnippetHandlerReturnsPath -v`
Expected: FAIL — `out.Path undefined`

- [ ] **Step 7: Implement the MCP mapping** in `internal/mcp/graph.go`:

Add to `GetCodeSnippetOutput` (after `Found`):

```go
	Path string `json:"path,omitempty"` // repo-relative file of the returned definition
```

Map it in **both** branches of `getCodeSnippet`:

```go
		out := GetCodeSnippetOutput{
			Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
			QualifiedName: r.QualifiedName, Snippet: r.Snippet,
		}
```

and

```go
	return GetCodeSnippetOutput{
		Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
		QualifiedName: r.QualifiedName, Snippet: r.Snippet,
	}, nil
```

- [ ] **Step 8: Update the tool description** in `internal/mcp/server.go` — in the `get_code_snippet` description string, change the sentence
`Returns found=false (not an error) when the symbol is unknown, plus its line range.`
to
`Returns the definition's path and line range, or found=false (not an error) when the symbol is unknown.`

- [ ] **Step 9: Run both packages**

Run: `go test ./internal/graph/ ./internal/mcp/`
Expected: PASS (if any existing test constructs `SnippetResult` literals positionally, fix it to named fields)

- [ ] **Step 10: Commit**

```bash
git add internal/graph/snippet.go internal/graph/snippet_test.go internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go
git commit -m "feat(graph): snippet results carry the definition's path"
```

---

### Task 3: Go type snippets are exact source (include the `type` keyword)

`internal/parse/queries/go.scm:3` captures the `type_spec` node as the definition, so a Go type's recorded byte span starts at the identifier — `get_code_snippet` returns `Opts struct {…}`, which is not valid Go and can't be pasted as an Edit `old_string`. Widen the span to the enclosing `type_declaration` when (and only when) it declares a single spec; grouped `type ( A …; B … )` blocks keep per-spec spans so each name keeps a distinct location.

**Files:**
- Modify: `internal/parse/parse.go:98-139`
- Test: `internal/graph/snippet_test.go` (exercises the parser through the real index+snippet pipeline)

- [ ] **Step 1: Write the failing tests** — append to `internal/graph/snippet_test.go`:

```go
func TestGoTypeSnippetIncludesTypeKeyword(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "t.go", "package p\n\n// Opts configures things.\ntype Opts struct {\n\tA int\n}\n")

	r, err := e.Snippet(ctx, wt, "t.go", "Opts")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found {
		t.Fatal("Opts not found")
	}
	if !strings.HasPrefix(r.Snippet, "type Opts struct") {
		t.Errorf("snippet = %q, want it to start with `type Opts struct`", r.Snippet)
	}
}

func TestGoGroupedTypeSpecsKeepPerSpecSpans(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "t.go", "package p\n\ntype (\n\tA struct{ X int }\n\tB struct{ Y int }\n)\n")

	r, err := e.Snippet(ctx, wt, "t.go", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Found {
		t.Fatal("A not found")
	}
	if strings.Contains(r.Snippet, "B struct") {
		t.Errorf("snippet = %q: grouped specs must stay per-spec, not widen to the whole block", r.Snippet)
	}
}
```

- [ ] **Step 2: Run them to verify the first fails**

Run: `go test ./internal/graph/ -run 'TestGoTypeSnippet|TestGoGroupedTypeSpecs' -v`
Expected: `TestGoTypeSnippetIncludesTypeKeyword` FAILs (snippet starts at `Opts struct`); the grouped test PASSes (it pins current behavior so the fix can't regress it)

- [ ] **Step 3: Implement the parser widening** in `internal/parse/parse.go`.

In `ParseFile`, keep a handle on the definition node. Add `defNode` to the per-match declarations (line ~99):

```go
		var name, recv, recvType, recvName, bindName, bindType string
		var defKind, refKind string
		var sb, eb, sl, el uint
		var nameByte, nameLine uint
		var recvNameByte, bindByte uint
		var defNode *sitter.Node
```

Capture it in the `definition.` case:

```go
			case strings.HasPrefix(cn, "definition."):
				defKind = strings.TrimPrefix(cn, "definition.")
				n := c.Node
				defNode = &n
				sb, eb = c.Node.StartByte(), c.Node.EndByte()
				sl, el = c.Node.StartPosition().Row+1, c.Node.EndPosition().Row+1
```

Widen in the symbol-append branch (line ~134), before `res.Symbols = append(…)`:

```go
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
			res.Symbols = append(res.Symbols, Symbol{
```

Add the helper at file scope (after `edgeKind`):

```go
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
```

(If the tree-sitter binding's method names differ — e.g. `ChildCount`/`Child` variants — adapt to the binding's actual API; the intent is "count `type_spec` children of the parent".)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/graph/ -run 'TestGoTypeSnippet|TestGoGroupedTypeSpecs' -v`
Expected: both PASS

- [ ] **Step 5: Run the full suite — expectation updates are part of this task**

Run: `go test ./...`
Any test that asserts a Go type's snippet text, `start_byte`, or outline span without the `type ` prefix now fails; update those expectations to the widened span (the new text is the correct one). Re-run until green.

- [ ] **Step 6: Commit**

```bash
git add internal/parse/parse.go internal/graph/snippet_test.go
git commit -m "fix(parse): Go type spans include the type keyword (byte-exact snippets)"
```

(Include any test files whose expectations Step 5 updated.)

---

### Task 4: First-class `find_references`

The documented workaround — hand-roll a `\bName\b` regex in `search_content` — proves the missing capability. Add `Engine.References`: graph edges targeting the symbol (the structural truth, with enclosing source symbol and edge kind), plus the word-boundary text sweep for what the graph can't see, deduplicated by `path:line`, every hit flagged `is_test`.

**Files:**
- Create: `internal/graph/references.go`
- Create: `internal/graph/references_test.go`
- Modify: `internal/mcp/graph.go` (new handler + types, at end of file)
- Modify: `internal/mcp/server.go` (register tool)
- Modify: `internal/agents/instructions.go:82-100` (retire the workaround advice)
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing engine tests** — create `internal/graph/references_test.go`:

```go
package graph

import (
	"context"
	"testing"
)

// indexInto does not run cross-file resolve, so the call edge below stays
// unresolved (dst_node_id NULL, dst_name "Target") — exactly the case the
// dst_name fallback must still find, mirroring Implementations.
func TestReferencesFindsUnresolvedCallEdgesByName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "b.go", "package p\nfunc Caller() { Target() }\n")

	refs, meta, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Resolved {
		t.Error("Resolved = false, want true (Target is an indexed node)")
	}
	var edgeHit bool
	for _, r := range refs {
		if r.Path == "b.go" && r.Line == 2 && r.Kind == "calls" {
			edgeHit = true
			if r.From != "Caller" {
				t.Errorf("From = %q, want Caller", r.From)
			}
		}
	}
	if !edgeHit {
		t.Fatalf("no calls-edge reference at b.go:2 in %+v", refs)
	}
}

func TestReferencesTextFallbackAndDedup(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "b.go", "package p\nfunc Caller() { Target() }\n")
	writeFile(t, wt, "README.md", "Target is documented here. TargetFactory is not it.\n")

	refs, _, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	var textHit, factoryHit bool
	siteCount := 0
	for _, r := range refs {
		if r.Path == "README.md" && r.Kind == "text" {
			textHit = true
		}
		if r.Path == "b.go" && r.Line == 2 {
			siteCount++ // the calls edge must suppress the duplicate text match
		}
		if r.Path == "README.md" && r.Line == 1 && r.Kind == "text" &&
			r.Text == "TargetFactory is not it." {
			factoryHit = true
		}
	}
	if !textHit {
		t.Errorf("no text reference from README.md in %+v", refs)
	}
	if siteCount != 1 {
		t.Errorf("b.go:2 appears %d times, want 1 (edge hit dedupes the text match)", siteCount)
	}
	_ = factoryHit // word-boundary: \bTarget\b matches "Target …" but a substring would also flood on TargetFactory
}

func TestReferencesFlagsTestFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "a_test.go", "package p\nfunc helper() { Target() }\n")

	refs, _, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Path == "a_test.go" && !r.IsTest {
			t.Errorf("reference in a_test.go not flagged IsTest: %+v", r)
		}
	}
}
```

(`writeFile` is the existing test helper `indexInto` already uses.)

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/graph/ -run TestReferences -v`
Expected: FAIL — `e.References undefined`

- [ ] **Step 3: Implement the engine** — create `internal/graph/references.go`:

```go
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
func (e *Engine) References(ctx context.Context, worktree, symbol string, limit int) ([]Reference, ReferencesMeta, error) {
	var meta ReferencesMeta
	if limit <= 0 {
		limit = 50
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, meta, nil
	}
	leaf := symbol
	if i := lastDot(symbol); i >= 0 {
		leaf = symbol[i+1:]
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

	rows, err := e.st.DB.QueryContext(ctx, `
SELECT f.path, e2.line, e2.kind, COALESCE(sn.qualified_name, '')
  FROM edges e2
  JOIN files f ON f.id = e2.file_id
  LEFT JOIN nodes sn ON sn.id = e2.src_node_id
 WHERE f.worktree = ?
   AND ( e2.dst_name = ?
         OR e2.dst_node_id IN (
              SELECT n.id FROM nodes n JOIN files f2 ON f2.id = n.file_id
               WHERE f2.worktree = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?)) )
 ORDER BY f.path, e2.line
 LIMIT ?`,
		worktree, leaf, worktree, symbol, symbol, suffix, limit+1)
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

	if len(refs) < limit {
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
			refs = append(refs, Reference{
				Path: m.Path, Line: m.Line, Kind: "text",
				From: m.Symbol, Text: m.Text, IsTest: isTestPath(m.Path),
			})
		}
	}
	if len(refs) > limit {
		refs = refs[:limit]
		meta.Truncated = true
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return refs, meta, nil
}
```

- [ ] **Step 4: Run the engine tests**

Run: `go test ./internal/graph/ -run TestReferences -v`
Expected: PASS (note: the text sweep legitimately also matches the definition line `a.go:2` — the tests assert presence/dedup, not an exact count)

- [ ] **Step 5: Write the failing MCP test** — append to `internal/mcp/graph_test.go`:

```go
func TestFindReferencesHandler(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "b.go"), []byte("package p\nfunc Caller() { Target() }\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "README.md"), []byte("Target is documented here.\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := findReferences(context.Background(), a, FindReferencesInput{Symbol: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Resolved {
		t.Error("Resolved = false, want true")
	}
	var call, text bool
	for _, r := range out.References {
		if r.Path == "b.go" && r.Kind == "calls" {
			call = true
		}
		if r.Path == "README.md" && r.Kind == "text" {
			text = true
		}
	}
	if !call || !text {
		t.Fatalf("want a calls ref from b.go and a text ref from README.md, got %+v", out.References)
	}
	if out.Count != len(out.References) {
		t.Errorf("Count = %d, want %d", out.Count, len(out.References))
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/mcp/ -run TestFindReferencesHandler -v`
Expected: FAIL — `undefined: findReferences`

- [ ] **Step 7: Implement the MCP handler** — append to `internal/mcp/graph.go`:

```go
type FindReferencesInput struct {
	Symbol string `json:"symbol" jsonschema:"the symbol whose references to find (bare, qualified, or Type.Method form)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max references (default 50)"`
}
type ReferenceOut struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Kind   string `json:"kind"`              // calls|imports|extends|implements|annotates|references|text
	From   string `json:"from,omitempty"`    // enclosing/referencing symbol, "" at file scope
	Text   string `json:"text,omitempty"`    // matching line (text refs only)
	IsTest bool   `json:"is_test,omitempty"` // reference lives in test code
}
type FindReferencesOutput struct {
	// Resolved reports whether the symbol matched a graph node; false means every
	// hit below is text-only (the symbol may be misspelled, or live outside the
	// indexed languages).
	Resolved   bool           `json:"resolved"`
	Count      int            `json:"count"`
	Truncated  bool           `json:"truncated,omitempty"`
	References []ReferenceOut `json:"references"`
}

func findReferences(ctx context.Context, a *app.App, in FindReferencesInput) (FindReferencesOutput, error) {
	ensureFresh(ctx, a)
	refs, meta, err := a.Graph.References(ctx, a.Git.Worktree, in.Symbol, in.Limit)
	if err != nil {
		return FindReferencesOutput{}, err
	}
	out := FindReferencesOutput{
		Resolved: meta.Resolved, Count: len(refs), Truncated: meta.Truncated,
		References: make([]ReferenceOut, 0, len(refs)),
	}
	for _, r := range refs {
		out.References = append(out.References, ReferenceOut{
			Path: r.Path, Line: r.Line, Kind: r.Kind,
			From: r.From, Text: r.Text, IsTest: r.IsTest,
		})
	}
	return out, nil
}
```

- [ ] **Step 8: Register the tool** in `internal/mcp/server.go`, after the `find_annotated` registration (line ~69):

```go
	addTool(s, a, "find_references",
		"Find every place a symbol is used — call sites, type usages, imports, annotations — from the graph's edges, plus a word-boundary text sweep for references the graph cannot see (comments, config, unparsed files, dynamically-bound sites), deduplicated by path:line. Each hit carries its file/line, the referencing symbol (`from`), an edge kind (`calls`, `extends`, … or `text` for grep-only hits), and `is_test` so test usages are tell-apart-able at a glance. `resolved` reports whether the symbol matched a graph node at all — false means hits are text-only. Prefer this over hand-rolling a `\\bName\\b` regex in search_content: one call, structurally ranked, already deduplicated.",
		findReferences)
```

- [ ] **Step 9: Run the MCP tests**

Run: `go test ./internal/mcp/ -run TestFindReferencesHandler -v`
Expected: PASS

- [ ] **Step 10: Update the agent instructions** in `internal/agents/instructions.go`.

Replace the workaround tail of the search_content bullet (note the Go string-concat quoting — match the source exactly):

old:

```
  It does full regex too: set ` + "`regex=true`" + ` (` + "`ignore_case`" + ` for case-folding). For
  precise *references* to a type/symbol, prefer a word-boundary regex (` + "`\\bName\\b`" + `)
  over a bare substring, which also matches ` + "`NameFactory`, `GetName`" + `, etc.
```

new:

```
  It does full regex too: set ` + "`regex=true`" + ` (` + "`ignore_case`" + ` for case-folding). For
  *references* to a type/symbol, use ` + "`find_references`" + ` instead of a hand-rolled
  regex — it is the structural answer with a text sweep built in.
```

Then add a new bullet right after the find_implementations bullet (anchor: `` `search_graph` `` + `` ` can't surface them.` ``):

```
- **Find references** to a symbol with ` + "`find_references`" + ` — call sites, type
  usages, imports and annotations from the graph's edges, plus a word-boundary
  text sweep for what the graph cannot see, deduplicated by path:line. Each hit
  carries its referencing symbol, an edge kind (or ` + "`text`" + ` for grep-only hits),
  and ` + "`is_test`" + `. Prefer it over a ` + "`\\bName\\b`" + ` regex in ` + "`search_content`" + `.
```

- [ ] **Step 11: Run everything touched**

Run: `go test ./internal/graph/ ./internal/mcp/ ./internal/agents/`
Expected: PASS (if an agents test asserts the full instructions text, update its expectation)

- [ ] **Step 12: Commit**

```bash
git add internal/graph/references.go internal/graph/references_test.go internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go internal/agents/instructions.go
git commit -m "feat(graph): first-class find_references — edges + word-boundary text sweep"
```

---

### Task 5: Trace hops carry `is_test`; all-test-callers hint

An all-test caller list with no marking reads as "dead production code". Carry the existing classification onto every hop, and when every caller of a symbol is test code, say so in `hint` and point at `find_references` (which Task 4 just shipped — this ordering is deliberate).

**Files:**
- Modify: `internal/mcp/graph.go:125-204`
- Modify: `internal/mcp/server.go:46-48` (tool description)
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing test** — append to `internal/mcp/graph_test.go`:

```go
func TestTraceCallersFlagTestsAndHintWhenAllTest(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Target() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "a_test.go"), []byte("package p\nfunc helperTest() { Target() }\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "Target", Direction: "callers"})
	if err != nil {
		t.Fatal(err)
	}
	var sawTestCaller bool
	for _, hops := range out.Chains {
		if len(hops) < 2 {
			continue
		}
		if hops[0].Symbol == "helperTest" {
			sawTestCaller = true
			if !hops[0].IsTest {
				t.Error("caller hop in a_test.go not flagged is_test")
			}
			if hops[len(hops)-1].IsTest {
				t.Error("Target (a.go) wrongly flagged is_test")
			}
		}
	}
	if !sawTestCaller {
		t.Fatalf("no caller chain from helperTest in %+v", out.Chains)
	}
	if !strings.Contains(out.Hint, "test code") || !strings.Contains(out.Hint, "find_references") {
		t.Errorf("hint = %q, want all-test-callers steer toward find_references", out.Hint)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/mcp/ -run TestTraceCallersFlagTestsAndHintWhenAllTest -v`
Expected: FAIL — `hops[0].IsTest undefined`

- [ ] **Step 3: Implement** in `internal/mcp/graph.go`.

Add to `TraceHop`:

```go
	IsTest bool `json:"is_test,omitempty"` // hop lives in test code
```

Set it when building hops (inside `traceCallPath`'s chain loop):

```go
			hops[i] = TraceHop{
				Symbol: n.Name, Path: n.Path, Line: n.Line,
				Resolution: n.Resolution, Confidence: n.Confidence, Receiver: n.Receiver,
				IsTest: graph.IsTestPath(n.Path),
			}
```

After the existing dead-end hint block (line ~202), add:

```go
	// All callers are tests: without a marker this reads as "dead production
	// code", when the truth is usually a wrapper or sibling API the trace can't
	// see (e.g. SearchContent vs SearchContentWithMeta). Say so and steer to
	// find_references, which sees edges and text.
	if dir == "callers" && out.Hint == "" {
		anyCaller, allTest := false, true
		for _, hops := range out.Chains {
			if len(hops) < 2 {
				continue
			}
			anyCaller = true
			for _, h := range hops[:len(hops)-1] {
				if !h.IsTest {
					allTest = false
				}
			}
		}
		if anyCaller && allTest {
			leaf := leafName(in.Symbol)
			out.Hint = fmt.Sprintf("every caller found is test code — production may reach %s through a wrapper or sibling API the trace can't see. Use find_references(%q) for non-test references.", leaf, leaf)
		}
	}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/mcp/ -run TestTraceCallersFlagTestsAndHintWhenAllTest -v`
Expected: PASS

- [ ] **Step 5: Update the trace tool description** in `internal/mcp/server.go` — in the `trace_call_path` description, after the sentence ending `…treat low/name hops as candidates to verify with get_code_snippet.`, insert:

`Hops in test files carry is_test; when every caller of a symbol is test code the hint says so (production usually reaches it through a wrapper — find_references sees what the trace cannot).`

- [ ] **Step 6: Run the package**

Run: `go test ./internal/mcp/`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go
git commit -m "feat(mcp): trace hops carry is_test; hint when all callers are tests"
```

---

### Task 6: Batch `get_code_snippet` (`symbols` param)

Round trips are the LLM's scarcest resource: after a search returns 3 relevant hits, fetching them is 3 sequential calls. Accept `symbols` (≤10) and return per-entry results; overflow is reported in `skipped`, never silently dropped.

**Files:**
- Modify: `internal/mcp/graph.go:348-396`
- Modify: `internal/mcp/server.go:58-60` (tool description)
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing tests** — append to `internal/mcp/graph_test.go`:

```go
func TestGetCodeSnippetBatch(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Alpha() {}\nfunc Beta() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: []string{"Alpha", "Beta", "NoSuch"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Snippets) != 3 {
		t.Fatalf("got %d batch entries, want 3: %+v", len(out.Snippets), out.Snippets)
	}
	if out.Snippets[0].Symbol != "Alpha" || !out.Snippets[0].Found || out.Snippets[0].Path != "a.go" {
		t.Errorf("entry 0 = %+v, want found Alpha in a.go", out.Snippets[0])
	}
	if !strings.Contains(out.Snippets[1].Snippet, "func Beta()") {
		t.Errorf("entry 1 snippet = %q, want Beta's source", out.Snippets[1].Snippet)
	}
	if out.Snippets[2].Found {
		t.Errorf("entry 2 (NoSuch) reported found")
	}
}

func TestGetCodeSnippetBatchCapReportsSkipped(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Alpha() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	syms := make([]string, 12)
	for i := range syms {
		syms[i] = "Alpha"
	}
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: syms})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Snippets) != 10 || len(out.Skipped) != 2 {
		t.Fatalf("got %d snippets / %d skipped, want 10 / 2 (no silent caps)", len(out.Snippets), len(out.Skipped))
	}
}

func TestGetCodeSnippetNoSymbolErrors(t *testing.T) {
	a := newTestApp(t, "main")
	a.Git.Worktree = t.TempDir()
	if _, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{}); err == nil {
		t.Fatal("empty input should error, not return an empty result")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/mcp/ -run TestGetCodeSnippet -v`
Expected: FAIL — `unknown field Symbols` / `out.Snippets undefined`

- [ ] **Step 3: Implement** in `internal/mcp/graph.go`. Replace the input/output types and handler with:

```go
type GetCodeSnippetInput struct {
	File    string   `json:"file,omitempty" jsonschema:"repo-relative file path. Optional: omit it to resolve the symbol by name across the repo — the common case right after a search returns a name."`
	Symbol  string   `json:"symbol,omitempty" jsonschema:"symbol name or qualified name"`
	Symbols []string `json:"symbols,omitempty" jsonschema:"up to 10 symbols to fetch in one call — the batch form of symbol, resolved identically and returned in order under snippets. Use after a search returns several relevant names instead of one call per symbol."`
}
type SnippetCandidate struct {
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	Kind          string `json:"kind,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
}
type GetCodeSnippetOutput struct {
	Found         bool   `json:"found"`
	Path          string `json:"path,omitempty"` // repo-relative file of the returned definition
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Snippet       string `json:"snippet,omitempty"`
	// Candidates is populated only when file was omitted and the bare name matched
	// several definitions: Found is false, and the agent should re-query with a
	// qualified_name (usually unique) or by supplying the file.
	Candidates []SnippetCandidate `json:"candidates,omitempty"`
	// Snippets holds the per-symbol results of a batch (`symbols`) call, in input
	// order; the top-level fields above stay zero. Skipped lists symbols beyond
	// the batch cap that were NOT fetched — re-call with them rather than
	// assuming coverage.
	Snippets []BatchSnippet `json:"snippets,omitempty"`
	Skipped  []string       `json:"skipped,omitempty"`
}
type BatchSnippet struct {
	Symbol        string             `json:"symbol"` // as requested
	Found         bool               `json:"found"`
	Path          string             `json:"path,omitempty"`
	StartLine     int                `json:"start_line,omitempty"`
	EndLine       int                `json:"end_line,omitempty"`
	QualifiedName string             `json:"qualified_name,omitempty"`
	Snippet       string             `json:"snippet,omitempty"`
	Candidates    []SnippetCandidate `json:"candidates,omitempty"`
}

// maxBatchSnippets caps one batch call so a pathological symbol list cannot blow
// the token budget; overflow is reported in Skipped, never silently dropped.
const maxBatchSnippets = 10

// snippetForSymbol is the single-symbol body shared by the scalar and batch
// forms of getCodeSnippet.
func snippetForSymbol(ctx context.Context, a *app.App, file, symbol string) (GetCodeSnippetOutput, error) {
	if file == "" {
		r, cands, err := a.Graph.SnippetByName(ctx, a.Git.Worktree, symbol)
		if err != nil {
			return GetCodeSnippetOutput{}, err
		}
		out := GetCodeSnippetOutput{
			Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
			QualifiedName: r.QualifiedName, Snippet: r.Snippet,
		}
		for _, c := range cands {
			out.Candidates = append(out.Candidates, SnippetCandidate{
				Path: c.Path, StartLine: c.StartLine, Kind: c.Kind, QualifiedName: c.QualifiedName,
			})
		}
		return out, nil
	}
	r, err := a.Graph.Snippet(ctx, a.Git.Worktree, file, symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	return GetCodeSnippetOutput{
		Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
		QualifiedName: r.QualifiedName, Snippet: r.Snippet,
	}, nil
}

func getCodeSnippet(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	ensureFresh(ctx, a)
	if len(in.Symbols) > 0 {
		var out GetCodeSnippetOutput
		batch := in.Symbols
		if len(batch) > maxBatchSnippets {
			out.Skipped = batch[maxBatchSnippets:]
			batch = batch[:maxBatchSnippets]
		}
		for _, sym := range batch {
			one, err := snippetForSymbol(ctx, a, in.File, sym)
			if err != nil {
				return GetCodeSnippetOutput{}, err
			}
			out.Snippets = append(out.Snippets, BatchSnippet{
				Symbol: sym, Found: one.Found, Path: one.Path,
				StartLine: one.StartLine, EndLine: one.EndLine,
				QualifiedName: one.QualifiedName, Snippet: one.Snippet,
				Candidates: one.Candidates,
			})
		}
		return out, nil
	}
	if in.Symbol == "" {
		return GetCodeSnippetOutput{}, fmt.Errorf("pass symbol (one) or symbols (batch)")
	}
	return snippetForSymbol(ctx, a, in.File, in.Symbol)
}
```

(Task 2's Path mapping is already inside `snippetForSymbol` — this refactor must keep it.)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/mcp/ -run TestGetCodeSnippet -v`
Expected: PASS (including Task 2's `TestGetCodeSnippetHandlerReturnsPath`)

- [ ] **Step 5: Update the tool description** in `internal/mcp/server.go` — append to the `get_code_snippet` description:

` Fetch several symbols in one call with symbols (≤10; overflow is listed in skipped, in input order under snippets) — one call replaces N when a search returned several relevant hits.`

- [ ] **Step 6: Run the package**

Run: `go test ./internal/mcp/`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go
git commit -m "feat(mcp): batch get_code_snippet via symbols param (cap 10, explicit skipped)"
```

---

### Task 7: `offset` paging on `search_content`

Today's recovery from `truncated` is "raise `limit` and re-pay the whole page". The result order is already deterministic (path order, hand-written before generated), so an `offset` makes paging cheap and honest.

**Files:**
- Modify: `internal/graph/content.go` (`ContentSearchOpts`, `SearchContentWithMeta`)
- Modify: `internal/mcp/graph.go:298-344` (`SearchContentInput`, handler)
- Modify: `internal/mcp/server.go:55-57` (tool description)
- Test: `internal/graph/content_test.go`, `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing engine test** — append to `internal/graph/content_test.go`:

```go
func TestSearchContentOffsetPaging(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "t.go", "package p\n// needle one\n// needle two\n// needle three\nfunc F() {}\n")

	page1, m1, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || !m1.Truncated || m1.Total != 3 {
		t.Fatalf("page1: got %d matches, truncated=%v, total=%d; want 2/true/3", len(page1), m1.Truncated, m1.Total)
	}

	page2, m2, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].Line != 4 {
		t.Fatalf("page2: got %+v, want exactly the third match (line 4)", page2)
	}
	if m2.Truncated {
		t.Error("page2 truncated = true, want false (offset+shown covers total)")
	}

	empty, _, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10, Offset: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("overshoot offset: got %+v, want empty (not an error)", empty)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/graph/ -run TestSearchContentOffsetPaging -v`
Expected: FAIL — `unknown field Offset in struct literal`

- [ ] **Step 3: Implement the engine change** in `internal/graph/content.go`.

Add to `ContentSearchOpts` (after `MaxPerFile`):

```go
	Offset int // skip the first N matches of the stable result order (paging)
```

In `SearchContentWithMeta`, replace the final cap block

```go
	out := append(hand, gen...)
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	meta.Shown = len(out)
	meta.Truncated = meta.Total > meta.Shown
```

with

```go
	out := append(hand, gen...)
	// Offset pages through the post-cap ordering (hand-written before generated,
	// path order) — the same stable order every call returns, so offset+limit
	// walks it without overlap. Overshooting is an empty page, not an error.
	if opts.Offset > 0 {
		if opts.Offset >= len(out) {
			out = out[:0]
		} else {
			out = out[opts.Offset:]
		}
	}
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	meta.Shown = len(out)
	meta.Truncated = meta.Total > opts.Offset+meta.Shown
```

- [ ] **Step 4: Run the engine tests**

Run: `go test ./internal/graph/ -run TestSearchContent -v`
Expected: PASS (existing zero-offset tests keep their semantics: `Offset==0` reduces to the old expressions)

- [ ] **Step 5: Write the failing MCP test** — append to `internal/mcp/graph_test.go`:

```go
func TestSearchContentHandlerOffset(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\n// needle one\n// needle two\nfunc F() {}\n"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "needle", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Matches[0].Line != 3 {
		t.Fatalf("got %+v, want exactly the second match (line 3)", out.Matches)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/mcp/ -run TestSearchContentHandlerOffset -v`
Expected: FAIL — `unknown field Offset`

- [ ] **Step 7: Implement the MCP mapping** in `internal/mcp/graph.go`.

Add to `SearchContentInput` (after `MaxPerFile`):

```go
	Offset int `json:"offset,omitempty" jsonschema:"skip the first N matches of the stable result order — page through a truncated result with offset+limit instead of re-fetching with a bigger limit"`
```

Pass it through in `searchContent`:

```go
	res, meta, err := a.Graph.SearchContentWithMeta(ctx, a.Git.Worktree, in.Pattern, graph.ContentSearchOpts{
		Regex: in.Regex, IgnoreCase: in.IgnoreCase, PathGlob: in.PathGlob,
		Language: in.Language, Limit: in.Limit, MaxPerFile: in.MaxPerFile, Offset: in.Offset,
	})
```

- [ ] **Step 8: Update the tool description** in `internal/mcp/server.go` — in the `search_content` description, change
`raise `limit`/`max_per_file` or narrow the filters to see the rest, rather than treating a capped page as complete.`
to
`page deeper with offset+limit (the result order is stable) or narrow the filters, rather than treating a capped page as complete.`

- [ ] **Step 9: Run both packages**

Run: `go test ./internal/graph/ ./internal/mcp/`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add internal/graph/content.go internal/graph/content_test.go internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go
git commit -m "feat(graph): offset paging on search_content (stable-order pages)"
```

---

### Task 8: Instructions sweep, full verification

**Files:**
- Modify: `internal/agents/instructions.go`
- Verify: whole repo

- [ ] **Step 1: Teach the snippet/path/batch behavior** in `internal/agents/instructions.go` — in the get_code_snippet bullet, after the sentence ending `` come back together in a single snippet — no disambiguation needed. ``, append:

```
  Every snippet carries its ` + "`path`" + ` and line range — cite it as ` + "`path:line`" + ` and
  edit without a second lookup. Need several symbols after one search? Pass
  ` + "`symbols`" + ` (≤10) and get them all in one call; anything beyond the cap is
  listed in ` + "`skipped`" + `, never silently dropped.
```

(Match the file's Go string-concat quoting style exactly, as in Task 4 Step 10.)

- [ ] **Step 2: Run the agents package**

Run: `go test ./internal/agents/`
Expected: PASS (update any full-text instruction expectations)

- [ ] **Step 3: Full verification**

```bash
gofmt -l internal/ && go vet ./... && go test ./...
```

Expected: `gofmt -l` prints nothing; vet clean; all tests PASS.

- [ ] **Step 4: Smoke-test as the real user** — rebuild and exercise the new surface against this repo itself:

```bash
go build -o /tmp/recarto ./cmd/recarto
```

Then via the MCP tools (or equivalent CLI graph commands): `get_code_snippet ContentSearchOpts` must return `path: internal/graph/content.go` and a snippet starting `type ContentSearchOpts struct` (after a reindex picks up the parser change); `trace_call_path Engine.SearchContent callers` must flag the 8 test callers `is_test: true` and emit the all-test-callers hint; `find_references Engine.SearchContentWithMeta` must list the call in `internal/mcp/graph.go`.

- [ ] **Step 5: Record the learning**

Record a recarto memory (kind `learning`): title "LLM-UX round 1: snippet path/span, is_test traces, find_references, batch snippets, offset paging", body summarizing what shipped and that the two filed issues (`019eb160-aa50…`, `019eb160-c5b2…`) are addressed, with refs to `internal/graph/references.go`, `internal/graph/snippet.go`, `internal/mcp/graph.go`.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/instructions.go
git commit -m "docs(agents): teach snippet path/batch + find_references in agent instructions"
```

(Include any expectation files updated in Step 2.)

---

## Self-review notes

- **Spec coverage:** snippet path (Task 2), exact spans (Task 3), test-caller annotation + misread hint (Task 5), find_references (Task 4), batch snippets (Task 6), offset paging (Task 7), guidance updates (Tasks 4, 8). Deferred items are listed up top with reasons.
- **Ordering constraint:** Task 4 (find_references) must land before Task 5 — the trace hint names the tool. Task 1 must land before Task 5 (`graph.IsTestPath`). Task 2 must land before Task 6 (the batch refactor carries `Path`).
- **Known judgment calls an executor may exercise:** the tree-sitter binding's exact Node API names in Task 3 (intent is fixed, method names may need adapting); the C# qualified-name form in Task 2's overload test (if `C.Run` doesn't resolve as written, inspect the indexed qualified name and adjust the lookup string — the assertion semantics stay).
