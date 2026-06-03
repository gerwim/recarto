# Recarto Phase 2 — Code Graph — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a worktree-aware tree-sitter code graph (symbols + relationships) to Recarto's per-repo SQLite store, with five structural query tools over MCP/CLI/hooks, kept in sync by lazy reconcile + hook-incremental indexing.

**Architecture:** Two new packages — `internal/parse` (all CGO/tree-sitter confined here, behind a safe must-Close wrapper) and `internal/graph` (engine mirroring `internal/memory.Engine`) — plus a third SQLite migration and extensions to `internal/app`, `internal/cli`, `internal/mcp`, and `internal/agents/claudecode`. Everything reuses Phase 1's store, `WriteTx` concurrency discipline, `ledger`, `clock`, `ids`, and `gitctx`.

**Tech Stack:** Go 1.26.4 (now **`CGO_ENABLED=1`**), official tree-sitter Go binding `github.com/tree-sitter/go-tree-sitter` + grammar modules for C#/Go/TypeScript/JavaScript, `modernc.org/sqlite` (retained), `zig cc` for cross-compilation.

**Conventions reused (do not reinvent):**
- Store: `store.Store{DB}`, `store.WriteTx(ctx, func(c store.Conn) error)` runs `BEGIN IMMEDIATE` on a serialized connection. Reads use `st.DB`. **`ledger.Incr` opens its own `WriteTx` — never call it inside an enclosing `WriteTx` (non-reentrant mutex → deadlock).**
- Ids: `ids.New()` → UUIDv7. Clock: `e.clk.Now().UnixMilli()`.
- FTS5: `bm25()` returns **negative** scores (more negative = more relevant); `ORDER BY <bm25> ASC` and flip the sign before exposing. FTS rows are **manually** synced (no external-content link), like `memories_fts`.
- CLI command shape: `func Name(ctx, *app.App, io.Writer, args []string) error`; flag commands use `parseFlags(fs, args)`; register in `cli.known()` + the `cli.Run` switch.
- MCP: add tools in `server.go` via `addTool(s, a, name, desc, handler)`; handlers are SDK-free `func(ctx, *app.App, In) (Out, error)` with `json`+`jsonschema` struct tags (`,omitempty` ⇒ optional).
- Hooks: exit-0 always; `writeHookContext(out, event, text)` emits `hookSpecificOutput.additionalContext`.

**Build/test note:** every `go` command in this plan runs with the module toolchain (go1.26.4) and CGO enabled. If `CGO_ENABLED` is unset in the environment, prefix commands with `CGO_ENABLED=1`. A C compiler (`cc`/`gcc`/`clang`) must be installed.

---

## File Structure

**Create:**
- `internal/parse/registry.go` — language registry, `LangForExt`, compiled-query cache, `NewRegistry`/`Close`.
- `internal/parse/parse.go` — `Symbol`/`Edge`/`Result` types + `ParseFile` (the only place that touches tree-sitter C objects).
- `internal/parse/queries.go` — `//go:embed` of the four `.scm` query files + the lang→query map.
- `internal/parse/queries/{go,csharp,typescript,javascript}.scm` — tag/edge queries.
- `internal/parse/parse_test.go`, `internal/parse/close_test.go` — golden + must-Close tests.
- `internal/graph/graph.go` — `Engine`, `New`, `Close`, shared helpers (`sha256hex`, `enclosing`, `nullStr`).
- `internal/graph/source.go` — `FileEntry`, `Source`, `FSSource`.
- `internal/graph/index.go` — `IndexFile` + `writeFileGraph`.
- `internal/graph/reconcile.go` — `Reconcile`.
- `internal/graph/resolve.go` — `Resolve` (name → `dst_node_id`).
- `internal/graph/search.go`, `snippet.go`, `trace.go`, `architecture.go`, `query.go` — the five query methods.
- `internal/graph/*_test.go` — per-method tests + concurrency test.
- `internal/cli/index.go` — `recarto index`.
- `internal/cli/graph.go` — `recarto graph <sub>`.
- `internal/mcp/graph.go` — graph tool In/Out structs + handlers.
- `internal/agents/claudecode/graphhooks.go` — `PostEdit`, `Augment` handlers.

**Modify:**
- `internal/store/migrate.go` (+ `migrate_test.go`) — third migration.
- `internal/app/app.go` (+ `app_test.go`) — build/close the graph engine.
- `internal/cli/cli.go` — register `index` + `graph`; `internal/cli/hook.go` — dispatch `post-edit` + `augment`.
- `internal/mcp/server.go` — register the five graph tools.
- `internal/agents/claudecode/hooks.go` — extend `SessionStart` to reconcile; `install.go` — two new `hookSpec`s.
- `go.mod` / `go.sum` — grammar dependencies.
- `.github/workflows/ci.yml` — CGO toolchain + cross-compile smoke.

---

## Task 1: Research spike — pin grammar versions and smoke-compile a parse

**Files:**
- Create (temporary, deleted at end of task): `internal/parse/spike_test.go`

- [ ] **Step 1: Add the binding + four grammar modules**

Run:
```bash
CGO_ENABLED=1 go get github.com/tree-sitter/go-tree-sitter@latest
CGO_ENABLED=1 go get github.com/tree-sitter/tree-sitter-go/bindings/go@latest
CGO_ENABLED=1 go get github.com/tree-sitter/tree-sitter-c-sharp/bindings/go@latest
CGO_ENABLED=1 go get github.com/tree-sitter/tree-sitter-typescript/bindings/go@latest
CGO_ENABLED=1 go get github.com/tree-sitter/tree-sitter-javascript/bindings/go@latest
```
Expected: `go.mod` gains five `require` lines with concrete pinned versions. Record those exact versions in the commit message (this is the "pin" deliverable).

- [ ] **Step 2: Write a throwaway smoke test that parses one snippet per language**

```go
package parse

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tscs "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

func TestSpikeParsesAllLanguages(t *testing.T) {
	cases := []struct {
		name string
		lang *sitter.Language
		src  string
	}{
		{"go", sitter.NewLanguage(tsgo.Language()), "package p\nfunc F() {}"},
		{"csharp", sitter.NewLanguage(tscs.Language()), "class C { void M() {} }"},
		{"typescript", sitter.NewLanguage(tsts.LanguageTypescript()), "function f(): void {}"},
		{"tsx", sitter.NewLanguage(tsts.LanguageTSX()), "const a = <div/>;"},
		{"javascript", sitter.NewLanguage(tsjs.Language()), "function f() { return <div/>; }"},
	}
	for _, c := range cases {
		p := sitter.NewParser()
		p.SetLanguage(c.lang)
		tree := p.Parse([]byte(c.src), nil)
		root := tree.RootNode()
		t.Logf("%s root: %s", c.name, root.Kind())
		if root.HasError() {
			t.Errorf("%s: parse produced an error tree", c.name)
		}
		tree.Close()
		p.Close()
	}
}
```

- [ ] **Step 3: Run the smoke test (CGO compiles every grammar's C here)**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestSpikeParsesAllLanguages -v`
Expected: PASS, logging a root kind per language (`source_file`, `compilation_unit`, `program`, …). If a grammar fails to compile, resolve the C toolchain now — this is the riskiest step in the phase. If `LanguageTypescript`/`LanguageTSX` are named differently in the pinned version, fix the references (the binding's `pkg.go.dev` page lists the exported funcs).

- [ ] **Step 4: Delete the spike test and commit the pins**

Run: `rm internal/parse/spike_test.go`
```bash
git add go.mod go.sum
git commit -m "build: pin tree-sitter Go binding + C#/Go/TS/JS grammars (CGO)"
```

---

## Task 2: Flip the module to CGO and prove the existing suite still builds

**Files:**
- Test: `internal/store/store_test.go` (existing — used as a build smoke; no edit)

- [ ] **Step 1: Confirm the whole module builds and passes with CGO on**

Run: `CGO_ENABLED=1 go build ./... && CGO_ENABLED=1 go test ./... `
Expected: PASS. The Phase 1 packages are pure Go and are unaffected by enabling CGO; this confirms the toolchain is healthy before we add graph code.

- [ ] **Step 2: Run the race detector once (CGO required for -race)**

Run: `CGO_ENABLED=1 go test -race ./internal/store/ ./internal/memory/`
Expected: PASS, no race output.

- [ ] **Step 3: Commit (no code change; this is a checkpoint)**

No commit needed if nothing changed. If `go.sum` was tidied, run `git add go.sum && git commit -m "build: tidy go.sum after CGO enablement"`.

---

## Task 3: Third migration — files / nodes / edges / nodes_fts

**Files:**
- Modify: `internal/store/migrate.go:10-13` (bump `schemaVersion`; append a migration string)
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/migrate_test.go`:
```go
func TestMigrateCreatesGraphTables(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, table := range []string{"files", "nodes", "edges", "nodes_fts"} {
		var name string
		err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}
	var v int
	st.DB.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 3 {
		t.Errorf("user_version = %d, want 3", v)
	}
}
```
(Ensure `path/filepath` is imported in the test file.)

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/store/ -run TestMigrateCreatesGraphTables -v`
Expected: FAIL (`files` not created; `user_version = 2`).

- [ ] **Step 3: Append the migration**

In `internal/store/migrate.go`, change `const schemaVersion = 2` to `const schemaVersion = 3`, and append a third entry to the `migrations` slice (after the `memories_fts` block, before the closing `}`):
```go
	// v3 (Phase 2): worktree-aware code graph. files is the reconcile diff
	// target; nodes/edges inherit worktree via file_id. nodes_fts is manually
	// synced (FTS5 has no external-content link to TEXT-uuid rowids).
	`
CREATE TABLE files (
  id              TEXT PRIMARY KEY,
  worktree        TEXT NOT NULL,
  path            TEXT NOT NULL,
  language        TEXT NOT NULL,
  content_hash    TEXT NOT NULL,
  mtime           INTEGER NOT NULL,
  size            INTEGER NOT NULL,
  last_indexed_at INTEGER NOT NULL,
  UNIQUE (worktree, path)
);
CREATE INDEX idx_files_worktree ON files(worktree);

CREATE TABLE nodes (
  id             TEXT PRIMARY KEY,
  file_id        TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,
  name           TEXT NOT NULL,
  qualified_name TEXT NOT NULL DEFAULT '',
  signature      TEXT NOT NULL DEFAULT '',
  start_byte     INTEGER NOT NULL,
  end_byte       INTEGER NOT NULL,
  start_line     INTEGER NOT NULL,
  end_line       INTEGER NOT NULL
);
CREATE INDEX idx_nodes_file ON nodes(file_id);
CREATE INDEX idx_nodes_name ON nodes(name);
CREATE INDEX idx_nodes_qname ON nodes(qualified_name);

CREATE TABLE edges (
  id          TEXT PRIMARY KEY,
  src_node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
  dst_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
  dst_name    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  line        INTEGER NOT NULL
);
CREATE INDEX idx_edges_src ON edges(src_node_id, kind);
CREATE INDEX idx_edges_dst ON edges(dst_node_id);
CREATE INDEX idx_edges_dstname ON edges(dst_name);

CREATE VIRTUAL TABLE nodes_fts USING fts5(
  node_id UNINDEXED,
  name,
  qualified_name
);
`,
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/store/ -v`
Expected: PASS (including the existing idempotency test, which now expects `user_version = 3` — update `TestMigrateIsIdempotent` / any hardcoded `2` in `migrate_test.go` to `3`).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): add v3 migration for worktree-aware code graph"
```

---

## Task 4: parse — language registry and extension mapping

**Files:**
- Create: `internal/parse/registry.go`, `internal/parse/queries.go`, `internal/parse/queries/go.scm`
- Test: `internal/parse/registry_test.go`

- [ ] **Step 1: Write the failing test**

`internal/parse/registry_test.go`:
```go
package parse

import "testing"

func TestLangForExt(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go": "go", "x.cs": "csharp", "x.ts": "typescript",
		"x.tsx": "tsx", "x.js": "javascript", "x.jsx": "javascript",
	}
	for path, want := range cases {
		got, ok := LangForExt(path)
		if !ok || got != want {
			t.Errorf("LangForExt(%q) = %q,%v; want %q,true", path, got, ok, want)
		}
	}
	if _, ok := LangForExt("README.md"); ok {
		t.Error("LangForExt(.md) should be unsupported")
	}
}

func TestNewRegistryCompilesGoQuery(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.queries["go"] == nil || r.langs["go"] == nil {
		t.Fatal("go language/query not registered")
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run 'TestLangForExt|TestNewRegistry' -v`
Expected: FAIL (undefined `LangForExt`, `NewRegistry`).

- [ ] **Step 3: Create the Go query file**

`internal/parse/queries/go.scm`:
```scheme
(function_declaration name: (identifier) @name) @definition.function
(method_declaration name: (field_identifier) @name) @definition.method
(type_spec name: (type_identifier) @name) @definition.type
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (selector_expression field: (field_identifier) @name)) @reference.call
(import_spec path: (interpreted_string_literal) @name) @reference.import
```

- [ ] **Step 4: Create `queries.go` (embed) and `registry.go`**

`internal/parse/queries.go`:
```go
package parse

import _ "embed"

//go:embed queries/go.scm
var goQuery string

//go:embed queries/csharp.scm
var csharpQuery string

//go:embed queries/typescript.scm
var typescriptQuery string

//go:embed queries/javascript.scm
var javascriptQuery string

// querySources maps a language id to its compiled-query source. tsx reuses the
// TypeScript query (the tsx grammar shares the same definition/reference nodes).
func querySources() map[string]string {
	return map[string]string{
		"go": goQuery, "csharp": csharpQuery,
		"typescript": typescriptQuery, "tsx": typescriptQuery,
		"javascript": javascriptQuery,
	}
}
```

`internal/parse/registry.go`:
```go
// Package parse turns source files into a normalized symbol/edge graph using
// tree-sitter. It is the ONLY package that links the tree-sitter C library
// (CGO); every C-allocating object (Parser/Tree/Query/QueryCursor) is closed
// here behind ParseFile, so no raw C object escapes the package.
package parse

import (
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tscs "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Registry holds the per-language grammars and pre-compiled queries. Build once
// per process; safe for concurrent ParseFile calls (each creates its own Parser).
type Registry struct {
	langs   map[string]*sitter.Language
	queries map[string]*sitter.Query
}

// NewRegistry compiles every language's query. A malformed query is a programmer
// error surfaced at startup.
func NewRegistry() (*Registry, error) {
	langs := map[string]*sitter.Language{
		"go":         sitter.NewLanguage(tsgo.Language()),
		"csharp":     sitter.NewLanguage(tscs.Language()),
		"typescript": sitter.NewLanguage(tsts.LanguageTypescript()),
		"tsx":        sitter.NewLanguage(tsts.LanguageTSX()),
		"javascript": sitter.NewLanguage(tsjs.Language()),
	}
	queries := map[string]*sitter.Query{}
	for lang, src := range querySources() {
		q, qErr := sitter.NewQuery(langs[lang], src)
		if qErr != nil {
			return nil, fmt.Errorf("parse: compile %s query: %v", lang, qErr)
		}
		queries[lang] = q
	}
	return &Registry{langs: langs, queries: queries}, nil
}

// Close releases the compiled queries (must-Close discipline).
func (r *Registry) Close() {
	for _, q := range r.queries {
		q.Close()
	}
}

// LangForExt returns the language id for a path's extension, or ok=false.
func LangForExt(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go", true
	case ".cs":
		return "csharp", true
	case ".ts":
		return "typescript", true
	case ".tsx":
		return "tsx", true
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript", true
	}
	return "", false
}
```

Create empty placeholder query files so the package compiles (filled in later tasks):
```bash
printf '\n' > internal/parse/queries/csharp.scm
printf '\n' > internal/parse/queries/typescript.scm
printf '\n' > internal/parse/queries/javascript.scm
```
(An empty query compiles to zero patterns — valid.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run 'TestLangForExt|TestNewRegistry' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/parse/
git commit -m "feat(parse): language registry, extension mapping, embedded queries"
```

---

## Task 5: parse — ParseFile + Go extraction (golden)

**Files:**
- Create: `internal/parse/parse.go`
- Test: `internal/parse/parse_test.go`

- [ ] **Step 1: Write the failing golden test**

`internal/parse/parse_test.go`:
```go
package parse

import "testing"

func names(syms []Symbol, kind string) []string {
	var out []string
	for _, s := range syms {
		if s.Kind == kind {
			out = append(out, s.Name)
		}
	}
	return out
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestParseGo(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`package p

import "fmt"

type Greeter struct{}

func (g Greeter) Hello(name string) string {
	return fmt.Sprintf("hi %s", name)
}

func Run() {
	g := Greeter{}
	g.Hello("world")
}
`)
	res, err := r.ParseFile("go", "p/greet.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if !has(names(res.Symbols, "function"), "Run") {
		t.Errorf("missing function Run; functions=%v", names(res.Symbols, "function"))
	}
	if !has(names(res.Symbols, "method"), "Hello") {
		t.Errorf("missing method Hello; methods=%v", names(res.Symbols, "method"))
	}
	if !has(names(res.Symbols, "type"), "Greeter") {
		t.Errorf("missing type Greeter; types=%v", names(res.Symbols, "type"))
	}
	// Hello's body calls fmt.Sprintf; Run calls Hello.
	var calls, imports []string
	for _, e := range res.Edges {
		switch e.Kind {
		case "calls":
			calls = append(calls, e.DstName)
		case "imports":
			imports = append(imports, e.DstName)
		}
	}
	if !has(calls, "Sprintf") || !has(calls, "Hello") {
		t.Errorf("calls = %v, want Sprintf and Hello", calls)
	}
	if len(imports) == 0 {
		t.Errorf("expected an import edge, got none")
	}
	// A Symbol must carry a non-empty byte range (powers get_code_snippet).
	for _, s := range res.Symbols {
		if s.EndByte <= s.StartByte {
			t.Errorf("symbol %q has empty byte range", s.Name)
		}
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseGo -v`
Expected: FAIL (undefined `Symbol`/`Edge`/`Result`/`ParseFile`).

- [ ] **Step 3: Implement `parse.go`**

`internal/parse/parse.go`:
```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseGo -v`
Expected: PASS. If a capture is empty, print the tree with `tree.RootNode().ToSexp()` to inspect the pinned grammar's actual node names and adjust `queries/go.scm`.

- [ ] **Step 5: Commit**

```bash
git add internal/parse/parse.go internal/parse/parse_test.go
git commit -m "feat(parse): ParseFile + Go symbol/edge extraction"
```

---

## Task 6: parse — C# extraction (golden)

**Files:**
- Modify: `internal/parse/queries/csharp.scm`
- Test: `internal/parse/parse_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/parse/parse_test.go`:
```go
func TestParseCSharp(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`namespace App {
  class Service {
    public void Run() { Helper.Do(); }
  }
}`)
	res, err := r.ParseFile("csharp", "Service.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	if !has(names(res.Symbols, "class"), "Service") {
		t.Errorf("missing class Service; classes=%v", names(res.Symbols, "class"))
	}
	if !has(names(res.Symbols, "method"), "Run") {
		t.Errorf("missing method Run; methods=%v", names(res.Symbols, "method"))
	}
	var calls []string
	for _, e := range res.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	if !has(calls, "Do") {
		t.Errorf("calls = %v, want Do", calls)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseCSharp -v`
Expected: FAIL (no symbols — query file is empty).

- [ ] **Step 3: Fill `queries/csharp.scm`**

```scheme
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.struct
(method_declaration name: (identifier) @name) @definition.method
(namespace_declaration name: (_) @name) @definition.namespace
(invocation_expression function: (member_access_expression name: (identifier) @name)) @reference.call
(invocation_expression function: (identifier) @name) @reference.call
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseCSharp -v`
Expected: PASS. (If the pinned grammar names the namespace node `file_scoped_namespace_declaration` for `namespace X;` form, add that variant; the block form above is sufficient for the test.)

- [ ] **Step 5: Commit**

```bash
git add internal/parse/queries/csharp.scm internal/parse/parse_test.go
git commit -m "feat(parse): C# extraction"
```

---

## Task 7: parse — TypeScript + TSX extraction (golden)

**Files:**
- Modify: `internal/parse/queries/typescript.scm`
- Test: `internal/parse/parse_test.go`

- [ ] **Step 1: Write the failing test (covers both `typescript` and `tsx`)**

```go
func TestParseTypeScript(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, lang := range []string{"typescript", "tsx"} {
		src := []byte(`import { x } from "./x";
class Widget { render() { draw(); } }
function mount() { const w = new Widget(); w.render(); }`)
		res, err := r.ParseFile(lang, "w."+lang, src)
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if !has(names(res.Symbols, "class"), "Widget") {
			t.Errorf("%s: missing class Widget; classes=%v", lang, names(res.Symbols, "class"))
		}
		if !has(names(res.Symbols, "function"), "mount") {
			t.Errorf("%s: missing function mount", lang)
		}
		if !has(names(res.Symbols, "method"), "render") {
			t.Errorf("%s: missing method render", lang)
		}
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseTypeScript -v`
Expected: FAIL.

- [ ] **Step 3: Fill `queries/typescript.scm`**

```scheme
(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(import_statement source: (string) @name) @reference.import
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseTypeScript -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/parse/queries/typescript.scm internal/parse/parse_test.go
git commit -m "feat(parse): TypeScript + TSX extraction"
```

---

## Task 8: parse — JavaScript + JSX extraction (golden)

**Files:**
- Modify: `internal/parse/queries/javascript.scm`
- Test: `internal/parse/parse_test.go`

- [ ] **Step 1: Write the failing test (JSX-bearing source = React)**

```go
func TestParseJavaScript(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`import React from "react";
class Box { draw() {} }
function App() { render(); return <div/>; }`)
	res, err := r.ParseFile("javascript", "app.jsx", src)
	if err != nil {
		t.Fatal(err)
	}
	if !has(names(res.Symbols, "function"), "App") {
		t.Errorf("missing function App; functions=%v", names(res.Symbols, "function"))
	}
	if !has(names(res.Symbols, "class"), "Box") {
		t.Errorf("missing class Box")
	}
	var calls []string
	for _, e := range res.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	if !has(calls, "render") {
		t.Errorf("calls = %v, want render", calls)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -run TestParseJavaScript -v`
Expected: FAIL.

- [ ] **Step 3: Fill `queries/javascript.scm`**

```scheme
(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (identifier) @name) @definition.class
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(import_statement source: (string) @name) @reference.import
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/parse/ -v`
Expected: PASS (all four language tests).

- [ ] **Step 5: Commit**

```bash
git add internal/parse/queries/javascript.scm internal/parse/parse_test.go
git commit -m "feat(parse): JavaScript + JSX extraction"
```

---

## Task 9: parse — must-Close discipline test

**Files:**
- Test: `internal/parse/close_test.go`

- [ ] **Step 1: Write the test (stress ParseFile; a leak would surface under -race / over many iterations)**

`internal/parse/close_test.go`:
```go
package parse

import "testing"

// TestParseFileRepeatedDoesNotPanicOrLeak exercises the Parser/Tree/QueryCursor
// open+Close cycle many times. With the defer Close() discipline this is stable;
// a missing Close would leak C memory (observable as growth, and unsafe under
// the race detector across goroutines).
func TestParseFileRepeatedDoesNotPanicOrLeak(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("package p\nfunc F() { G() }\nfunc G() {}")
	for i := 0; i < 2000; i++ {
		if _, err := r.ParseFile("go", "p.go", src); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseFileConcurrent(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("package p\nfunc F() {}")
	done := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				if _, err := r.ParseFile("go", "p.go", src); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run it (with race) to verify it passes**

Run: `CGO_ENABLED=1 go test -race ./internal/parse/ -run TestParseFile -v`
Expected: PASS, no race output. (Each `ParseFile` creates and closes its own Parser/Tree/QueryCursor; the shared `*Query` is read-only and concurrency-safe.)

- [ ] **Step 3: Commit**

```bash
git add internal/parse/close_test.go
git commit -m "test(parse): must-Close and concurrent ParseFile safety"
```

---

## Task 10: graph — Engine, constructor, shared helpers

**Files:**
- Create: `internal/graph/graph.go`
- Test: `internal/graph/graph_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/graph_test.go`:
```go
package graph

import (
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// newTestEngine builds a graph engine over a fresh temp store.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e, err := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestNewEngine(t *testing.T) {
	e := newTestEngine(t)
	if e.reg == nil {
		t.Fatal("registry not built")
	}
}

func TestEnclosing(t *testing.T) {
	spans := []span{{"a", 0, 100}, {"b", 10, 40}, {"c", 50, 60}}
	if got := enclosing(spans, 20); got != "b" {
		t.Errorf("enclosing(20) = %q, want b (innermost)", got)
	}
	if got := enclosing(spans, 70); got != "a" {
		t.Errorf("enclosing(70) = %q, want a", got)
	}
	if got := enclosing(spans, 200); got != "" {
		t.Errorf("enclosing(200) = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run 'TestNewEngine|TestEnclosing' -v`
Expected: FAIL (undefined `Engine`/`New`/`span`/`enclosing`).

- [ ] **Step 3: Implement `graph.go`**

`internal/graph/graph.go`:
```go
// Package graph builds and queries Recarto's worktree-aware code graph over the
// shared per-repo store. It mirrors internal/memory.Engine: write side (Index/
// Reconcile) and read side (Search/TraceCallPath/Architecture/Query/Snippet). It
// is git-agnostic — callers pass the worktree and a file Source.
package graph

import (
	"crypto/sha256"
	"encoding/hex"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/parse"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Engine indexes files into, and answers structural queries over, the store.
type Engine struct {
	st  *store.Store
	led *ledger.Ledger
	clk clock.Clock
	reg *parse.Registry
}

// New builds an Engine, compiling the parse registry (which links tree-sitter).
func New(st *store.Store, led *ledger.Ledger, clk clock.Clock) (*Engine, error) {
	reg, err := parse.NewRegistry()
	if err != nil {
		return nil, err
	}
	return &Engine{st: st, led: led, clk: clk, reg: reg}, nil
}

// Close releases the parse registry's compiled queries.
func (e *Engine) Close() { e.reg.Close() }

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// span is a node id with its byte range, used to attribute an edge to the
// innermost enclosing definition.
type span struct {
	id    string
	start uint
	end   uint
}

// enclosing returns the id of the innermost span containing byte b, or "".
func enclosing(spans []span, b uint) string {
	best := ""
	var bestLen uint
	for _, s := range spans {
		if b >= s.start && b < s.end {
			if l := s.end - s.start; best == "" || l < bestLen {
				best, bestLen = s.id, l
			}
		}
	}
	return best
}

// nullStr maps "" → SQL NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run 'TestNewEngine|TestEnclosing' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/graph.go internal/graph/graph_test.go
git commit -m "feat(graph): engine, constructor, enclosing/hash helpers"
```

---

## Task 11: graph — IndexFile (atomic per-file write)

**Files:**
- Create: `internal/graph/index.go`
- Test: `internal/graph/index_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/index_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func countRows(t *testing.T, e *Engine, table, worktree string) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table
	var err error
	switch table {
	case "files":
		err = e.st.DB.QueryRow(q+" WHERE worktree=?", worktree).Scan(&n)
	default:
		err = e.st.DB.QueryRow(q+
			" WHERE file_id IN (SELECT id FROM files WHERE worktree=?)", worktree).Scan(&n)
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIndexFileWritesNodesAndEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc A() { B() }\nfunc B() {}")
	if err := e.IndexFile(ctx, "/wt", "p/a.go", src, 111, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 2 { // A, B
		t.Errorf("nodes = %d, want 2", got)
	}
	if got := countRows(t, e, "edges", "/wt"); got < 1 { // A calls B
		t.Errorf("edges = %d, want >=1", got)
	}
	// The call edge must be attributed to its enclosing function A.
	var srcKind string
	err := e.st.DB.QueryRow(`
		SELECT n.name FROM edges ed JOIN nodes n ON n.id = ed.src_node_id
		WHERE ed.kind='calls' AND ed.dst_name='B'`).Scan(&srcKind)
	if err != nil {
		t.Fatalf("call edge not attributed to enclosing node: %v", err)
	}
	if srcKind != "A" {
		t.Errorf("call to B attributed to %q, want A", srcKind)
	}
}

func TestIndexFileReplacesOnReindex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}\nfunc B(){}"), 1, 30); err != nil {
		t.Fatal(err)
	}
	// Reindex same path with fewer symbols → old rows replaced, not duplicated.
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}"), 2, 20); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1 (upsert, not insert)", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 1 {
		t.Errorf("nodes = %d, want 1 after reindex", got)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestIndexFile -v`
Expected: FAIL (undefined `IndexFile`).

- [ ] **Step 3: Implement `index.go`**

`internal/graph/index.go`:
```go
package graph

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/parse"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// IndexFile parses src and atomically (re)writes the file's nodes/edges and its
// files row. Unsupported extensions are skipped (no error). It runs in one short
// WriteTx so a file's graph is replaced atomically and the lock is held briefly.
func (e *Engine) IndexFile(ctx context.Context, worktree, path string, src []byte, mtime, size int64) error {
	lang, ok := parse.LangForExt(path)
	if !ok {
		return nil
	}
	res, err := e.reg.ParseFile(lang, path, src)
	if err != nil {
		return err
	}
	hash := sha256hex(src)
	now := e.clk.Now().UnixMilli()
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.writeFileGraph(ctx, c, worktree, path, lang, res, hash, mtime, size, now)
	})
}

func (e *Engine) writeFileGraph(ctx context.Context, c store.Conn, worktree, path, lang string,
	res parse.Result, hash string, mtime, size, now int64) error {
	// Upsert the files row and obtain its id.
	var fileID string
	err := c.QueryRowContext(ctx,
		`SELECT id FROM files WHERE worktree=? AND path=?`, worktree, path).Scan(&fileID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fileID = ids.New()
		if _, err := c.ExecContext(ctx,
			`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			fileID, worktree, path, lang, hash, mtime, size, now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// Drop the file's FTS rows before deleting nodes (FTS5 is not linked).
		if _, err := c.ExecContext(ctx,
			`DELETE FROM nodes_fts WHERE node_id IN (SELECT id FROM nodes WHERE file_id=?)`,
			fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM nodes WHERE file_id=?`, fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM edges WHERE file_id=?`, fileID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE files SET language=?,content_hash=?,mtime=?,size=?,last_indexed_at=? WHERE id=?`,
			lang, hash, mtime, size, now, fileID); err != nil {
			return err
		}
	}

	// Insert nodes; remember each node's byte span for edge attribution.
	spans := make([]span, 0, len(res.Symbols))
	for _, s := range res.Symbols {
		nid := ids.New()
		qn := s.QualifiedName
		if qn == "" {
			qn = s.Name
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,
			   start_byte,end_byte,start_line,end_line)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			nid, fileID, s.Kind, s.Name, qn, "",
			int64(s.StartByte), int64(s.EndByte), int64(s.StartLine), int64(s.EndLine)); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO nodes_fts(node_id,name,qualified_name) VALUES (?,?,?)`,
			nid, s.Name, qn); err != nil {
			return err
		}
		spans = append(spans, span{id: nid, start: s.StartByte, end: s.EndByte})
	}

	// Insert edges, attributing each to the innermost enclosing node.
	for _, ed := range res.Edges {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
			 VALUES (?,?,?,?,?,?,?)`,
			ids.New(), nullStr(enclosing(spans, ed.StartByte)), nil, ed.DstName, ed.Kind,
			fileID, int64(ed.Line)); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestIndexFile -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/index.go internal/graph/index_test.go
git commit -m "feat(graph): atomic per-file IndexFile with edge attribution"
```

---

## Task 12: graph — Source + Reconcile

**Files:**
- Create: `internal/graph/source.go`, `internal/graph/reconcile.go`
- Test: `internal/graph/reconcile_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/reconcile_test.go`:
```go
package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileIndexesChangedAndRemovesDeleted(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){}")
	writeFile(t, wt, "sub/b.go", "package q\nfunc B(){}")
	writeFile(t, wt, "notes.md", "ignore me")

	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", wt); got != 2 { // a.go, sub/b.go; not notes.md
		t.Fatalf("files = %d, want 2", got)
	}

	// Delete b.go, change a.go; reconcile must drop b and reindex a only.
	if err := os.Remove(filepath.Join(wt, "sub/b.go")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wt, "a.go", "package p\nfunc A(){}\nfunc C(){}")
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", wt); got != 1 {
		t.Errorf("files = %d, want 1 after delete", got)
	}
	if got := countRows(t, e, "nodes", wt); got != 2 { // A, C in a.go
		t.Errorf("nodes = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestReconcile -v`
Expected: FAIL (undefined `FSSource`/`Reconcile`).

- [ ] **Step 3: Implement `source.go`**

`internal/graph/source.go`:
```go
package graph

import (
	"io/fs"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/parse"
)

// FileEntry is a candidate source file discovered by a Source.
type FileEntry struct {
	Path  string // repo-relative, slash-separated
	Mtime int64  // unix ms
	Size  int64
}

// Source enumerates and reads a worktree's supported source files. It is an
// interface so the engine stays testable (and git-agnostic).
type Source interface {
	List() ([]FileEntry, error)
	Read(path string) ([]byte, error)
}

// skipDir is the directory denylist (build output, deps, VCS, IDE metadata).
var skipDir = map[string]bool{
	".git": true, "node_modules": true, "bin": true, "obj": true,
	"dist": true, "build": true, "vendor": true, ".vs": true, ".idea": true,
}

// FSSource walks the filesystem under Root, selecting files by supported
// extension and skipping the denylist directories. (.gitignore-honoring is a
// later refinement; the denylist covers the common noise.)
type FSSource struct{ Root string }

func (s FSSource) List() ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(s.Root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := parse.LangForExt(rel); !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Mtime: info.ModTime().UnixMilli(), Size: info.Size()})
		return nil
	})
	return out, err
}

func (s FSSource) Read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(path)))
}
```

- [ ] **Step 4: Implement `reconcile.go`**

`internal/graph/reconcile.go`:
```go
package graph

import (
	"context"
)

// Reconcile makes the worktree's graph match src: it (re)indexes new and changed
// files (mtime/size pre-check, then content-hash confirm), deletes removed files,
// and resolves name references. Unchanged files are skipped cheaply.
func (e *Engine) Reconcile(ctx context.Context, worktree string, src Source) error {
	entries, err := src.List()
	if err != nil {
		return err
	}

	// Load current index state for this worktree.
	type rec struct {
		hash         string
		mtime, size  int64
	}
	have := map[string]rec{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, content_hash, mtime, size FROM files WHERE worktree=?`, worktree)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		var r rec
		if err := rows.Scan(&p, &r.hash, &r.mtime, &r.size); err != nil {
			rows.Close()
			return err
		}
		have[p] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	seen := map[string]bool{}
	for _, fe := range entries {
		seen[fe.Path] = true
		prev, known := have[fe.Path]
		if known && prev.mtime == fe.Mtime && prev.size == fe.Size {
			continue // cheap pre-check: unchanged
		}
		content, err := src.Read(fe.Path)
		if err != nil {
			return err
		}
		if known && prev.hash == sha256hex(content) {
			continue // mtime moved but content identical
		}
		if err := e.IndexFile(ctx, worktree, fe.Path, content, fe.Mtime, fe.Size); err != nil {
			return err
		}
	}

	// Delete files that disappeared (CASCADE removes their nodes/edges; FTS rows
	// are dropped explicitly since FTS5 is not linked).
	for p := range have {
		if seen[p] {
			continue
		}
		if err := e.deleteFile(ctx, worktree, p); err != nil {
			return err
		}
	}
	return e.Resolve(ctx, worktree)
}
```

`reconcile.go` calls `e.Resolve` and `e.deleteFile`, implemented next in the same task so the package compiles.

- [ ] **Step 4: Create `resolve.go` (Resolve + deleteFile)**

`internal/graph/resolve.go`:
```go
package graph

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Resolve links unresolved reference edges to a defining node within the same
// worktree by matching dst_name against node name or qualified_name. Ambiguous
// names take the first match (LIMIT 1); dst_name is retained regardless, so
// trace_call_path still works by name where resolution is partial.
func (e *Engine) Resolve(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ?
    AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
  LIMIT 1)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree, worktree)
		return err
	})
}

// deleteFile removes a file's graph rows for the given worktree. nodes/edges
// cascade via FK; nodes_fts rows are dropped manually (FTS5 is unlinked).
func (e *Engine) deleteFile(ctx context.Context, worktree, path string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts WHERE node_id IN (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx,
			`DELETE FROM files WHERE worktree = ? AND path = ?`, worktree, path)
		return err
	})
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestReconcile -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/graph/source.go internal/graph/reconcile.go internal/graph/resolve.go internal/graph/reconcile_test.go
git commit -m "feat(graph): FSSource + Reconcile + name resolution"
```

---

## Task 13: graph — name-resolution regression test

**Files:**
- Test: `internal/graph/resolve_test.go`

> `Resolve` and `deleteFile` were implemented in Task 12 (reconcile.go depends on them). This task adds a focused test proving cross-file call resolution links `dst_name` to a defining node.

- [ ] **Step 1: Write the test**

`internal/graph/resolve_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func TestResolveLinksCallEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Two files in one worktree: a.go's A() calls B() defined in b.go.
	if err := e.IndexFile(ctx, "/wt", "a.go", []byte("package p\nfunc A(){ B() }"), 1, 24); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "b.go", []byte("package p\nfunc B(){}"), 1, 19); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var resolved int
	e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges WHERE kind='calls' AND dst_name='B' AND dst_node_id IS NOT NULL`).
		Scan(&resolved)
	if resolved != 1 {
		t.Errorf("resolved call edges to B = %d, want 1", resolved)
	}
}
```

- [ ] **Step 2: Run it to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run 'TestResolve|TestReconcile' -v`
Expected: PASS (`Resolve` already exists from Task 12).

- [ ] **Step 3: Commit**

```bash
git add internal/graph/resolve_test.go
git commit -m "test(graph): cross-file call name resolution"
```

---

## Task 14: graph — concurrency proof (multi-goroutine reconcile)

**Files:**
- Test: `internal/graph/concurrency_test.go`

> Cross-process safety of the store itself is already proven by `internal/store`'s `TestConcurrentWritersAcrossProcessesAndGoroutines`; graph writes use the same `WriteTx`/`BEGIN IMMEDIATE` path. This test proves the graph write path specifically is integrity-safe and worktree-isolated under concurrency.

- [ ] **Step 1: Write the test**

`internal/graph/concurrency_test.go`:
```go
package graph

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// memSource is an in-memory Source for deterministic concurrency tests.
type memSource struct{ files map[string]string }

func (m memSource) List() ([]FileEntry, error) {
	var out []FileEntry
	for p, c := range m.files {
		out = append(out, FileEntry{Path: p, Mtime: 1, Size: int64(len(c))})
	}
	return out, nil
}
func (m memSource) Read(path string) ([]byte, error) { return []byte(m.files[path]), nil }

func TestConcurrentReconcileAcrossWorktrees(t *testing.T) {
	e := newTestEngine(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt := fmt.Sprintf("/wt%d", i)
			src := memSource{files: map[string]string{
				"a.go": fmt.Sprintf("package p\nfunc A%d(){ B%d() }", i, i),
				"b.go": fmt.Sprintf("package p\nfunc B%d(){}", i),
			}}
			for r := 0; r < 5; r++ {
				if err := e.Reconcile(context.Background(), wt, src); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Integrity + per-worktree isolation: each worktree has exactly its 2 nodes.
	var integrity string
	e.st.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}
	for i := 0; i < n; i++ {
		if got := countRows(t, e, "nodes", fmt.Sprintf("/wt%d", i)); got != 2 {
			t.Errorf("worktree %d nodes = %d, want 2", i, got)
		}
	}
}
```

- [ ] **Step 2: Run it (with race) to verify it passes**

Run: `CGO_ENABLED=1 go test -race ./internal/graph/ -run TestConcurrentReconcile -v`
Expected: PASS, `integrity_check = ok`, no race output.

- [ ] **Step 3: Commit**

```bash
git add internal/graph/concurrency_test.go
git commit -m "test(graph): concurrent reconcile integrity + worktree isolation"
```

---

## Task 15: graph — Search (search_graph)

**Files:**
- Create: `internal/graph/search.go`
- Test: `internal/graph/search_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/search_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func TestSearchByName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Authenticate(){}\nfunc helper(){}\ntype Session struct{}")
	if err := e.IndexFile(ctx, "/wt", "auth.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "Authenticate", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Authenticate" {
		t.Fatalf("Search = %+v, want one result Authenticate", got)
	}
	if got[0].Path != "auth.go" || got[0].Kind != "function" {
		t.Errorf("result = %+v, want path auth.go kind function", got[0])
	}
	// kind filter
	only, err := e.Search(ctx, "/wt", "Session", "type", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Kind != "type" {
		t.Errorf("kind-filtered search = %+v", only)
	}
	// worktree isolation: nothing for a different worktree
	none, _ := e.Search(ctx, "/other", "Authenticate", "", "", 10)
	if len(none) != 0 {
		t.Errorf("cross-worktree search leaked %d results", len(none))
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestSearchByName -v`
Expected: FAIL (undefined `Search`).

- [ ] **Step 3: Implement `search.go`**

`internal/graph/search.go`:
```go
package graph

import (
	"context"
	"strings"
)

// SearchResult is one symbol match.
type SearchResult struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Language      string
	Path          string
	Line          int
	Score         float64 // higher = more relevant
}

// ftsQuery turns a free-text query into a safe FTS5 MATCH expression: each
// whitespace-separated term is double-quoted (so identifiers with punctuation or
// FTS operators can't break the query) and ANDed together.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return `""`
	}
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

// Search returns symbols matching query (FTS over name/qualified_name), scoped to
// worktree, optionally filtered by kind and language, ranked by bm25.
func (e *Engine) Search(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	args := []any{ftsQuery(query), worktree}
	sql := `
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line,
       bm25(nodes_fts) AS score
  FROM nodes_fts
  JOIN nodes n ON n.id = nodes_fts.node_id
  JOIN files f ON f.id = n.file_id
 WHERE nodes_fts MATCH ? AND f.worktree = ?`
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	sql += " ORDER BY score ASC LIMIT ?" // bm25 is negative: more negative = better
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var bm float64
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line, &bm); err != nil {
			return nil, err
		}
		r.Score = -bm // flip sign so higher = better
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestSearchByName -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/search.go internal/graph/search_test.go
git commit -m "feat(graph): search_graph (FTS bm25, worktree-scoped)"
```

---

## Task 16: graph — Snippet (get_code_snippet) + token savings

**Files:**
- Create: `internal/graph/snippet.go`
- Test: `internal/graph/snippet_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/snippet_test.go`:
```go
package graph

import (
	"context"
	"strings"
	"testing"
)

func TestSnippetReturnsSymbolSource(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	content := "package p\n\nfunc Target() int {\n\treturn 42\n}\n\nfunc Other() {}\n"
	writeFile(t, wt, "t.go", content)
	src, _ := FSSource{Root: wt}.Read("t.go")
	if err := e.IndexFile(ctx, wt, "t.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	snip, err := e.Snippet(ctx, wt, "t.go", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snip, "func Target() int") || !strings.Contains(snip, "return 42") {
		t.Errorf("snippet = %q, want the Target function body", snip)
	}
	if strings.Contains(snip, "func Other") {
		t.Errorf("snippet leaked Other; should be just Target")
	}
	// token savings recorded (whole file is larger than the snippet)
	saved, _ := e.led.Get(ctx, "lifetime", "graph_tokens_saved")
	if saved <= 0 {
		t.Errorf("graph_tokens_saved = %d, want > 0", saved)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestSnippet -v`
Expected: FAIL (undefined `Snippet`).

- [ ] **Step 3: Implement `snippet.go`**

`internal/graph/snippet.go`:
```go
package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

// Snippet returns the exact source for a symbol (by name or qualified_name) in a
// file, using its stored byte range. It records the tokens saved vs. reading the
// whole file into the ledger.
func (e *Engine) Snippet(ctx context.Context, worktree, path, symbol string) (string, error) {
	var sb, eb int64
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND (n.name = ? OR n.qualified_name = ?)
 LIMIT 1`, worktree, path, symbol, symbol).Scan(&sb, &eb)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("graph: symbol %q not found in %s", symbol, path)
	}
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	if sb < 0 || eb > int64(len(content)) || sb > eb {
		return "", fmt.Errorf("graph: stale byte range for %q (reindex needed)", symbol)
	}
	snippet := string(content[sb:eb])

	saved := ledger.EstimateTokens(string(content)) - ledger.EstimateTokens(snippet)
	if saved > 0 {
		_ = e.led.Incr(ctx, "lifetime", "graph_tokens_saved", int64(saved))
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return snippet, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestSnippet -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/snippet.go internal/graph/snippet_test.go
git commit -m "feat(graph): get_code_snippet with token-savings accounting"
```

---

## Task 17: graph — TraceCallPath (trace_call_path)

**Files:**
- Create: `internal/graph/trace.go`
- Test: `internal/graph/trace_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/trace_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func TestTraceCallPathCallees(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A -> B -> C
	src := []byte("package p\nfunc A(){ B() }\nfunc B(){ C() }\nfunc C(){}")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "A", "callees", 3)
	if err != nil {
		t.Fatal(err)
	}
	// Expect at least one chain A -> B -> C
	found := false
	for _, ch := range chains {
		var seq string
		for _, n := range ch {
			seq += n.Name + ">"
		}
		if seq == "A>B>C>" {
			found = true
		}
	}
	if !found {
		t.Errorf("chains = %v, want one A>B>C", chains)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestTraceCallPath -v`
Expected: FAIL (undefined `TraceCallPath`).

- [ ] **Step 3: Implement `trace.go`**

`internal/graph/trace.go`:
```go
package graph

import (
	"context"
	"database/sql"
	"errors"
)

// CallNode is one hop in a call chain.
type CallNode struct {
	NodeID string
	Name   string
	Path   string
	Line   int
}

// TraceCallPath returns call chains starting at symbol. direction "callees"
// follows outgoing calls (what this calls); "callers" follows incoming calls
// (who calls this). Chains are bounded by depth and a per-node fan-out cap to
// avoid explosion. Edges resolved by dst_node_id are followed directly;
// unresolved edges fall back to matching dst_name to a node name.
func (e *Engine) TraceCallPath(ctx context.Context, worktree, symbol, direction string, depth int) ([][]CallNode, error) {
	if depth <= 0 {
		depth = 3
	}
	start, err := e.nodeByName(ctx, worktree, symbol)
	if err != nil {
		return nil, err
	}
	if start.NodeID == "" {
		return nil, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	var chains [][]CallNode
	var walk func(cur CallNode, path []CallNode, visited map[string]bool, d int)
	walk = func(cur CallNode, path []CallNode, visited map[string]bool, d int) {
		next, err := e.callNeighbors(ctx, worktree, cur, direction)
		if err != nil || len(next) == 0 || d == 0 {
			chains = append(chains, path)
			return
		}
		for _, nb := range next {
			if visited[nb.NodeID] {
				continue
			}
			v2 := make(map[string]bool, len(visited)+1)
			for k := range visited {
				v2[k] = true
			}
			v2[nb.NodeID] = true
			walk(nb, append(append([]CallNode{}, path...), nb), v2, d-1)
		}
	}
	walk(start, []CallNode{start}, map[string]bool{start.NodeID: true}, depth)
	return chains, nil
}

func (e *Engine) nodeByName(ctx context.Context, worktree, name string) (CallNode, error) {
	var n CallNode
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.id, n.name, f.path, n.start_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ?) LIMIT 1`,
		worktree, name, name).Scan(&n.NodeID, &n.Name, &n.Path, &n.Line)
	if errors.Is(err, sql.ErrNoRows) {
		return CallNode{}, nil
	}
	return n, err
}

// callNeighbors returns the next hop nodes for cur in the given direction, capped.
func (e *Engine) callNeighbors(ctx context.Context, worktree string, cur CallNode, direction string) ([]CallNode, error) {
	const fanout = 25
	var q string
	if direction == "callers" {
		// nodes whose edges point at cur (by resolved id or by name)
		q = `
SELECT DISTINCT n.id, n.name, f.path, n.start_line
  FROM edges ed
  JOIN nodes n ON n.id = ed.src_node_id
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND f.worktree = ?
   AND (ed.dst_node_id = ? OR ed.dst_name = ?)
 LIMIT ?`
	} else {
		// nodes that cur calls (resolved id, or unresolved name matched to a node)
		q = `
SELECT DISTINCT n.id, n.name, f.path, n.start_line
  FROM edges ed
  JOIN nodes n ON (n.id = ed.dst_node_id OR (ed.dst_node_id IS NULL AND n.name = ed.dst_name))
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND ed.src_node_id = ? AND f.worktree = ?
 LIMIT ?`
	}
	var rows *sql.Rows
	var err error
	if direction == "callers" {
		rows, err = e.st.DB.QueryContext(ctx, q, worktree, cur.NodeID, cur.Name, fanout)
	} else {
		rows, err = e.st.DB.QueryContext(ctx, q, cur.NodeID, worktree, fanout)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallNode
	for rows.Next() {
		var n CallNode
		if err := rows.Scan(&n.NodeID, &n.Name, &n.Path, &n.Line); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestTraceCallPath -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/trace.go internal/graph/trace_test.go
git commit -m "feat(graph): trace_call_path (bounded BFS, resolved + name fallback)"
```

---

## Task 18: graph — Architecture (get_architecture)

**Files:**
- Create: `internal/graph/architecture.go`
- Test: `internal/graph/architecture_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/architecture_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func TestArchitectureGroupsByModule(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	mustIndex := func(path, src string) {
		if err := e.IndexFile(ctx, "/wt", path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	mustIndex("auth/login.go", "package auth\nfunc Login(){}\nfunc Logout(){}")
	mustIndex("auth/token.go", "package auth\nfunc Mint(){}")
	mustIndex("api/handler.go", "package api\nfunc Handle(){}")

	arch, err := e.Architecture(ctx, "/wt", "")
	if err != nil {
		t.Fatal(err)
	}
	mods := map[string]Module{}
	for _, m := range arch.Modules {
		mods[m.Name] = m
	}
	if mods["auth"].Files != 2 || mods["auth"].Symbols != 3 {
		t.Errorf("auth module = %+v, want 2 files / 3 symbols", mods["auth"])
	}
	if mods["api"].Files != 1 || mods["api"].Symbols != 1 {
		t.Errorf("api module = %+v, want 1 file / 1 symbol", mods["api"])
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestArchitecture -v`
Expected: FAIL (undefined `Architecture`/`Module`).

- [ ] **Step 3: Implement `architecture.go`**

`internal/graph/architecture.go`:
```go
package graph

import (
	"context"
	"sort"
	"strings"
)

// Module is a top-level grouping (first path segment) with rollup counts.
type Module struct {
	Name    string
	Files   int
	Symbols int
}

// Architecture is a bird's-eye structural map of a worktree.
type Architecture struct {
	Modules []Module
}

// moduleOf returns the first path segment ("" for a top-level file → ".").
func moduleOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// Architecture summarizes the worktree's modules: file and symbol counts per
// top-level directory. scope, if non-empty, restricts to paths under that prefix.
func (e *Engine) Architecture(ctx context.Context, worktree, scope string) (Architecture, error) {
	q := `SELECT f.path, (SELECT COUNT(*) FROM nodes n WHERE n.file_id = f.id)
	        FROM files f WHERE f.worktree = ?`
	args := []any{worktree}
	if scope != "" {
		q += " AND f.path LIKE ?"
		args = append(args, scope+"%")
	}
	rows, err := e.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return Architecture{}, err
	}
	defer rows.Close()
	type agg struct{ files, syms int }
	byMod := map[string]*agg{}
	for rows.Next() {
		var path string
		var syms int
		if err := rows.Scan(&path, &syms); err != nil {
			return Architecture{}, err
		}
		m := moduleOf(path)
		if byMod[m] == nil {
			byMod[m] = &agg{}
		}
		byMod[m].files++
		byMod[m].syms += syms
	}
	if err := rows.Err(); err != nil {
		return Architecture{}, err
	}
	var arch Architecture
	for name, a := range byMod {
		arch.Modules = append(arch.Modules, Module{Name: name, Files: a.files, Symbols: a.syms})
	}
	sort.Slice(arch.Modules, func(i, j int) bool { return arch.Modules[i].Name < arch.Modules[j].Name })
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return arch, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestArchitecture -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/architecture.go internal/graph/architecture_test.go
git commit -m "feat(graph): get_architecture (module rollups)"
```

---

## Task 19: graph — Query (query_graph, structured)

**Files:**
- Create: `internal/graph/query.go`
- Test: `internal/graph/query_test.go`

- [ ] **Step 1: Write the failing test**

`internal/graph/query_test.go`:
```go
package graph

import (
	"context"
	"testing"
)

func TestQueryNodeFiltersAndEdge(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Caller(){ Callee() }\nfunc Callee(){}\ntype T struct{}")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}

	// kind filter
	types, err := e.Query(ctx, "/wt", QuerySpec{Kind: "type"})
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0].Name != "T" {
		t.Errorf("kind=type query = %+v, want [T]", types)
	}

	// name LIKE + outgoing-call edge filter → only Caller (it has an outgoing call)
	callers, err := e.Query(ctx, "/wt", QuerySpec{
		NameLike: "Call",
		Edge:     &EdgeFilter{Kind: "calls", Direction: "out"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].Name != "Caller" {
		t.Errorf("edge=out calls query = %+v, want [Caller]", callers)
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestQuery -v`
Expected: FAIL (undefined `Query`/`QuerySpec`/`EdgeFilter`).

- [ ] **Step 3: Implement `query.go`**

`internal/graph/query.go`:
```go
package graph

import (
	"context"
	"strings"
)

// EdgeFilter restricts the result to nodes participating in an edge of Kind in
// Direction ("out" = node is the edge source, "in" = node is the edge target).
type EdgeFilter struct {
	Kind      string
	Direction string // "out" | "in"
}

// QuerySpec is the constrained structured query for query_graph (NOT raw SQL).
type QuerySpec struct {
	Kind     string
	NameLike string // substring match on name/qualified_name
	Path     string // exact file path
	Language string
	Edge     *EdgeFilter
	Limit    int
}

// QueryNode is one node in a query_graph result.
type QueryNode struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Path          string
	Line          int
}

// Query runs a constrained structured query over the worktree's graph.
func (e *Engine) Query(ctx context.Context, worktree string, spec QuerySpec) ([]QueryNode, error) {
	limit := spec.Limit
	if limit <= 0 {
		limit = 50
	}
	var b strings.Builder
	b.WriteString(`
SELECT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?`)
	args := []any{worktree}
	if spec.Kind != "" {
		b.WriteString(" AND n.kind = ?")
		args = append(args, spec.Kind)
	}
	if spec.NameLike != "" {
		b.WriteString(" AND (n.name LIKE ? OR n.qualified_name LIKE ?)")
		like := "%" + spec.NameLike + "%"
		args = append(args, like, like)
	}
	if spec.Path != "" {
		b.WriteString(" AND f.path = ?")
		args = append(args, spec.Path)
	}
	if spec.Language != "" {
		b.WriteString(" AND f.language = ?")
		args = append(args, spec.Language)
	}
	if spec.Edge != nil && spec.Edge.Kind != "" {
		if spec.Edge.Direction == "in" {
			b.WriteString(` AND n.id IN (SELECT dst_node_id FROM edges WHERE kind = ? AND dst_node_id IS NOT NULL)`)
		} else {
			b.WriteString(` AND n.id IN (SELECT src_node_id FROM edges WHERE kind = ? AND src_node_id IS NOT NULL)`)
		}
		args = append(args, spec.Edge.Kind)
	}
	b.WriteString(" ORDER BY n.qualified_name LIMIT ?")
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryNode
	for rows.Next() {
		var n QueryNode
		if err := rows.Scan(&n.NodeID, &n.Name, &n.QualifiedName, &n.Kind, &n.Path, &n.Line); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/graph/ -run TestQuery -v`
Expected: PASS.

- [ ] **Step 5: Run the full graph package suite + race**

Run: `CGO_ENABLED=1 go test -race ./internal/graph/ ./internal/parse/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/graph/query.go internal/graph/query_test.go
git commit -m "feat(graph): query_graph (constrained structured query)"
```

---

## Task 20: app — wire the graph engine

**Files:**
- Modify: `internal/app/app.go:30-36` (App struct) and `:72-84` (Open/Close)
- Test: `internal/app/app_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/app/app_test.go`:
```go
func TestOpenBuildsGraphEngine(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{CWD: "/repo", Runner: fakeGit{}, Clock: clock.NewFake(time.UnixMilli(1))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Graph == nil {
		t.Fatal("App.Graph is nil")
	}
}
```
(Match the existing `app_test.go` imports and `fakeGit`; if `fakeGit{}` needs a branch field, mirror the existing helper there.)

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/app/ -run TestOpenBuildsGraphEngine -v`
Expected: FAIL (`a.Graph` undefined).

- [ ] **Step 3: Wire it**

In `internal/app/app.go`, add the import:
```go
	"git.gerwim.nl/gerwim/recarto/internal/graph"
```
Add a field to `App`:
```go
	Graph  *graph.Engine
```
In `Open`, after `eng := memory.New(st, led, clk)`:
```go
	gr, err := graph.New(st, led, clk)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
```
Change the `a := &App{...}` literal to include `Graph: gr`. Update `Close`:
```go
// Close releases the graph engine's parser then the store.
func (a *App) Close() error {
	if a.Graph != nil {
		a.Graph.Close()
	}
	return a.Store.Close()
}
```

- [ ] **Step 4: Run the test (and the package) to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/app/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/app.go internal/app/app_test.go
git commit -m "feat(app): build and close the graph engine alongside memory"
```

---

## Task 21: cli — `recarto index`

**Files:**
- Create: `internal/cli/index.go`
- Modify: `internal/cli/cli.go:15` (`known`), `:60-80` (switch), `:93-104` (usage)
- Test: `internal/cli/index_test.go`

- [ ] **Step 1: Write the failing test**

`internal/cli/index_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexReportsCounts(t *testing.T) {
	a := newTestApp(t, "main") // Git.Worktree == "/repo" per fakeGit
	// Point the index at a real temp dir by overriding the worktree via a file tree.
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Index(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("indexed")) {
		t.Errorf("output = %q, want an 'indexed' summary", out.String())
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/cli/ -run TestIndexReportsCounts -v`
Expected: FAIL (undefined `Index`).

- [ ] **Step 3: Implement `index.go`**

`internal/cli/index.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Index reconciles the code graph for the current worktree and prints a summary.
func Index(ctx context.Context, a *app.App, w io.Writer, _ []string) error {
	wt := a.Git.Worktree
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		return err
	}
	var files, nodes int
	a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE worktree=?`, wt).Scan(&files)
	a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE file_id IN (SELECT id FROM files WHERE worktree=?)`, wt).Scan(&nodes)
	fmt.Fprintf(w, "indexed %d files, %d symbols (worktree %s)\n", files, nodes, wt)
	return nil
}
```

- [ ] **Step 4: Register the command in `cli.go`**

In `known()`, add `"index"`:
```go
	case "status", "doctor", "remember", "recall", "bug", "memory", "serve", "hook", "index", "graph":
		return true
```
In the `Run` switch (before the closing `}`), add:
```go
		case "index":
			return Index(ctx, a, out, rest)
		case "graph":
			return Graph(ctx, a, out, rest)
```
(The `graph` case is implemented in Task 22; adding it now keeps `known()` and the switch consistent. If executing strictly task-by-task, comment the `graph` line until Task 22.)
In `usage`, add:
```go
	fmt.Fprintln(w, "  index             build/refresh the code graph for this worktree")
	fmt.Fprintln(w, "  graph <sub>       query the code graph (search|callers|callees|arch|query|snippet)")
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/cli/ -run TestIndexReportsCounts -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/index.go internal/cli/cli.go internal/cli/index_test.go
git commit -m "feat(cli): recarto index (reconcile + summary)"
```

---

## Task 22: cli — `recarto graph <sub>`

**Files:**
- Create: `internal/cli/graph.go`
- Test: `internal/cli/graph_test.go`

- [ ] **Step 1: Write the failing test**

`internal/cli/graph_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGraphSearch(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"search", "Authenticate"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Authenticate")) {
		t.Errorf("graph search output = %q", out.String())
	}
}

func TestGraphRequiresSubcommand(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Graph(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no subcommand given")
	}
}
```

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/cli/ -run TestGraph -v`
Expected: FAIL (undefined `Graph`).

- [ ] **Step 3: Implement `graph.go`**

`internal/cli/graph.go`:
```go
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Graph dispatches `graph <sub>`: search | callers | callees | arch | query | snippet.
func Graph(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("graph: subcommand required (search|callers|callees|arch|query|snippet)")
	}
	wt := a.Git.Worktree
	sub, rest := args[0], args[1:]
	switch sub {
	case "search":
		res, err := a.Graph.Search(ctx, wt, strings.Join(rest, " "), "", "", 20)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, r := range res {
			fmt.Fprintf(w, "• [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
		}
		return nil
	case "callers", "callees":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		dir := "callees"
		if sub == "callers" {
			dir = "callers"
		}
		chains, err := a.Graph.TraceCallPath(ctx, wt, rest[0], dir, 4)
		if err != nil {
			return err
		}
		for _, ch := range chains {
			parts := make([]string, len(ch))
			for i, n := range ch {
				parts[i] = n.Name
			}
			fmt.Fprintln(w, strings.Join(parts, " → "))
		}
		return nil
	case "arch":
		arch, err := a.Graph.Architecture(ctx, wt, "")
		if err != nil {
			return err
		}
		for _, m := range arch.Modules {
			fmt.Fprintf(w, "%-20s %d files  %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		return nil
	case "query":
		// Minimal CLI surface: `graph query <kind> [nameLike]`.
		spec := graph.QuerySpec{}
		if len(rest) > 0 {
			spec.Kind = rest[0]
		}
		if len(rest) > 1 {
			spec.NameLike = rest[1]
		}
		nodes, err := a.Graph.Query(ctx, wt, spec)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			fmt.Fprintf(w, "• [%s] %s  %s:%d\n", n.Kind, n.QualifiedName, n.Path, n.Line)
		}
		return nil
	case "snippet":
		if len(rest) < 2 {
			return fmt.Errorf("graph snippet: <file> <symbol> required")
		}
		snip, err := a.Graph.Snippet(ctx, wt, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(w, snip)
		return nil
	default:
		return fmt.Errorf("graph: unknown subcommand %q", sub)
	}
}
```

If you commented the `graph` case in `cli.go` Run during Task 21, uncomment it now.

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/cli/ -run TestGraph -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/graph.go internal/cli/cli.go internal/cli/graph_test.go
git commit -m "feat(cli): recarto graph search|callers|callees|arch|query|snippet"
```

---

## Task 23: mcp — five graph tools

**Files:**
- Create: `internal/mcp/graph.go`
- Modify: `internal/mcp/server.go:18-36` (register tools)
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing test (in-process MCP round-trip)**

`internal/mcp/graph_test.go` (mirror the existing `server_test.go` harness for connecting an in-memory client/server; the key assertion is the tool result):
```go
package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchGraphHandler(t *testing.T) {
	a := newTestApp(t) // reuse the existing mcp test helper (see handlers_test.go)
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graphFSSource(wt)); err != nil {
		t.Fatal(err)
	}
	out, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "Authenticate"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Results[0].Name != "Authenticate" {
		t.Fatalf("searchGraph = %+v, want 1 result Authenticate", out)
	}
}
```
Add a tiny test helper near the top of the file:
```go
import "git.gerwim.nl/gerwim/recarto/internal/graph"

func graphFSSource(root string) graph.Source { return graph.FSSource{Root: root} }
```
(If `newTestApp` does not exist in the `mcp` test package, copy the `newTestApp`/`fakeGit` helper from `internal/cli/helpers_test.go` into an `internal/mcp/helpers_test.go`.)

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/mcp/ -run TestSearchGraphHandler -v`
Expected: FAIL (undefined `searchGraph`/`SearchGraphInput`).

- [ ] **Step 3: Implement `graph.go` (SDK-free handlers + In/Out structs)**

`internal/mcp/graph.go`:
```go
package mcp

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// --- search_graph ---

type SearchGraphInput struct {
	Query    string `json:"query" jsonschema:"symbol name or keywords to search"`
	Kind     string `json:"kind,omitempty" jsonschema:"restrict to a node kind (function, method, class, …)"`
	Language string `json:"language,omitempty" jsonschema:"restrict to a language (go, csharp, typescript, tsx, javascript)"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results"`
}
type GraphSymbol struct {
	Name          string  `json:"name"`
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	Line          int     `json:"line"`
	Score         float64 `json:"score,omitempty"`
}
type SearchGraphOutput struct {
	Count   int           `json:"count"`
	Results []GraphSymbol `json:"results"`
}

func searchGraph(ctx context.Context, a *app.App, in SearchGraphInput) (SearchGraphOutput, error) {
	res, err := a.Graph.Search(ctx, a.Git.Worktree, in.Query, in.Kind, in.Language, in.Limit)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	out := SearchGraphOutput{Count: len(res), Results: make([]GraphSymbol, 0, len(res))}
	for _, r := range res {
		out.Results = append(out.Results, GraphSymbol{
			Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
			Path: r.Path, Line: r.Line, Score: r.Score,
		})
	}
	return out, nil
}

// --- trace_call_path ---

type TraceCallPathInput struct {
	Symbol    string `json:"symbol" jsonschema:"the symbol to trace from"`
	Direction string `json:"direction,omitempty" jsonschema:"callers or callees (default callees)"`
	Depth     int    `json:"depth,omitempty" jsonschema:"max chain depth (default 3)"`
}
type TraceCallPathOutput struct {
	Chains [][]string `json:"chains"`
}

func traceCallPath(ctx context.Context, a *app.App, in TraceCallPathInput) (TraceCallPathOutput, error) {
	dir := in.Direction
	if dir == "" {
		dir = "callees"
	}
	chains, err := a.Graph.TraceCallPath(ctx, a.Git.Worktree, in.Symbol, dir, in.Depth)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	out := TraceCallPathOutput{Chains: make([][]string, 0, len(chains))}
	for _, ch := range chains {
		names := make([]string, len(ch))
		for i, n := range ch {
			names[i] = n.Name
		}
		out.Chains = append(out.Chains, names)
	}
	return out, nil
}

// --- get_architecture ---

type GetArchitectureInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"restrict to paths under this prefix"`
}
type ArchModule struct {
	Name    string `json:"name"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
}
type GetArchitectureOutput struct {
	Modules []ArchModule `json:"modules"`
}

func getArchitecture(ctx context.Context, a *app.App, in GetArchitectureInput) (GetArchitectureOutput, error) {
	arch, err := a.Graph.Architecture(ctx, a.Git.Worktree, in.Scope)
	if err != nil {
		return GetArchitectureOutput{}, err
	}
	out := GetArchitectureOutput{Modules: make([]ArchModule, 0, len(arch.Modules))}
	for _, m := range arch.Modules {
		out.Modules = append(out.Modules, ArchModule{Name: m.Name, Files: m.Files, Symbols: m.Symbols})
	}
	return out, nil
}

// --- query_graph ---

type QueryGraphInput struct {
	Kind          string `json:"kind,omitempty" jsonschema:"node kind filter"`
	NameContains  string `json:"name_contains,omitempty" jsonschema:"substring match on name/qualified_name"`
	Path          string `json:"path,omitempty" jsonschema:"exact file path filter"`
	Language      string `json:"language,omitempty" jsonschema:"language filter"`
	EdgeKind      string `json:"edge_kind,omitempty" jsonschema:"require an edge of this kind (e.g. calls, imports)"`
	EdgeDirection string `json:"edge_direction,omitempty" jsonschema:"out (node is source) or in (node is target)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max results"`
}
type QueryGraphOutput struct {
	Count int           `json:"count"`
	Nodes []GraphSymbol `json:"nodes"`
}

func queryGraph(ctx context.Context, a *app.App, in QueryGraphInput) (QueryGraphOutput, error) {
	spec := graph.QuerySpec{
		Kind: in.Kind, NameLike: in.NameContains, Path: in.Path,
		Language: in.Language, Limit: in.Limit,
	}
	if in.EdgeKind != "" {
		spec.Edge = &graph.EdgeFilter{Kind: in.EdgeKind, Direction: in.EdgeDirection}
	}
	nodes, err := a.Graph.Query(ctx, a.Git.Worktree, spec)
	if err != nil {
		return QueryGraphOutput{}, err
	}
	out := QueryGraphOutput{Count: len(nodes), Nodes: make([]GraphSymbol, 0, len(nodes))}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, GraphSymbol{
			Name: n.Name, QualifiedName: n.QualifiedName, Kind: n.Kind, Path: n.Path, Line: n.Line,
		})
	}
	return out, nil
}

// --- get_code_snippet ---

type GetCodeSnippetInput struct {
	File   string `json:"file" jsonschema:"repo-relative file path"`
	Symbol string `json:"symbol" jsonschema:"symbol name or qualified name"`
}
type GetCodeSnippetOutput struct {
	Snippet string `json:"snippet"`
}

func getCodeSnippet(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	snip, err := a.Graph.Snippet(ctx, a.Git.Worktree, in.File, in.Symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	return GetCodeSnippetOutput{Snippet: snip}, nil
}
```

- [ ] **Step 4: Register the tools in `server.go`**

In `NewServer`, before `return s`, add:
```go
	addTool(s, a, "search_graph",
		"Search the code graph for symbols by name or keywords (functions, methods, classes, types).",
		searchGraph)
	addTool(s, a, "trace_call_path",
		"Trace call chains to/from a symbol (direction: callers or callees).",
		traceCallPath)
	addTool(s, a, "get_architecture",
		"Summarize the codebase structure: modules with file and symbol counts.",
		getArchitecture)
	addTool(s, a, "query_graph",
		"Run a structured query over the code graph (node filters + optional edge traversal).",
		queryGraph)
	addTool(s, a, "get_code_snippet",
		"Return the exact source of a symbol by file + name, instead of reading the whole file.",
		getCodeSnippet)
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test ./internal/mcp/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/graph.go internal/mcp/server.go internal/mcp/graph_test.go internal/mcp/helpers_test.go
git commit -m "feat(mcp): expose search_graph/trace_call_path/get_architecture/query_graph/get_code_snippet"
```

---

## Task 24: hooks — SessionStart reconcile, PostEdit reindex, Augment

**Files:**
- Modify: `internal/agents/claudecode/hooks.go:49-56` (SessionStart reconcile)
- Create: `internal/agents/claudecode/graphhooks.go`
- Modify: `internal/agents/claudecode/install.go:21-27` (hookSpecs); `internal/cli/hook.go:19-28` (dispatch)
- Test: `internal/agents/claudecode/graphhooks_test.go`

- [ ] **Step 1: Write the failing test**

`internal/agents/claudecode/graphhooks_test.go`:
```go
package claudecode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostEditIndexesFile(t *testing.T) {
	a := newTestApp(t) // reuse the existing claudecode test helper
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Edited(){}"), 0o644)
	payload := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	if err := PostEdit(context.Background(), a, strings.NewReader(payload), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var n int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name='Edited'`).Scan(&n)
	if n != 1 {
		t.Errorf("Edited node count = %d, want 1 (file not indexed)", n)
	}
}

func TestAugmentInjectsSearchContext(t *testing.T) {
	a := newTestApp(t)
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	// index it
	pe := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	PostEdit(context.Background(), a, strings.NewReader(pe), &bytes.Buffer{})

	var out bytes.Buffer
	aug := `{"session_id":"s1","tool_input":{"pattern":"Authenticate"}}`
	if err := Augment(context.Background(), a, strings.NewReader(aug), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Authenticate") {
		t.Errorf("augment output = %q, want it to mention Authenticate", out.String())
	}
}
```
(If `newTestApp`/`fakeGit` aren't already in the `claudecode` test package, copy them from `internal/cli/helpers_test.go` into `internal/agents/claudecode/helpers_test.go`.)

- [ ] **Step 2: Run it to verify failure**

Run: `CGO_ENABLED=1 go test ./internal/agents/claudecode/ -run 'TestPostEdit|TestAugment' -v`
Expected: FAIL (undefined `PostEdit`/`Augment`).

- [ ] **Step 3: Implement `graphhooks.go`**

`internal/agents/claudecode/graphhooks.go`:
```go
package claudecode

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// editInput is the subset of PostToolUse(Edit|Write) we use.
type editInput struct {
	SessionID string `json:"session_id"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// PostEdit incrementally re-indexes the file the agent just edited, then resolves
// references. Exit-0 advisory: any error is returned for the caller to log, never
// to block. Unsupported files are skipped.
func PostEdit(ctx context.Context, a *app.App, in io.Reader, _ io.Writer) error {
	var hi editInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	abs := hi.ToolInput.FilePath
	if abs == "" {
		return nil
	}
	wt := a.Git.Worktree
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // outside the worktree; ignore
	}
	rel = filepath.ToSlash(rel)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil // file vanished between edit and hook; ignore
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	if err := a.Graph.IndexFile(ctx, wt, rel, content, info.ModTime().UnixMilli(), info.Size()); err != nil {
		return err
	}
	return a.Graph.Resolve(ctx, wt)
}

// augmentInput is the subset of PostToolUse(Grep|Glob) we use.
type augmentInput struct {
	ToolInput struct {
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
	} `json:"tool_input"`
}

// Augment runs a code-graph search matching the agent's Grep/Glob pattern and
// injects the structured result as advisory additionalContext (non-blocking).
func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi augmentInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	pattern := hi.ToolInput.Pattern
	if pattern == "" {
		pattern = hi.ToolInput.Query
	}
	if pattern == "" {
		return nil
	}
	res, err := a.Graph.Search(ctx, a.Git.Worktree, pattern, "", "", 8)
	if err != nil || len(res) == 0 {
		return err // nothing to add (or non-blocking error)
	}
	var b strings.Builder
	b.WriteString("Recarto code graph — symbols matching your search:\n")
	for _, r := range res {
		fmt.Fprintf(&b, "- [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
	}
	b.WriteString("Use get_code_snippet to read one symbol instead of the whole file.")
	return writeHookContext(out, "PostToolUse", b.String())
}
```

- [ ] **Step 4: Extend SessionStart to reconcile**

In `internal/agents/claudecode/hooks.go`, inside `SessionStart`, after the `_ = a.Ledger.Incr(ctx, "lifetime", "sessions", 1)` line and before the `Recall`, add:
```go
	// Warm the code graph for this worktree (best-effort, non-blocking).
	_ = a.Graph.Reconcile(ctx, a.Git.Worktree, graph.FSSource{Root: a.Git.Worktree})
```
Add the import `"git.gerwim.nl/gerwim/recarto/internal/graph"` to `hooks.go` (it now references `graph.FSSource`).

- [ ] **Step 5: Wire the new hooks into the installer and dispatcher**

In `internal/agents/claudecode/install.go`, extend `hookSpecs`:
```go
	return []hookSpec{
		{"SessionStart", "", binary + " hook session-start"},
		{"PostToolUse", "Read", binary + " hook post-read"},
		{"PostToolUse", "Edit|Write", binary + " hook post-edit"},
		{"PostToolUse", "Grep|Glob", binary + " hook augment"},
		{"Stop", "", binary + " hook stop"},
	}
```
In `internal/cli/hook.go`, add cases to the switch:
```go
		case "post-edit":
			return claudecode.PostEdit(ctx, a, in, out)
		case "augment":
			return claudecode.Augment(ctx, a, in, out)
```
And update the two error strings in `hook.go` to list the new events: `(session-start|post-read|post-edit|augment|stop)`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/agents/claudecode/ ./internal/cli/ -v`
Expected: PASS. (The existing `install_test.go` idempotency test now installs 5 hooks; update any count assertion from 3 to 5.)

- [ ] **Step 7: Commit**

```bash
git add internal/agents/claudecode/ internal/cli/hook.go
git commit -m "feat(hooks): graph reconcile on session-start, post-edit reindex, grep augment"
```

---

## Task 25: CI — CGO toolchain + cross-compile smoke

**Files:**
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Inspect the current workflow**

Run: `cat .github/workflows/ci.yml`
Expected: the Phase 1 build/vet/test/gofmt jobs (pure-Go, no CC).

- [ ] **Step 2: Ensure CGO + a C compiler on the test job**

Edit the existing test job so steps run with CGO enabled and a compiler present. On `ubuntu-latest`/`macos-latest` `cc` is preinstalled; for `windows-latest` Go uses the bundled gcc via `CGO_ENABLED=1` only if a gcc is on PATH — add a step before the build on Windows:
```yaml
      - name: Install C toolchain (Windows)
        if: runner.os == 'Windows'
        run: choco install mingw -y
      - name: Build
        run: go build ./...
        env:
          CGO_ENABLED: "1"
      - name: Test
        run: go test ./...
        env:
          CGO_ENABLED: "1"
```
Keep the existing `gofmt` check step unchanged.

- [ ] **Step 3: Add a cross-compile smoke job (zig cc) for the three ship targets**

Append a job:
```yaml
  crosscompile:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26.4'
      - uses: mlugg/setup-zig@v1
      - name: Build linux/amd64
        run: GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl" go build -o /tmp/recarto-linux ./cmd/recarto
      - name: Build darwin/arm64
        run: GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC="zig cc -target aarch64-macos" go build -o /tmp/recarto-darwin ./cmd/recarto
      - name: Build windows/amd64
        run: GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-windows-gnu" go build -o /tmp/recarto.exe ./cmd/recarto
```
(If a target's grammar C fails under a specific zig target triple, that is the §16 risk surfacing — adjust the triple or grammar pin. This job is the early-warning system.)

- [ ] **Step 4: Validate the workflow locally as far as possible**

Run: `GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl" go build -o /tmp/recarto-linux ./cmd/recarto` (requires zig installed locally; otherwise rely on CI).
Expected: a static binary at `/tmp/recarto-linux`. Verify staticness: `file /tmp/recarto-linux` → "statically linked".

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: CGO toolchain on test matrix + zig cc cross-compile smoke (3 targets)"
```

---

## Final verification

- [ ] **Step 1: Full suite, race, and format gate**

Run:
```bash
CGO_ENABLED=1 go build ./... \
  && CGO_ENABLED=1 go test ./... \
  && CGO_ENABLED=1 go test -race ./internal/store/ ./internal/memory/ ./internal/parse/ ./internal/graph/ \
  && test -z "$(gofmt -l .)"
```
Expected: all PASS; `gofmt -l .` prints nothing.

- [ ] **Step 2: End-to-end binary smoke**

Run:
```bash
CGO_ENABLED=1 go build -o /tmp/recarto ./cmd/recarto
cd "$(mktemp -d)" && git init -q && printf 'package p\nfunc A(){ B() }\nfunc B(){}\n' > a.go
RECARTO_CACHE_DIR="$(mktemp -d)" /tmp/recarto index
RECARTO_CACHE_DIR="$(mktemp -d)" /tmp/recarto graph search A
```
Expected: `index` prints "indexed 1 files, 2 symbols"; `graph search A` lists `[function] A  a.go:2`. (Use the same `RECARTO_CACHE_DIR` for both commands to query what you indexed.)

- [ ] **Step 3: Confirm Phase 2 exit criteria (spec §18)** — index builds a worktree graph; all five tools answer over MCP + CLI; reconcile + post-edit keep it current with no daemon; augment injects context; concurrent worktrees stay isolated (Task 14); token savings recorded (Task 16); CGO links statically for the three targets (Task 25); suite + race + gofmt green.

---

## Notes for the implementer

- **Tree-sitter node names can differ by grammar version.** When a golden test (Tasks 5–8) shows an empty capture, print `tree.RootNode().ToSexp()` for the fixture and adjust the relevant `.scm`. The pinned versions from Task 1 are the source of truth.
- **Never call `ledger.Incr` inside a `store.WriteTx`** (non-reentrant write mutex → deadlock). All `_ = e.led.Incr(...)` calls in the graph engine are deliberately on read paths or after the write tx commits.
- **FTS5 is manually synced.** Any new code path that deletes/replaces nodes must also delete the matching `nodes_fts` rows first (see `writeFileGraph` and `deleteFile`).
- **Worktree scoping is enforced by joining through `files`.** Every read query filters `f.worktree = ?`; never query `nodes`/`edges` without that join, or you will leak another worktree's graph.
