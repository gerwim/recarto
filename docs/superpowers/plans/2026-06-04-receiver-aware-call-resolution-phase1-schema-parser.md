# Receiver-Aware Call Resolution — Phase 1 (Schema + Parser + Indexer) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture call receivers and per-scope type bindings for C# and Go, fix Go method qualified names to include the receiver type, and persist all of it — so a later phase can resolve `x.Method()` to the right `Type.Method`.

**Architecture:** Tree-sitter `.scm` queries gain receiver + type-binding captures; `parse.go` turns them into `Edge.Recv`, `Symbol.RecvType`, and a new `TypeBinding`; `index.go` persists them (new columns/tables) and synthesizes Go method qualified names; a schema migration (v9) adds the storage and wipes the rebuildable graph so existing caches re-index.

**Tech Stack:** Go, tree-sitter (`github.com/tree-sitter/go-tree-sitter`), SQLite (`modernc.org`/FTS5). Spec: `docs/superpowers/specs/2026-06-04-receiver-aware-call-resolution-design.md`.

**Scope of this phase:** Sections 2 (data model), 3 (parser captures), 4 (indexer) of the spec. The resolver (Section 5), trace/MCP surface (Section 6), and oracles (eval) are Phases 2–4.

---

## File structure (this phase)

| File | Responsibility | Action |
|---|---|---|
| `internal/store/migrate.go` | Schema v9: `edges.dst_recv`, `type_bindings`, `edge_candidates`; graph wipe | Modify |
| `internal/store/migrate_test.go` | Assert v9 schema + idempotent re-run | Modify |
| `internal/parse/parse.go` | `Edge.Recv`, `Symbol.RecvType`, `TypeBinding`, `Result.Bindings`; capture handling | Modify |
| `internal/parse/queries/csharp.scm` | Capture call receiver + field/param/local/`new` type bindings | Modify |
| `internal/parse/queries/go.scm` | Capture call receiver, method receiver, param/local/composite-literal bindings | Modify |
| `internal/parse/parse_test.go` | Parser unit tests for receiver + bindings (C# + Go) | Modify |
| `internal/graph/index.go` | Persist `dst_recv` + `type_bindings`; Go method qualified names | Modify |
| `internal/graph/index_test.go` | Indexer unit tests for the above | Modify |

**Convention reminder (from `.wolf/cerebrum.md` Do-Not-Repeat):** a tree-sitter `.scm` query is a silent allow-list, and grammar node/field names are not obvious (e.g. C# file-scoped namespaces, TS enum identifiers). For every `.scm` task below, **probe the grammar with a ToSexp test first** to confirm the real node/field names before trusting the pattern in this plan.

---

## Task 1: Schema migration v9 (storage + graph wipe)

**Files:**
- Modify: `internal/store/migrate.go:10` (schemaVersion) and the `migrations` slice (append v9)
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/migrate_test.go`:

```go
func TestMigrateV9AddsResolutionSchema(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// New tables exist.
	for _, table := range []string{"type_bindings", "edge_candidates"} {
		var name string
		if err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}
	// edges has the new dst_recv column.
	rows, err := st.DB.Query(`PRAGMA table_info(edges)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hasRecv := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "dst_recv" {
			hasRecv = true
		}
	}
	if !hasRecv {
		t.Error("edges.dst_recv column missing")
	}
}
```

Ensure `database/sql` is imported in the test file (it already imports `fmt`, `path/filepath`, `testing`).

Also add this guard, pinning the standing invariant that a graph-invalidating
migration NEVER deletes memories/bugs (bugs are `memories` rows with `kind='bug'`):

```go
func TestV9MigrationPreservesMemories(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a memory and a bug, plus a stale graph index as a prior version left it.
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('m1','learning','keep me','body',1,1)`)
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('b1','bug','a bug','body',1,1)`)
	mustExec(t, st, `INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		VALUES('f1','/wt','A.cs','csharp','h',1,1,1)`)
	mustExec(t, st, `INSERT INTO meta(key,value) VALUES('graph_token:/wt','tok')`)
	// Simulate a store written by the previous binary, forcing v9 to re-run on reopen.
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1))
	st.Close()

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var mem int
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&mem); err != nil {
		t.Fatal(err)
	}
	if mem != 2 {
		t.Errorf("memories after migration = %d, want 2 (migrations must never delete memories/bugs)", mem)
	}
	var files int
	if err := st2.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Errorf("graph not rebuilt: files = %d, want 0", files)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestMigrateV9AddsResolutionSchema|TestV9MigrationPreservesMemories' -v`
Expected: `TestMigrateV9AddsResolutionSchema` FAILs — `type_bindings` not created / `dst_recv` missing. (`TestV9MigrationPreservesMemories` may pass even pre-implementation since no migration deletes memories — that's fine; it is a standing regression guard, not a red-first driver.)

- [ ] **Step 3: Implement migration v9**

In `internal/store/migrate.go`, change `const schemaVersion = 8` to `const schemaVersion = 9`. Append this string as the last element of the `migrations` slice (after the v8 entry, before the closing `}`):

```go
	// v9 (receiver-aware call resolution): add per-scope type bindings and
	// per-edge candidate sets with confidence, plus a receiver column on edges.
	// edges is rebuildable, so it is DROP+CREATEd with the new dst_recv column
	// rather than ALTERed — keeping this migration idempotent, which the rebuild
	// test (TestLatestGraphMigrationRebuildsIndex) requires of the last migration.
	// Wipe the derived graph + freshness token so the new indexer repopulates it.
	// See the v5 comment for the standing convention.
	`
DROP TABLE IF EXISTS edge_candidates;
DROP TABLE IF EXISTS type_bindings;

DROP TABLE IF EXISTS edges;
CREATE TABLE edges (
  id          TEXT PRIMARY KEY,
  src_node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
  dst_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
  dst_name    TEXT NOT NULL DEFAULT '',
  dst_recv    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  line        INTEGER NOT NULL
);
CREATE INDEX idx_edges_src     ON edges(src_node_id, kind);
CREATE INDEX idx_edges_dst     ON edges(dst_node_id);
CREATE INDEX idx_edges_dstname ON edges(dst_name);

CREATE TABLE type_bindings (
  id               TEXT PRIMARY KEY,
  file_id          TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  type_name        TEXT NOT NULL,
  scope_start_byte INTEGER NOT NULL,
  scope_end_byte   INTEGER NOT NULL
);
CREATE INDEX idx_type_bindings_file ON type_bindings(file_id);
CREATE INDEX idx_type_bindings_name ON type_bindings(name);

CREATE TABLE edge_candidates (
  edge_id     TEXT NOT NULL REFERENCES edges(id) ON DELETE CASCADE,
  dst_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  confidence  REAL NOT NULL DEFAULT 0,
  method      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (edge_id, dst_node_id)
);
CREATE INDEX idx_edge_candidates_dst ON edge_candidates(dst_node_id);

DELETE FROM nodes_fts;
DELETE FROM nodes_fts_stem;
DELETE FROM nodes_fts_tri;
DELETE FROM nodes;
DELETE FROM files;
DELETE FROM meta WHERE key LIKE 'graph_token:%';
`,
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS — including `TestMigrateV9AddsResolutionSchema`, `TestMigrateCreatesGraphTables`, and crucially `TestLatestGraphMigrationRebuildsIndex` (proves v9 is idempotent on re-run, since it resets `user_version` to `schemaVersion-1` and re-applies v9).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): schema v9 — type_bindings, edge_candidates, edges.dst_recv

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Parser data model — `Edge.Recv`, `Symbol.RecvType`, `TypeBinding`

**Files:**
- Modify: `internal/parse/parse.go` (types + capture loop)
- Test: `internal/parse/parse_test.go`

This task adds the fields and the capture-loop plumbing using a **generic capture convention** the `.scm` files (Tasks 3–4) will target:
- `@recv` on a call match → the receiver expression text → `Edge.Recv`.
- `@recv.type` + `@recv.name` on a method definition → receiver type + var → `Symbol.RecvType` and a `TypeBinding` for the receiver var.
- `@bind.name` + `@bind.type` on a declaration match → a `TypeBinding`.

- [ ] **Step 1: Write the failing test**

Add to `internal/parse/parse_test.go`:

```go
func TestParseGoCapturesReceiverAndBindings(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	res, err := reg.ParseFile("go", "saga.go", src)
	if err != nil {
		t.Fatal(err)
	}

	// The Handle method carries its receiver type.
	var handle *Symbol
	for i := range res.Symbols {
		if res.Symbols[i].Name == "Handle" {
			handle = &res.Symbols[i]
		}
	}
	if handle == nil || handle.RecvType != "Saga" {
		t.Fatalf("Handle RecvType = %v, want Saga", handle)
	}
	// The call s.step() carries its receiver expression.
	foundCall := false
	for _, e := range res.Edges {
		if e.Kind == "calls" && e.DstName == "step" && e.Recv == "s" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Errorf("no calls edge with DstName=step Recv=s; edges=%v", res.Edges)
	}
	// The receiver var s is a binding of type Saga.
	foundBind := false
	for _, b := range res.Bindings {
		if b.Name == "s" && b.TypeName == "Saga" {
			foundBind = true
		}
	}
	if !foundBind {
		t.Errorf("no binding s:Saga; bindings=%v", res.Bindings)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/parse/ -run TestParseGoCapturesReceiverAndBindings -v`
Expected: FAIL — `res.Bindings` / `RecvType` / `Recv` undefined (compile error first; that counts as the failing state — fix by Steps 3–4 then Tasks 3–4 supply the captures).

- [ ] **Step 3: Add the types**

In `internal/parse/parse.go`, extend the structs:

```go
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
```

- [ ] **Step 4: Extend the capture loop**

In `ParseFile`, replace the per-match body (the `for m := matches.Next(); ...` loop) with the version below. It adds `recv`, `recvType`, `recvName` + byte, and `bindName`/`bindType` + byte, and emits the new outputs. A leading `*` (Go pointer receiver / type) is stripped so `*Saga` → `Saga`:

```go
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
		case bindName != "" && bindType != "":
			res.Bindings = append(res.Bindings,
				TypeBinding{Name: bindName, TypeName: bindType, StartByte: bindByte})
		}
	}
```

- [ ] **Step 5: Run tests to verify they compile (test still fails on captures)**

Run: `go test ./internal/parse/ -run TestParseGoCapturesReceiverAndBindings -v`
Expected: still FAIL, but now on assertions (not compile) — the `.scm` captures don't exist yet. That's correct; Task 4 supplies them. Do **not** commit a red test alone; this task's commit happens after the structs compile cleanly with the rest of the suite green:

Run: `go test ./internal/parse/ -run 'TestParse$|TestLangFor|TestNewRegistry' -v` (existing tests)
Expected: PASS (the new fields are additive).

- [ ] **Step 6: Commit**

```bash
git add internal/parse/parse.go internal/parse/parse_test.go
git commit -m "feat(parse): receiver + type-binding capture plumbing (Edge.Recv, RecvType, TypeBinding)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: C# captures — call receiver + type bindings

**Files:**
- Modify: `internal/parse/queries/csharp.scm`
- Test: `internal/parse/parse_test.go`

- [ ] **Step 1: Probe the grammar (mandatory, per convention)**

Add a temporary probe test to confirm the real C# node/field names, run it, read the s-expression, then delete it:

```go
func TestProbeCsharpGrammar(t *testing.T) {
	reg, _ := NewRegistry()
	defer reg.Close()
	src := []byte("namespace N;\nclass A {\n  private readonly Dep _dep;\n" +
		"  void M(Other o) { var x = new Thing(); _dep.Run(); }\n}")
	t.Log(reg.ToSexp("csharp", src)) // add ToSexp helper if not present (see note)
}
```

Note: if `Registry` has no `ToSexp`, add a small test-only helper in `parse_test.go` that parses and prints `tree.RootNode().ToSexp()`. Confirm the field names used below: `member_access_expression` has `expression:`/`name:`; `field_declaration` wraps `variable_declaration type: … (variable_declarator (identifier))`; `parameter` has `type:`/`name:`; `local_declaration_statement` wraps a `variable_declaration`; `object_creation_expression` has `type:`. Adjust the patterns in Step 3 to match what the probe prints.

Run: `go test ./internal/parse/ -run TestProbeCsharpGrammar -v`. Read output. Delete the probe test.

- [ ] **Step 2: Write the failing test**

Add to `internal/parse/parse_test.go`:

```go
func TestParseCsharpCapturesReceiverAndBindings(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	src := []byte("namespace N;\n" +
		"class A {\n" +
		"  private readonly Dep _dep;\n" +
		"  void M(Other o) { var x = new Thing(); _dep.Run(); }\n" +
		"}")
	res, err := reg.ParseFile("csharp", "A.cs", src)
	if err != nil {
		t.Fatal(err)
	}

	wantBind := map[string]string{"_dep": "Dep", "o": "Other", "x": "Thing"}
	got := map[string]string{}
	for _, b := range res.Bindings {
		got[b.Name] = b.TypeName
	}
	for n, ty := range wantBind {
		if got[n] != ty {
			t.Errorf("binding %s = %q, want %q; all=%v", n, got[n], ty, res.Bindings)
		}
	}
	foundCall := false
	for _, e := range res.Edges {
		if e.Kind == "calls" && e.DstName == "Run" && e.Recv == "_dep" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Errorf("no calls edge Run recv=_dep; edges=%v", res.Edges)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/parse/ -run TestParseCsharpCapturesReceiverAndBindings -v`
Expected: FAIL — bindings empty, Recv empty.

- [ ] **Step 4: Add captures to `internal/parse/queries/csharp.scm`**

Change the member-access call pattern to also capture the receiver, and append the binding patterns (verify exact field names against the Step 1 probe):

Replace line 13:
```
(invocation_expression function: (member_access_expression name: (identifier) @name)) @reference.call
```
with:
```
(invocation_expression function: (member_access_expression expression: (_) @recv name: (identifier) @name)) @reference.call
```

Append at end of file:
```
; type bindings: field / property / parameter / typed local / new
(field_declaration (variable_declaration type: (_) @bind.type (variable_declarator name: (identifier) @bind.name)))
(property_declaration type: (_) @bind.type name: (identifier) @bind.name)
(parameter type: (_) @bind.type name: (identifier) @bind.name)
(local_declaration_statement (variable_declaration type: (_) @bind.type (variable_declarator name: (identifier) @bind.name)))
(variable_declarator name: (identifier) @bind.name (equals_value_clause (object_creation_expression type: (_) @bind.type)))
```

Note on `var x = new Thing()`: the `local_declaration_statement` pattern binds `x` to type `var` (useless); the `object_creation_expression` pattern then binds `x` to `Thing` (useful). Both fire; the indexer (Task 5 / Phase 2) prefers a non-`var` type when a name has multiple bindings in the same scope. For this phase the test only asserts `x:Thing` is present among bindings.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/parse/ -run TestParseCsharpCapturesReceiverAndBindings -v`
Expected: PASS. Also run `go test ./internal/parse/ -v` — all green (the `NewRegistry` query must still compile; a malformed `.scm` fails at `NewRegistry`).

- [ ] **Step 6: Commit**

```bash
git add internal/parse/queries/csharp.scm internal/parse/parse_test.go
git commit -m "feat(parse): C# call-receiver + type-binding captures

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Go captures — call receiver, method receiver, type bindings

**Files:**
- Modify: `internal/parse/queries/go.scm`
- Test: `internal/parse/parse_test.go` (the `TestParseGoCapturesReceiverAndBindings` from Task 2 now goes green)

- [ ] **Step 1: Probe the grammar (mandatory)**

Temporary probe (delete after reading):

```go
func TestProbeGoGrammar(t *testing.T) {
	reg, _ := NewRegistry()
	defer reg.Close()
	src := []byte("package p\ntype Saga struct{}\n" +
		"func (s *Saga) Handle(o Other) { x := Thing{}; s.step() }\n")
	t.Log(reg.ToSexp("go", src))
}
```

Confirm: `method_declaration` has `receiver: (parameter_list (parameter_declaration name: (identifier) type: (pointer_type (type_identifier))))` and `name: (field_identifier)`; `call_expression function: (selector_expression operand: (_) field: (field_identifier))`; `short_var_declaration left: (expression_list (identifier)) right: (expression_list (composite_literal type: (type_identifier)))`; `parameter_declaration name:/type:`. Adjust Step 3 patterns to match.

Run, read, delete.

- [ ] **Step 2: Verify the Task 2 test still fails on Go captures**

Run: `go test ./internal/parse/ -run TestParseGoCapturesReceiverAndBindings -v`
Expected: FAIL — RecvType/Recv/binding still empty (no Go captures yet).

- [ ] **Step 3: Add captures to `internal/parse/queries/go.scm`**

Replace line 2:
```
(method_declaration name: (field_identifier) @name) @definition.method
```
with (captures the receiver var + type):
```
(method_declaration receiver: (parameter_list (parameter_declaration name: (identifier) @recv.name type: [(pointer_type (type_identifier) @recv.type) (type_identifier) @recv.type])) name: (field_identifier) @name) @definition.method
```

Replace line 5:
```
(call_expression function: (selector_expression field: (field_identifier) @name)) @reference.call
```
with:
```
(call_expression function: (selector_expression operand: (_) @recv field: (field_identifier) @name)) @reference.call
```

Append at end of file:
```
; type bindings: parameter / typed var / := composite literal / := &composite / new(T)
(parameter_declaration name: (identifier) @bind.name type: [(type_identifier) @bind.type (pointer_type (type_identifier) @bind.type)])
(var_spec name: (identifier) @bind.name type: [(type_identifier) @bind.type (pointer_type (type_identifier) @bind.type)])
(short_var_declaration left: (expression_list (identifier) @bind.name) right: (expression_list (composite_literal type: (type_identifier) @bind.type)))
(short_var_declaration left: (expression_list (identifier) @bind.name) right: (expression_list (unary_expression operand: (composite_literal type: (type_identifier) @bind.type))))
```

Note: a method with an unnamed receiver (`func (*Saga) M()`) won't match `name: (identifier) @recv.name`; add a second `method_declaration` pattern without `@recv.name` if the probe shows unnamed receivers occur — RecvType still resolves the qualified name; only the receiver-var binding is skipped.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/parse/ -run 'TestParseGoCapturesReceiverAndBindings|TestParseCsharpCapturesReceiverAndBindings' -v`
Expected: PASS (both). Then `go test ./internal/parse/ -v` — all green.

- [ ] **Step 5: Commit**

```bash
git add internal/parse/queries/go.scm
git commit -m "feat(parse): Go call-receiver, method-receiver, and type-binding captures

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Indexer — persist `dst_recv` + `type_bindings`; Go method qualified names

**Files:**
- Modify: `internal/graph/index.go` (edge insert; new bindings insert; Go qualified name)
- Test: `internal/graph/index_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/graph/index_test.go`:

```go
func TestIndexFilePersistsReceiverBindingsAndGoQName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	if err := e.IndexFile(ctx, "/wt", "saga.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Go method qualified name carries the receiver type.
	var qn string
	if err := e.st.DB.QueryRow(
		`SELECT qualified_name FROM nodes WHERE name='Handle'`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != "Saga.Handle" {
		t.Errorf("Handle qualified_name = %q, want Saga.Handle", qn)
	}

	// The call edge stored its receiver.
	var recv string
	if err := e.st.DB.QueryRow(
		`SELECT dst_recv FROM edges WHERE kind='calls' AND dst_name='step'`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	if recv != "s" {
		t.Errorf("step call dst_recv = %q, want s", recv)
	}

	// The receiver var binding is scoped to the Handle method body.
	var typeName string
	var scopeStart, scopeEnd int
	if err := e.st.DB.QueryRow(
		`SELECT type_name, scope_start_byte, scope_end_byte FROM type_bindings WHERE name='s' AND type_name='Saga' LIMIT 1`).
		Scan(&typeName, &scopeStart, &scopeEnd); err != nil {
		t.Fatal(err)
	}
	if scopeEnd <= scopeStart {
		t.Errorf("binding scope invalid: [%d,%d]", scopeStart, scopeEnd)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/graph/ -run TestIndexFilePersistsReceiverBindingsAndGoQName -v`
Expected: FAIL — `qualified_name` is `Handle` (not `Saga.Handle`); `dst_recv`/`type_bindings` unpopulated.

- [ ] **Step 3: Implement — Go qualified name + dst_recv + bindings**

In `internal/graph/index.go`, `writeFileGraph`:

(a) After `qn := qualifiedName(spans, res.Symbols, i)` (around line 90), special-case Go methods (a bare method name with a receiver type):

```go
		qn := qualifiedName(spans, res.Symbols, i)
		if s.RecvType != "" && !strings.Contains(qn, ".") {
			qn = s.RecvType + "." + s.Name
		}
```

(b) Change the call-edge insert (around line 126-134) to write `dst_recv`:

```go
	for _, ed := range res.Edges {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,dst_recv,kind,file_id,line)
			 VALUES (?,?,?,?,?,?,?,?)`,
			ids.New(), nullStr(enclosing(spans, ed.StartByte)), nil, ed.DstName, ed.Recv, ed.Kind,
			fileID, int64(ed.Line)); err != nil {
			return err
		}
	}
```

(The structural `contains` edge insert keeps `dst_recv` at its default `''`; leave that INSERT's column list unchanged — it omits `dst_recv`, which defaults to `''`. If you prefer symmetry, add `dst_recv` with `""`.)

(c) After the edges loop, before the `contains`-edge loop, insert type bindings. Each binding's scope is the span of its innermost enclosing definition (class for fields, method for params/locals), found with the existing `enclosing` helper; fall back to whole-file scope when a binding sits at file scope:

```go
	// Type bindings: scope each to its innermost enclosing definition's byte span
	// (class body for fields, method body for params/locals/receiver vars).
	fileEnd := uint(len(src))
	_ = fileEnd
	for _, b := range res.Bindings {
		scStart, scEnd := uint(0), uint(len(hashSrcLen(res)))
		if encID := enclosing(spans, b.StartByte); encID != "" {
			for _, sp := range spans {
				if sp.id == encID {
					scStart, scEnd = sp.start, sp.end
					break
				}
			}
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO type_bindings(id,file_id,name,type_name,scope_start_byte,scope_end_byte)
			 VALUES (?,?,?,?,?,?)`,
			ids.New(), fileID, b.Name, b.TypeName, int64(scStart), int64(scEnd)); err != nil {
			return err
		}
	}
```

Correction — `writeFileGraph` does not receive `src`, so file length isn't available there, and there is no `hashSrcLen`. Use the file's max node end-byte as the file-scope fallback instead. Replace the binding loop with:

```go
	// Type bindings: scope each to its innermost enclosing definition's byte span.
	var maxEnd uint
	for _, sp := range spans {
		if sp.end > maxEnd {
			maxEnd = sp.end
		}
	}
	for _, b := range res.Bindings {
		scStart, scEnd := uint(0), maxEnd
		if encID := enclosing(spans, b.StartByte); encID != "" {
			for _, sp := range spans {
				if sp.id == encID {
					scStart, scEnd = sp.start, sp.end
					break
				}
			}
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO type_bindings(id,file_id,name,type_name,scope_start_byte,scope_end_byte)
			 VALUES (?,?,?,?,?,?)`,
			ids.New(), fileID, b.Name, b.TypeName, int64(scStart), int64(scEnd)); err != nil {
			return err
		}
	}
```

Ensure `strings` is imported in `index.go` (it already imports `strings` for `qualifiedName`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/graph/ -run TestIndexFilePersistsReceiverBindingsAndGoQName -v`
Expected: PASS.
Run: `go test ./internal/graph/ ./internal/store/ ./internal/parse/`
Expected: PASS (the v9 wipe means existing graph tests re-index fresh; `dst_recv` is additive; `TestTraceCallPathCallersRespectsResolution` from the bug-110 fix still passes).

- [ ] **Step 5: Commit**

```bash
git add internal/graph/index.go internal/graph/index_test.go
git commit -m "feat(graph): persist dst_recv + type_bindings; Go method qualified names

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## End-of-phase verification

- [ ] **Full build + suite**

Run: `go build ./... && go test ./...`
Expected: all PASS. (No ORT build tag — default suite is memory-safe.)

- [ ] **Manual smoke (optional, throwaway dir — never run recarto in the repo root, per cerebrum)**

```bash
tmp=$(mktemp -d); cd "$tmp"; git init -q
printf 'package p\ntype Saga struct{}\nfunc (s *Saga) Handle(){ s.step() }\nfunc (s *Saga) step(){}\n' > saga.go
go run <path-to-recarto>/cmd/recarto index .   # or the project's index entrypoint
# verify: a graph search for Saga.Handle returns the qualified name
```

---

## Phase 1 → Phase 2 handoff

Phase 1 produces: `dst_recv`, `type_bindings` (scoped), `edge_candidates` (empty table), and Go method qualified names — all persisted and tested. **Nothing reads `edge_candidates` yet**; resolution is still the existing name-keyed `Resolve` + the bug-110-gated trace.

**Phase 2** (separate plan) implements the multi-strategy resolver (S1–S5) writing `edge_candidates` + the denormalized top `dst_node_id`, per spec Section 5.

## Self-review notes (author)

- **Spec coverage:** Section 2 (schema) → Task 1; Section 3 (parser captures) → Tasks 2–4; Section 4 (indexer: persistence + Go qnames) → Task 5. `edge_candidates` table is created in Task 1 but populated in Phase 2 (correctly out of scope here).
- **Idempotency risk handled:** v9 uses DROP+CREATE for `edges` (not ALTER) so `TestLatestGraphMigrationRebuildsIndex`'s re-run of the last migration passes.
- **Type consistency:** `Edge.Recv`, `Symbol.RecvType`, `TypeBinding{Name,TypeName,StartByte}`, `Result.Bindings` are defined in Task 2 and used identically in Tasks 3–5; columns `dst_recv`, `type_bindings(name,type_name,scope_start_byte,scope_end_byte)` match between Task 1 and Task 5.
- **Grammar uncertainty:** Tasks 3–4 require a ToSexp probe first (project convention) — the `.scm` patterns here are the starting point to verify, not blind truth.
