# LLM-Facing API Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Recarto's public MCP/hook/instructions surface easier for a consuming LLM to use correctly: real enums, a `forget_memory` tool, a structured snippet result, inline signatures, recall match-reasons + truncation flags, trace suggestions-on-miss, and refreshed agent docs.

**Architecture:** Seven independent units (A–G). MCP handlers stay SDK-free (logic in `internal/mcp/handlers.go` + `graph.go`; SDK only in `server.go`). Engine logic lives in `internal/memory` and `internal/graph`. Each unit is TDD'd against its handler/engine seam and ends in a commit. Units are independent except E, which changes `Engine.Recall`'s return type and updates its six callers.

**Tech Stack:** Go, `modelcontextprotocol/go-sdk` v1.6.1, `google/jsonschema-go` v0.4.3, modernc SQLite (`internal/store`), tree-sitter graph (`internal/graph`).

**Pre-flight (run once before Task 1):**
```bash
cd /home/claude/Projects/recarto-feature-llm-features
git checkout -b feature/llm-features-impl   # if not already on a working branch
go test ./... >/dev/null && echo "baseline green"
```

---

## Key facts confirmed during planning (do not re-derive)

- **Enums cannot go in struct tags.** `jsonschema-go` treats the whole `jsonschema:"..."` tag as the field *description* (`infer.go:336`) and errors on `WORD=` prefixes. Enums must be set on the `*jsonschema.Schema` via `jsonschema.For[In](nil)` then `schema.Properties["field"].Enum = []any{...}`, passed as `Tool.InputSchema`. `Schema.Enum` is `[]any` (`schema.go:74`).
- **`forget` is a hard delete.** `Engine.deleteMemoryRows` (`internal/memory/compact.go:147`) already removes the FTS rows (`lex.Delete`) and the `memories` row. Reuse it. No new status, no CHECK-constraint migration.
- **Snippet line range is already stored** (`nodes.start_line/end_line`); `Snippet` just doesn't return it.
- **The `signature` column already exists** (`internal/store/migrate.go:381`, default `''`) but is written as `""` ("reserved for a future version", `internal/graph/index.go:156`). Unit D *populates + surfaces* it; it does not add a column.
- **Recall already computes per-signal scores** in `RecallResult.Signals` (`internal/memory/recall.go:48`); signal weights are `wStructural=0.45, wLexical=0.25, wRecency=0.15, wReinforcement=0.10, wBranch=0.05` (`recall.go:16-20`).
- **Six `Engine.Recall` callers** (Unit E): `internal/mcp/handlers.go:97`, `:237`; `internal/agents/handle.go:77`, `:188`; `internal/cli/recall.go:34`; `internal/cli/bug.go:100`.

---

## Test setup conventions (real signatures — use exactly these)

Confirmed from the existing suites. Do **not** invent helpers; the tasks below assume these.

- **`internal/memory`** — `func newTestEngine(t *testing.T, clk clock.Clock) *Engine` (takes a clock). Use:
  ```go
  clk := clock.NewFake(time.UnixMilli(10_000_000))
  e := newTestEngine(t, clk)
  ```
- **`internal/mcp`** — `func newTestApp(t *testing.T, branch string) *app.App`. Graph tests index a temp worktree via `Reconcile` (pattern from `internal/mcp/graph_test.go:TestSearchGraphHandler`):
  ```go
  a := newTestApp(t, "main")
  wt := t.TempDir()
  a.Git.Worktree = wt
  os.WriteFile(filepath.Join(wt, "a.go"),
      []byte("package p\nfunc Greet(name string) string { return name }"), 0o644)
  if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
      t.Fatal(err)
  }
  ```
- **`internal/graph`** — `func newTestEngine(t *testing.T) *Engine` (no clock). Index inline; the worktree is the literal `"/wt"`:
  ```go
  e := newTestEngine(t)
  src := []byte("package p\nfunc Greet(name string) string { return name }")
  if err := e.IndexFile(context.Background(), "/wt", "sample.go", src, 1, int64(len(src))); err != nil {
      t.Fatal(err)
  }
  // then query with worktree "/wt", path "sample.go"
  ```
- **`internal/store`** — open with `store.Open(dbPath)` then `Migrate(st.DB)`. For version assertions, prefer asserting the `schemaVersion`/`len(migrations)` constants directly (no special store helper exists).

Wherever a task below names `newIndexedTestGraph`, `newTestAppWithGraph`, or `openMigratedTestStore`, substitute the matching pattern above (the task notes the substitution at its call site).

---

## Unit A — Schema hardening: `kind` + closed-set enums

### Task A1: Ordered `AllKinds` slice

**Files:**
- Modify: `internal/memory/record.go:8-23`
- Test: `internal/memory/record_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/memory/record_test.go
func TestAllKindsCoversValidKinds(t *testing.T) {
	if len(AllKinds) != len(validKinds) {
		t.Fatalf("AllKinds has %d, validKinds has %d", len(AllKinds), len(validKinds))
	}
	for _, k := range AllKinds {
		if !k.Valid() {
			t.Errorf("AllKinds entry %q not in validKinds", k)
		}
	}
	if AllKinds[len(AllKinds)-1] != KindTodo {
		t.Errorf("expected todo last, got %q", AllKinds[len(AllKinds)-1])
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/memory/ -run TestAllKindsCoversValidKinds`
Expected: FAIL — `undefined: AllKinds`.

- [ ] **Step 3: Implement — define `AllKinds` and derive `validKinds` from it**

In `internal/memory/record.go`, replace the `validKinds` map literal with:

```go
// AllKinds is the canonical, ordered list of memory kinds. It is the single
// source of truth: validKinds is derived from it, and the MCP kind enum lists it.
var AllKinds = []Kind{
	KindPreference, KindLearning, KindDecision,
	KindDoNotRepeat, KindBug, KindAction, KindTodo,
}

var validKinds = func() map[Kind]bool {
	m := make(map[Kind]bool, len(AllKinds))
	for _, k := range AllKinds {
		m[k] = true
	}
	return m
}()
```

(Leave the `Kind...` constants and `func (k Kind) Valid()` unchanged.)

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/memory/ -run TestAllKindsCoversValidKinds`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/record.go internal/memory/record_test.go
git commit -m "refactor(memory): ordered AllKinds slice as kind source of truth"
```

### Task A2: Enum-bearing tool schemas in the MCP server

**Files:**
- Modify: `internal/mcp/server.go`
- Create: `internal/mcp/schema.go`
- Test: `internal/mcp/schema_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/mcp/schema_test.go
package mcp

import (
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestRecordMemorySchemaHasKindEnum(t *testing.T) {
	sc := recordMemorySchema()
	p := sc.Properties["kind"]
	if p == nil {
		t.Fatal("no kind property")
	}
	if len(p.Enum) != len(memory.AllKinds) {
		t.Fatalf("enum len = %d, want %d", len(p.Enum), len(memory.AllKinds))
	}
	got := map[string]bool{}
	for _, v := range p.Enum {
		got[v.(string)] = true
	}
	if !got["todo"] {
		t.Error("kind enum missing todo")
	}
}

func TestTraceAndQueryEnums(t *testing.T) {
	if got := traceCallPathSchema().Properties["direction"].Enum; len(got) != 2 {
		t.Errorf("direction enum = %v, want 2 values", got)
	}
	if got := queryGraphSchema().Properties["edge_direction"].Enum; len(got) != 2 {
		t.Errorf("edge_direction enum = %v, want 2 values", got)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/mcp/ -run 'TestRecordMemorySchema|TestTraceAndQueryEnums'`
Expected: FAIL — `undefined: recordMemorySchema`.

- [ ] **Step 3: Implement the schema builders**

Create `internal/mcp/schema.go`:

```go
package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// schemaFor builds the auto-derived input schema for In, panicking on the
// (compile-time-impossible) reflection error so callers stay expression-simple.
func schemaFor[In any]() *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	return s
}

// withEnum sets a JSON-Schema enum on one property, turning a free-text string
// field into a closed set the client validates and the model sees explicitly.
func withEnum(s *jsonschema.Schema, field string, vals ...string) *jsonschema.Schema {
	if p := s.Properties[field]; p != nil {
		for _, v := range vals {
			p.Enum = append(p.Enum, any(v))
		}
	}
	return s
}

func kindStrings() []string {
	out := make([]string, len(memory.AllKinds))
	for i, k := range memory.AllKinds {
		out[i] = string(k)
	}
	return out
}

func recordMemorySchema() *jsonschema.Schema {
	return withEnum(schemaFor[RecordMemoryInput](), "kind", kindStrings()...)
}

func traceCallPathSchema() *jsonschema.Schema {
	return withEnum(schemaFor[TraceCallPathInput](), "direction", "callers", "callees")
}

func queryGraphSchema() *jsonschema.Schema {
	return withEnum(schemaFor[QueryGraphInput](), "edge_direction", "in", "out")
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/mcp/ -run 'TestRecordMemorySchema|TestTraceAndQueryEnums'`
Expected: PASS.

- [ ] **Step 5: Wire the schemas into the registered tools**

In `internal/mcp/server.go`, add a sibling registrar that accepts a prebuilt schema:

```go
// addToolSchema is addTool with an explicit InputSchema, used where a field needs
// a JSON-Schema enum (the jsonschema struct tag is description-only and cannot
// express enum).
func addToolSchema[In, Out any](s *sdk.Server, a *app.App, name, desc string,
	schema *jsonschema.Schema, fn func(context.Context, *app.App, In) (Out, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: desc, InputSchema: schema},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			out, err := fn(ctx, a, in)
			return nil, out, err
		})
}
```

Add the import `"github.com/google/jsonschema-go/jsonschema"` to `server.go`. Change the three registrations to use it:

```go
addToolSchema(s, a, "record_memory",
	"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, action, or todo (open work that resurfaces until done).",
	recordMemorySchema(), recordMemory)
// ...
addToolSchema(s, a, "trace_call_path", /* existing description */ traceCallPathSchema(), traceCallPath)
// ...
addToolSchema(s, a, "query_graph",
	"Run a structured query over the code graph (node filters + optional edge traversal).",
	queryGraphSchema(), queryGraph)
```

- [ ] **Step 6: Fix the `kind` field description (the near-bug) + run full package**

In `internal/mcp/handlers.go:33`, update the tag to include `todo`:

```go
Kind string `json:"kind" jsonschema:"memory kind: preference, learning, decision, do_not_repeat, bug, action, or todo"`
```

Run: `go test ./internal/mcp/ ./internal/memory/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/schema.go internal/mcp/schema_test.go internal/mcp/server.go internal/mcp/handlers.go go.mod go.sum
git commit -m "feat(mcp): real JSON-Schema enums for kind/direction/edge_direction; add todo to kind doc"
```

---

## Unit B — `forget_memory` tool

### Task B1: `Engine.Forget`

**Files:**
- Create: `internal/memory/forget.go`
- Test: `internal/memory/forget_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/memory/forget_test.go
package memory

import (
	"context"
	"testing"
)

func TestForgetRemovesMemory(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	id, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "x", Body: "forget me please"})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.Forget(ctx, id, "recorded in error")
	if err != nil || !ok {
		t.Fatalf("Forget = %v, %v; want true, nil", ok, err)
	}
	if rec, _ := e.GetByID(ctx, id); rec != nil {
		t.Error("memory still present after Forget")
	}
	// Gone from the candidate pool too — List no longer returns it. (Asserted via
	// List, not Recall, so this test is independent of Unit E's return-type change.)
	recs, _ := e.List(ctx, ListFilter{})
	for _, r := range recs {
		if r.ID == id {
			t.Error("forgotten memory still listed")
		}
	}
}

func TestForgetUnknownIDIsNoOp(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ok, err := e.Forget(context.Background(), "nope", "")
	if err != nil || ok {
		t.Fatalf("Forget unknown = %v, %v; want false, nil", ok, err)
	}
}
```

(Imports: `"github.com/.../internal/clock"` and `"time"` — match the existing memory `*_test.go` imports.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/memory/ -run TestForget`
Expected: FAIL — `e.Forget undefined`.

- [ ] **Step 3: Implement**

Create `internal/memory/forget.go`:

```go
package memory

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Forget hard-deletes a memory by id: the row, its refs, and its lexical index
// entry are removed (reusing deleteMemoryRows, the same teardown gcSuperseded
// uses). Unlike complete_todo it is not status-gated — any memory recorded in
// error can be removed. reason is advisory (for the caller's intent) and is not
// persisted, since the row is gone. Returns (false, nil) when id is unknown.
func (e *Engine) Forget(ctx context.Context, id, reason string) (bool, error) {
	_ = reason // advisory only; nothing to persist on a deleted row
	var found bool
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		var got string
		err := c.QueryRowContext(ctx, `SELECT id FROM memories WHERE id=?`, id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return e.deleteMemoryRows(ctx, c, []string{id})
	})
	if err != nil {
		return false, err
	}
	if found {
		_ = e.led.Incr(ctx, "lifetime", "memories_forgotten", 1)
	}
	return found, nil
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/memory/ -run TestForget`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/forget.go internal/memory/forget_test.go
git commit -m "feat(memory): Engine.Forget hard-deletes a memory by id"
```

### Task B2: `forget_memory` MCP tool

**Files:**
- Modify: `internal/mcp/handlers.go`, `internal/mcp/server.go`
- Test: `internal/mcp/handlers_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/mcp/handlers_test.go
func TestForgetMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "temp"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: rec.ID, Reason: "test"})
	if err != nil || !out.Forgotten {
		t.Fatalf("forgetMemory = %+v, %v; want Forgotten=true", out, err)
	}
	miss, _ := forgetMemory(ctx, a, ForgetMemoryInput{ID: "nope"})
	if miss.Forgotten {
		t.Error("unknown id reported as forgotten")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/mcp/ -run TestForgetMemoryHandler`
Expected: FAIL — `undefined: forgetMemory`.

- [ ] **Step 3: Implement the handler**

Append to `internal/mcp/handlers.go`:

```go
// ForgetMemoryInput is the argument to the forget_memory tool.
type ForgetMemoryInput struct {
	ID     string `json:"id" jsonschema:"id of the memory to delete"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is being forgotten (advisory; not stored)"`
}

// ForgetMemoryOutput reports whether a memory was deleted.
type ForgetMemoryOutput struct {
	Forgotten bool `json:"forgotten"`
}

func forgetMemory(ctx context.Context, a *app.App, in ForgetMemoryInput) (ForgetMemoryOutput, error) {
	ok, err := a.Engine.Forget(ctx, in.ID, in.Reason)
	if err != nil {
		return ForgetMemoryOutput{}, err
	}
	return ForgetMemoryOutput{Forgotten: ok}, nil
}
```

- [ ] **Step 4: Register the tool**

In `internal/mcp/server.go`, after the `complete_todo` registration add:

```go
addTool(s, a, "forget_memory",
	"Delete a memory recorded in error (hard delete; the memory is gone). No-op if the id is unknown. To replace rather than remove, record a new memory with supersedes_id.",
	forgetMemory)
```

- [ ] **Step 5: Run test, verify it passes**

Run: `go test ./internal/mcp/ -run TestForgetMemoryHandler`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/server.go internal/mcp/handlers_test.go
git commit -m "feat(mcp): forget_memory tool"
```

---

## Unit C — `get_code_snippet` structured result + miss convention

### Task C1: `Snippet` returns a struct; miss is not an error

**Files:**
- Modify: `internal/graph/snippet.go`
- Test: `internal/graph/snippet_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/graph/snippet_test.go — Snippet reads the file from disk, so use a
// real worktree dir + the writeFile/FSSource helpers the existing test uses.
func TestSnippetResultFoundAndMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "sample.go", "package p\n\nfunc Greet(name string) string {\n\treturn name\n}\n")
	src, _ := FSSource{Root: wt}.Read("sample.go")
	if err := e.IndexFile(ctx, wt, "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	got, err := e.Snippet(ctx, wt, "sample.go", "Greet")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.StartLine == 0 || got.Snippet == "" {
		t.Fatalf("found symbol: %+v", got)
	}

	miss, err := e.Snippet(ctx, wt, "sample.go", "DoesNotExist")
	if err != nil {
		t.Fatalf("miss should not error, got %v", err)
	}
	if miss.Found {
		t.Error("expected Found=false for unknown symbol")
	}
}
```

> The existing `TestSnippetReturnsSymbolSource` (`internal/graph/snippet_test.go:9`) asserts the **string** return (`strings.Contains(snip, ...)`). Update it in this step: `snip, err := e.Snippet(...)` → `res, err := e.Snippet(...)`, then assert against `res.Snippet`.

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/graph/ -run TestSnippetResultFoundAndMiss`
Expected: FAIL — compile error (`Snippet` returns `(string, error)`, `.Found` undefined).

- [ ] **Step 3: Implement — change `Snippet`'s signature and select line/qname**

Replace `internal/graph/snippet.go`'s `Snippet` with:

```go
// SnippetResult is the structured result of Snippet. A miss is Found=false (not
// an error); only genuine faults (stale byte range, vanished file) return an error.
type SnippetResult struct {
	Found         bool
	StartLine     int
	EndLine       int
	QualifiedName string
	Snippet       string
}

func (e *Engine) Snippet(ctx context.Context, worktree, path, symbol string) (SnippetResult, error) {
	var sb, eb int64
	var sl, el int
	var qn string
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte, n.start_line, n.end_line, n.qualified_name
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND (n.name = ? OR n.qualified_name = ?)
 LIMIT 1`, worktree, path, symbol, symbol).Scan(&sb, &eb, &sl, &el, &qn)
	if errors.Is(err, sql.ErrNoRows) {
		return SnippetResult{Found: false}, nil
	}
	if err != nil {
		return SnippetResult{}, err
	}
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if err != nil {
		return SnippetResult{}, err
	}
	if sb < 0 || eb > int64(len(content)) || sb > eb {
		return SnippetResult{}, fmt.Errorf("graph: stale byte range for %q (reindex needed)", symbol)
	}
	snippet := string(content[sb:eb])

	saved := ledger.EstimateTokens(string(content)) - ledger.EstimateTokens(snippet)
	if saved > 0 {
		_ = e.led.Incr(ctx, "lifetime", "graph_tokens_saved", int64(saved))
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return SnippetResult{Found: true, StartLine: sl, EndLine: el, QualifiedName: qn, Snippet: snippet}, nil
}
```

- [ ] **Step 4: Run test, verify it fails to compile at the MCP caller**

Run: `go build ./...`
Expected: FAIL — `internal/mcp/graph.go` `getCodeSnippet` still treats the result as a string. (Fixed next.)

- [ ] **Step 5: Update the MCP handler**

In `internal/mcp/graph.go`, replace `GetCodeSnippetOutput` and `getCodeSnippet`:

```go
type GetCodeSnippetOutput struct {
	Found         bool   `json:"found"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Snippet       string `json:"snippet,omitempty"`
}

func getCodeSnippet(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	ensureFresh(ctx, a)
	r, err := a.Graph.Snippet(ctx, a.Git.Worktree, in.File, in.Symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	return GetCodeSnippetOutput{
		Found: r.Found, StartLine: r.StartLine, EndLine: r.EndLine,
		QualifiedName: r.QualifiedName, Snippet: r.Snippet,
	}, nil
}
```

- [ ] **Step 6: Update any other `Snippet` callers**

Run: `grep -rn "\.Snippet(" --include=*.go internal cmd | grep -v _test`
For each hit outside `internal/mcp/graph.go` (e.g. a CLI `graph` subcommand), adapt it to the struct: a miss is `!r.Found` (print "symbol not found"); success uses `r.Snippet`.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/graph/ ./internal/mcp/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/graph/snippet.go internal/graph/snippet_test.go internal/mcp/graph.go internal/cli/
git commit -m "feat(graph): get_code_snippet returns {found,start_line,end_line,qualified_name,snippet}; miss is not an error"
```

---

## Unit D — Inline signatures in search_graph / query_graph

### Task D1: Populate `signature` at index time

**Files:**
- Modify: `internal/graph/index.go`
- Test: `internal/graph/index_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/graph/index_test.go — Search reads the DB only (no disk), so the
// virtual "/wt" worktree is fine here.
func TestSignaturePopulatedOnIndex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Greet(name string) string { return name }")
	if err := e.IndexFile(ctx, "/wt", "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(ctx, "/wt", "Greet", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	var sig string
	for _, r := range res {
		if r.Name == "Greet" {
			sig = r.Signature
		}
	}
	if !strings.Contains(sig, "func Greet(") {
		t.Fatalf("signature = %q, want it to contain the declaration line", sig)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/graph/ -run TestSignaturePopulatedOnIndex`
Expected: FAIL — `r.Signature` undefined.

- [ ] **Step 3: Implement — thread `src` into `writeFileGraph` and compute the signature**

In `internal/graph/index.go`:

1. Add a helper:

```go
// signatureOf returns the symbol's first source line (trimmed, capped), used as a
// cheap one-line signature surfaced in search results.
func signatureOf(src []byte, start uint, max int) string {
	if int(start) >= len(src) {
		return ""
	}
	line := src[start:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	s := strings.TrimSpace(string(line))
	if len(s) > max {
		s = s[:max]
	}
	return s
}
```

(Add `"bytes"` to the imports.)

2. Change `writeFileGraph`'s signature (line 94) to take `src`:

```go
func (e *Engine) writeFileGraph(ctx context.Context, c store.Conn, st *writeStmts, worktree, path, lang string,
	src []byte, res parse.Result, hash string, mtime, size, now int64) error {
```

3. In the node-insert loop (≈line 155), replace the empty signature literal:

```go
		if _, err := st.node.ExecContext(ctx,
			spans[i].id, fileID, s.Kind, s.Name, qn, parentSegment(qn),
			signatureOf(src, s.StartByte, 200),
			int64(s.StartByte), int64(s.EndByte), int64(s.StartLine), int64(s.EndLine)); err != nil {
			return err
		}
```

4. Update the call site in `IndexFile` (line ≈36) to pass `src`:

```go
		return e.writeFileGraph(ctx, c, st, worktree, path, lang, src, res, hash, mtime, size, now)
```

- [ ] **Step 4: Add `Signature` to `SearchResult` and select it**

In `internal/graph/search.go`:
- Add `Signature string` to the `SearchResult` struct (after `Path`).
- In the exact-match query (line 154) change the SELECT to `SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line, n.signature` and the scan (line 182) to append `&r.Signature`.
- In `ftsPass` (line 299) change the SELECT to add `n.signature` before `bm25(...)`, and the scan (line 325) to `&r.Signature` before `&bm`.

```go
// exact-match scan:
if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
	&r.Language, &r.Path, &r.Line, &r.Signature); err != nil {
// ftsPass scan:
if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
	&r.Language, &r.Path, &r.Line, &r.Signature, &bm); err != nil {
```

- [ ] **Step 5: Run test, verify it passes**

Run: `go test ./internal/graph/ -run TestSignaturePopulatedOnIndex`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/graph/index.go internal/graph/search.go internal/graph/index_test.go
git commit -m "feat(graph): populate node signature (first source line) and return it in search results"
```

### Task D2: Surface signature in MCP search_graph + query_graph

**Files:**
- Modify: `internal/graph/query.go`, `internal/mcp/graph.go`
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/mcp/graph_test.go (mirror TestSearchGraphHandler's setup)
func TestSearchGraphReturnsSignature(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Greet(name string) string { return name }"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "Greet"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range out.Results {
		if r.Name == "Greet" && strings.Contains(r.Signature, "func Greet(") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no result carried the Greet signature: %+v", out.Results)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/mcp/ -run TestSearchGraphReturnsSignature`
Expected: FAIL — `r.Signature` undefined on `GraphSymbol`.

- [ ] **Step 3: Implement**

In `internal/mcp/graph.go`:
- Add `Signature string` to `GraphSymbol` (json `"signature,omitempty"`).
- In `searchGraph`, set `Signature: r.Signature` in the `GraphSymbol` literal.
- In `queryGraph`, set `Signature: n.Signature` (requires query.go below).

In `internal/graph/query.go`:
- Add `Signature string` to the `QueryNode` struct.
- Change the SELECT (line 43) to `SELECT n.id, n.name, n.qualified_name, n.kind, f.path, n.start_line, n.signature`.
- Change the scan (line 83) to append `&n.Signature`.

- [ ] **Step 4: Run tests, verify they pass**

Run: `go test ./internal/mcp/ ./internal/graph/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/query.go internal/mcp/graph.go internal/mcp/graph_test.go
git commit -m "feat(mcp): surface symbol signature in search_graph and query_graph results"
```

### Task D3: Migration to backfill existing caches

**Files:**
- Modify: `internal/store/migrate.go`
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/store/migrate_test.go
func TestSchemaVersionIs15(t *testing.T) {
	if schemaVersion != 15 {
		t.Fatalf("schemaVersion = %d, want 15", schemaVersion)
	}
	if len(migrations) != 15 {
		t.Fatalf("len(migrations) = %d, want 15", len(migrations))
	}
	// Open auto-migrates (open() calls Migrate). migrate_test.go is `package store`,
	// so Open is unqualified. Confirm the version pragma advanced to 15.
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 15 {
		t.Fatalf("user_version = %d, want 15", v)
	}
}
```

(Add `"path/filepath"` to the test imports if not present.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/store/ -run TestSchemaVersionIs15`
Expected: FAIL — `schemaVersion = 14`.

- [ ] **Step 3: Implement — append migration #15 and bump the version**

In `internal/store/migrate.go`: change `const schemaVersion = 14` to `15`, and append to the `migrations` slice (after the current last entry):

```go
	// v15: graph cache reset so the now-populated node `signature` column
	// backfills on the next reconcile. Clears graph rows + freshness tokens;
	// memories are untouched. ReconcileIfStale re-indexes every file.
	`
DELETE FROM nodes_fts_tri;
DELETE FROM nodes_fts_stem;
DELETE FROM nodes_fts;
DELETE FROM edges;
DELETE FROM nodes;
DELETE FROM files;
DELETE FROM meta WHERE key LIKE 'graph_token:%';
`,
```

> If any of `nodes_fts`/`nodes_fts_stem`/`nodes_fts_tri` is a contentless FTS5 table that rejects plain `DELETE`, use `INSERT INTO <tbl>(<tbl>) VALUES('delete-all');` instead for that table — check the table's DDL earlier in this file before finalizing. (The `edges`/`nodes`/`files` deletes are plain tables.)

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/store/ -run TestSchemaVersionIs15`
Expected: PASS.

- [ ] **Step 5: Run the store package + a graph reconcile smoke test**

Run: `go test ./internal/store/ ./internal/graph/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): migration 15 resets graph cache to backfill node signatures"
```

---

## Unit E — Recall metadata: matched_on, min_score, truncated, budget_used

### Task E1: `RecallResult.MatchedOn`

**Files:**
- Modify: `internal/memory/recall.go`
- Test: `internal/memory/recall_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/memory/recall_test.go
func TestMatchedOnDominantSignal(t *testing.T) {
	r := RecallResult{Signals: map[string]float64{
		"structural": 1.0, "lexical": 0, "recency": 0, "reinforcement": 0, "branch": 0,
	}}
	if got := r.MatchedOn(); len(got) != 1 || got[0] != "structural" {
		t.Fatalf("MatchedOn = %v, want [structural]", got)
	}

	none := RecallResult{Signals: map[string]float64{}}
	if got := none.MatchedOn(); got != nil {
		t.Fatalf("MatchedOn (no signals) = %v, want nil", got)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/memory/ -run TestMatchedOnDominantSignal`
Expected: FAIL — `r.MatchedOn undefined`.

- [ ] **Step 3: Implement**

Append to `internal/memory/recall.go`:

```go
// MatchedOn names the signals that drove this result's score, ordered by weighted
// contribution (desc), keeping any whose contribution is at least 20% of the top
// signal's. It lets a consuming LLM see *why* a memory surfaced. Returns nil when
// nothing contributed.
func (r RecallResult) MatchedOn() []string {
	weights := map[string]float64{
		"structural": wStructural, "lexical": wLexical, "recency": wRecency,
		"reinforcement": wReinforcement, "branch": wBranch,
	}
	type contrib struct {
		name string
		v    float64
	}
	var cs []contrib
	var max float64
	for name, w := range weights {
		v := w * r.Signals[name]
		if v > max {
			max = v
		}
		cs = append(cs, contrib{name, v})
	}
	if max <= 0 {
		return nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].v > cs[j].v })
	var out []string
	for _, c := range cs {
		if c.v >= 0.2*max {
			out = append(out, c.name)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/memory/ -run TestMatchedOnDominantSignal`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go
git commit -m "feat(memory): RecallResult.MatchedOn surfaces the signals behind a match"
```

### Task E2: `min_score` + `RecallResultSet` (truncated, budget_used)

**Files:**
- Modify: `internal/memory/recall.go`, and all six callers
- Test: `internal/memory/recall_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/memory/recall_test.go
func TestRecallReportsTruncation(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _, _ = e.Record(ctx, NewMemory{Kind: KindLearning, Body: "shared keyword payload number " + itoa(i)})
	}
	// Tiny budget forces truncation of the keyword-matched candidates.
	set, err := e.Recall(ctx, RecallQuery{TaskHint: "shared keyword payload", TokenBudget: 12})
	if err != nil {
		t.Fatal(err)
	}
	if !set.Truncated {
		t.Error("expected Truncated=true under a tiny budget")
	}
	if set.TokensUsed <= 0 {
		t.Error("expected positive TokensUsed")
	}
}

func TestRecallMinScoreFilters(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	_, _, _ = e.Record(ctx, NewMemory{Kind: KindLearning, Body: "exact relevant text"})
	set, _ := e.Recall(ctx, RecallQuery{TaskHint: "exact relevant text", MinScore: 0.999})
	if len(set.Results) != 0 {
		t.Errorf("MinScore=0.999 should exclude all, got %d", len(set.Results))
	}
}
```

(`itoa` = `strconv.Itoa`; import as needed.)

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/memory/ -run 'TestRecallReportsTruncation|TestRecallMinScoreFilters'`
Expected: FAIL — `set.Truncated` undefined / `Recall` returns a slice.

- [ ] **Step 3: Implement — new return type, MinScore filter, truncation flag**

In `internal/memory/recall.go`:

1. Add `MinScore float64` to `RecallQuery`.

2. Add the result-set type:

```go
// RecallResultSet wraps the selected results with budget telemetry so a caller
// can tell whether ranked candidates were dropped for space (Truncated) and how
// much of the budget the returned set consumed (TokensUsed).
type RecallResultSet struct {
	Results    []RecallResult
	TokensUsed int
	Truncated  bool
}
```

3. After the `scored` loop and before budgeting, drop sub-threshold candidates:

```go
	if q.MinScore > 0 {
		kept := scored[:0]
		for _, s := range scored {
			if s.Score >= q.MinScore {
				kept = append(kept, s)
			}
		}
		scored = kept
	}
```

4. Change `selectWithinBudget` to also report truncation:

```go
func selectWithinBudget(scored []RecallResult, budget int) (out []RecallResult, used int, truncated bool) {
	// ... existing pinned/rest split and sort unchanged ...
	for _, s := range pinned {
		out = append(out, s)
		used += tokensOf(s.Record)
	}
	for _, s := range rest {
		t := tokensOf(s.Record)
		if used+t > budget {
			truncated = true
			continue
		}
		out = append(out, s)
		used += t
	}
	return out, used, truncated
}
```

5. Change `Recall`'s signature to `(RecallResultSet, error)` and its returns:

```go
func (e *Engine) Recall(ctx context.Context, q RecallQuery) (RecallResultSet, error) {
	// ... unchanged through scoring; replace error returns `return nil, err`
	//     with `return RecallResultSet{}, err` ...
	out, used, truncated := selectWithinBudget(scored, budget)
	if len(out) == 0 {
		_ = e.led.Incr(ctx, "lifetime", "recall_misses", 1)
		return RecallResultSet{}, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "memories_recalled", int64(len(out)))
	_ = e.led.Incr(ctx, "lifetime", "recall_tokens_injected", int64(used))
	_ = e.stampRecalled(ctx, out)
	return RecallResultSet{Results: out, TokensUsed: used, Truncated: truncated}, nil
}
```

- [ ] **Step 4: Update the six callers to `.Results`**

- `internal/agents/handle.go:77` (`handleSessionStart`): `set, err := a.Engine.Recall(...)`; then `results := set.Results` (the rest of the function uses `results` unchanged).
- `internal/agents/handle.go:188` (`handleAugment`): `set, err := ...`; iterate `set.Results`.
- `internal/cli/recall.go:34`: iterate `set.Results`.
- `internal/cli/bug.go:100`: iterate `set.Results`.
- `internal/mcp/handlers.go:237` (`searchBugs`): `set, err := ...`; iterate `set.Results`.
- `internal/mcp/handlers.go:97` (`recallMemory`): handled in Task E3.

For each, the minimal diff is renaming the receiver and adding `.Results` at the range/len site. Run `grep -rn "\.Recall(" --include=*.go internal cmd | grep -v _test` to confirm none are missed.

- [ ] **Step 5: Run tests + full build**

Run: `go build ./... && go test ./internal/memory/ ./internal/agents/ ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go internal/agents/handle.go internal/cli/recall.go internal/cli/bug.go internal/mcp/handlers.go
git commit -m "feat(memory): Recall returns RecallResultSet with min_score filter, truncated + tokens_used"
```

### Task E3: Surface matched_on / truncated / budget_used on recall_memory

**Files:**
- Modify: `internal/mcp/handlers.go`
- Test: `internal/mcp/handlers_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/mcp/handlers_test.go
func TestRecallMemoryExposesMetadata(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	_, _ = recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: "alpha beta gamma", Files: []string{"x.go"}})
	out, err := recallMemory(ctx, a, RecallMemoryInput{TaskHint: "alpha beta gamma", Files: []string{"x.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Memories[0].MatchedOn) == 0 {
		t.Fatalf("expected matched_on populated, got %+v", out.Memories)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/mcp/ -run TestRecallMemoryExposesMetadata`
Expected: FAIL — `MatchedOn` undefined on `MemoryView`.

- [ ] **Step 3: Implement**

In `internal/mcp/handlers.go`:
- Add to `MemoryView`: `MatchedOn []string `json:"matched_on,omitempty"``.
- Add to `RecallMemoryInput`: `MinScore float64 `json:"min_score,omitempty" jsonschema:"drop candidates scoring below this (0-1)"``.
- Add to `RecallMemoryOutput`: `Truncated bool `json:"truncated"`` and `BudgetUsed int `json:"budget_used"``.
- Rewrite `recallMemory`:

```go
func recallMemory(ctx context.Context, a *app.App, in RecallMemoryInput) (RecallMemoryOutput, error) {
	set, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint:    in.TaskHint,
		Files:       in.Files,
		Symbols:     in.Symbols,
		Kind:        memory.Kind(in.Kind),
		Branch:      a.Git.Branch,
		Worktree:    a.Git.Worktree,
		Limit:       in.Limit,
		TokenBudget: in.TokenBudget,
		MinScore:    in.MinScore,
	})
	if err != nil {
		return RecallMemoryOutput{}, err
	}
	out := RecallMemoryOutput{
		Count: len(set.Results), Memories: make([]MemoryView, 0, len(set.Results)),
		Truncated: set.Truncated, BudgetUsed: set.TokensUsed,
	}
	for _, r := range set.Results {
		v := viewOf(r.Record)
		v.Score = r.Score
		v.Caveat = r.Caveat()
		v.MatchedOn = r.MatchedOn()
		out.Memories = append(out.Memories, v)
	}
	return out, nil
}
```

- Also set `v.MatchedOn = r.MatchedOn()` in `searchBugs` (consistency; harmless).

- [ ] **Step 4: Run tests, verify they pass**

Run: `go test ./internal/mcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/handlers_test.go
git commit -m "feat(mcp): recall_memory exposes matched_on, min_score, truncated, budget_used"
```

---

## Unit F — trace_call_path suggestions-on-miss

### Task F1: Suggestions from the graph layer

**Files:**
- Modify: `internal/graph/trace.go`
- Test: `internal/graph/trace_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/graph/trace_test.go
func TestTraceSuggestsOnMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Greet(name string) string { return name }")
	if err := e.IndexFile(ctx, "/wt", "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	chains, suggestions, err := e.TraceCallPathFiltered(ctx, "/wt", "Gree", "callees", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 0 {
		t.Errorf("expected no chains for a missed symbol, got %v", chains)
	}
	found := false
	for _, s := range suggestions {
		if s == "Greet" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'Greet' among suggestions, got %v", suggestions)
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/graph/ -run TestTraceSuggestsOnMiss`
Expected: FAIL — `TraceCallPathFiltered` returns two values, not three.

- [ ] **Step 3: Implement**

In `internal/graph/trace.go`, change `TraceCallPathFiltered` to return `([][]CallNode, []string, error)`. At the start-node miss branch (`if start.NodeID == ""`), build suggestions; on success return `nil` suggestions:

```go
func (e *Engine) TraceCallPathFiltered(ctx context.Context, worktree, symbol, direction string, depth int, minConf float64) ([][]CallNode, []string, error) {
	if depth <= 0 {
		depth = 3
	}
	start, err := e.nodeByName(ctx, worktree, symbol)
	if err != nil {
		return nil, nil, err
	}
	if start.NodeID == "" {
		// No exact match: offer lenient candidates (mirrors search_graph's hint)
		// instead of a silent empty result.
		var suggestions []string
		if res, serr := e.SearchGuided(ctx, worktree, symbol, []string{symbol}, "", "", 5); serr == nil {
			for _, r := range res {
				suggestions = append(suggestions, r.Name)
			}
		}
		return nil, suggestions, nil
	}
	// ... existing body unchanged ...
	return chains, nil, nil
}
```

Update every `return` inside the existing body to include the `nil` suggestions value (`return chains, nil, nil`, `return nil, nil, err`, etc.).

- [ ] **Step 4: Run test, verify it fails to compile at the MCP caller**

Run: `go build ./...`
Expected: FAIL — `internal/mcp/graph.go` `traceCallPath` expects two return values. (Fixed in F2.)

- [ ] **Step 5: Commit (after F2 builds)** — defer commit to F2 so the tree stays buildable.

### Task F2: Surface suggestions in the MCP tool

**Files:**
- Modify: `internal/mcp/graph.go`
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing test**

```go
// in internal/mcp/graph_test.go (mirror TestSearchGraphHandler's setup)
func TestTraceCallPathSuggestions(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Greet(name string) string { return name }"), 0o644)
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	out, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "Gree"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Chains) != 0 {
		t.Errorf("expected no chains, got %v", out.Chains)
	}
	if len(out.Suggestions) == 0 {
		t.Fatal("expected suggestions for a near-miss symbol")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/mcp/ -run TestTraceCallPathSuggestions`
Expected: FAIL — `out.Suggestions` undefined.

- [ ] **Step 3: Implement**

In `internal/mcp/graph.go`:
- Add `Suggestions []string `json:"suggestions,omitempty"`` to `TraceCallPathOutput`.
- Update `traceCallPath`:

```go
func traceCallPath(ctx context.Context, a *app.App, in TraceCallPathInput) (TraceCallPathOutput, error) {
	ensureFresh(ctx, a)
	dir := in.Direction
	if dir == "" {
		dir = "callees"
	}
	chains, suggestions, err := a.Graph.TraceCallPathFiltered(ctx, a.Git.Worktree, in.Symbol, dir, in.Depth, in.MinConfidence)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	out := TraceCallPathOutput{Chains: make([][]TraceHop, 0, len(chains)), Suggestions: suggestions}
	for _, ch := range chains {
		hops := make([]TraceHop, len(ch))
		for i, n := range ch {
			hops[i] = TraceHop{
				Symbol: n.Name, Path: n.Path, Line: n.Line,
				Resolution: n.Resolution, Confidence: n.Confidence, Receiver: n.Receiver,
			}
		}
		out.Chains = append(out.Chains, hops)
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests + full build**

Run: `go build ./... && go test ./internal/graph/ ./internal/mcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/trace.go internal/graph/trace_test.go internal/mcp/graph.go internal/mcp/graph_test.go
git commit -m "feat(graph,mcp): trace_call_path returns lenient suggestions on a symbol miss"
```

---

## Unit G — Docs + tool descriptions

### Task G1: Rewrite the AGENTS.md instructions block

**Files:**
- Modify: `internal/agents/instructions.go`
- Test: `internal/agents/install_test.go` (or a new `instructions_test.go` in the package)

- [ ] **Step 1: Write the failing test**

```go
// in internal/agents/instructions_test.go
package agents

import (
	"strings"
	"testing"
)

func TestInstructionsAdvertiseFullToolset(t *testing.T) {
	for _, want := range []string{
		"get_architecture", "query_graph", "list_memory", "get_memory",
		"forget_memory", "confidence",
	} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions block missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/agents/ -run TestInstructionsAdvertiseFullToolset`
Expected: FAIL — block lacks `get_architecture` etc.

- [ ] **Step 3: Implement — extend the `Instructions` const**

In `internal/agents/instructions.go`, extend the bullet list (keep the existing record/recall/todo bullets) with:

```
- **Orient first** in unfamiliar code with ` + "`get_architecture`" + ` (module/file/symbol
  map), then ` + "`search_graph`" + ` / ` + "`query_graph`" + ` to locate symbols and
  ` + "`get_code_snippet`" + ` to read one instead of a whole file. A lookup miss is an
  empty result, not an error.
- **Trace** with ` + "`trace_call_path`" + `: trust high/typed/self hops; treat low/name
  hops as candidates to verify with ` + "`get_code_snippet`" + `.
- **Inspect & curate** memories with ` + "`list_memory`" + ` / ` + "`get_memory`" + `; delete one
  recorded in error with ` + "`forget_memory`" + ` (or supersede it via ` + "`supersedes_id`" + `).
```

- [ ] **Step 4: Run test, verify it passes**

Run: `go test ./internal/agents/`
Expected: PASS. (If `install_test.go` asserts the exact block byte-for-byte, update that golden expectation too.)

- [ ] **Step 5: Commit**

```bash
git add internal/agents/instructions.go internal/agents/instructions_test.go
git commit -m "docs(agents): instructions advertise get_architecture/query_graph/list_memory/get_memory/forget_memory + trace confidence + miss convention"
```

### Task G2: Description polish for new fields/tools

**Files:**
- Modify: `internal/mcp/server.go`

- [ ] **Step 1: Update tool descriptions (no test; doc strings)**

In `internal/mcp/server.go`:
- `get_code_snippet`: end its description with " Returns found=false (not an error) when the symbol is unknown, plus its line range."
- `search_graph`: append " Each result carries a one-line signature."
- `recall_memory`: append " Each memory reports matched_on (the signals behind it); the result reports truncated/budget_used."

- [ ] **Step 2: Build + full test suite**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/mcp/server.go
git commit -m "docs(mcp): tool descriptions note new snippet/signature/recall-metadata fields"
```

---

## Final verification

- [ ] **Full suite green**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Manual smoke (optional but recommended)**

```bash
go build -o /tmp/recarto ./cmd/recarto
/tmp/recarto status   # confirms the new migration applies cleanly to a real cache
```
Expected: no migration error; status prints.

---

## Self-review notes (author)

- **Spec coverage:** A=#1, B=#2, C=#4+#5, D=#3, E=#6+#8, F=#7, G=#9. All nine items mapped.
- **Type consistency:** `RecallResultSet{Results,TokensUsed,Truncated}`, `SnippetResult{Found,StartLine,EndLine,QualifiedName,Snippet}`, `SearchResult.Signature`/`QueryNode.Signature`/`GraphSymbol.Signature`, `RecallQuery.MinScore`, `RecallResult.MatchedOn()`, `TraceCallPathFiltered(...) ([][]CallNode, []string, error)`, `Engine.Forget(ctx,id,reason)(bool,error)`, `memory.AllKinds` — names are used identically across tasks.
- **Test setup:** all task tests use the real helpers/signatures documented in "Test setup conventions" — `newTestEngine(t, clk)` (memory), `newTestEngine(t)` + inline `IndexFile` (graph), `newTestApp(t, "main")` + `Reconcile` (mcp), `Open` + auto-migrate (store). No invented helpers remain.
- **Pre-existing test to update:** Unit C changes `Snippet`'s return type, breaking `internal/graph/snippet_test.go:TestSnippetReturnsSymbolSource` (asserts the old string) — Task C1 Step 3 updates it.
- **Sequencing:** units are independent except E (changes `Engine.Recall`'s return type → 6 callers). Recommended order A→B→C→D→E→F→G; B asserts via `List`, so it does not depend on E.
- **Migration risk:** D3 includes a fallback for contentless-FTS5 `DELETE` rejection — verify the `nodes_fts*` DDL in `migrate.go` before finalizing the statement form.
