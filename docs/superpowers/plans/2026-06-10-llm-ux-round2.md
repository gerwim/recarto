# LLM-UX Round 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the four approved features of `docs/superpowers/specs/2026-06-10-llm-ux-round2-design.md`: CLI parity for round-1 tools, a token-budgeted graph delta on session resume, a gated block-once session-end capture nudge, and contradiction candidates on memory record.

**Architecture:** CLI rendering in `internal/cli/graph.go`; delta journaling at the single shared write seam `Engine.writeFileGraph` (+ `deleteFile`) with one new table (migration v19) and engine queries in a new `internal/graph/delta.go`; session-start tier in `internal/agents/format.go`/`handle.go`; nudge in `internal/agents` (neutral `Result` gains Block/Reason; only the claudecode adapter encodes it); conflict candidates inside `Engine.Record`'s existing transaction, surfaced by the MCP/CLI record paths.

**Tech Stack:** Go, SQLite (modernc), MCP go-sdk (confined to `internal/mcp/server.go`).

---

## Conventions (same as round 1)

- Engine tests: `internal/graph/*_test.go` with `newTestEngine(t)` (graph_test.go:14) and `indexInto(t, e, wt, rel, content)` (content_test.go:37). Memory tests: `internal/memory/*_test.go` (see engine_test.go for the fixture). MCP handler tests: `newTestApp(t, "main")` (handlers_test.go:40). CLI tests: `newTestApp(t, "main")` (internal/cli/helpers_test.go:38) + `Graph(ctx, a, &buf, args)` (see graph_test.go:12). Hook tests: `internal/agents/claudecode/hooks_test.go` drives `SessionStart/Stop(ctx, a, in, out)` with raw JSON payloads.
- Single test: `go test ./internal/<pkg>/ -run TestName -count=1 -v`.
- `gofmt -l internal/` must stay clean except the two pre-existing offenders (`internal/mcp/handlers_feedback_test.go`, `internal/memory/compact.go`).
- Commit after every task; end commit messages with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

---

### Task 1: CLI `graph refs`

**Files:**
- Modify: `internal/cli/graph.go`
- Test: `internal/cli/graph_test.go`

- [ ] **Step 1: Write the failing test** — append to `internal/cli/graph_test.go`:

```go
func TestGraphRefs(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	writeRepoFile(t, wt, "b.go", "package p\nfunc Caller() { Target() }\n")
	writeRepoFile(t, wt, "a_test.go", "package p\nfunc helper() { Target() }\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"refs", "Target"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "b.go:2") || !strings.Contains(out, "calls") {
		t.Errorf("output missing the calls reference from b.go:2:\n%s", out)
	}
	if !strings.Contains(out, "[test]") {
		t.Errorf("output missing the [test] marker for a_test.go:\n%s", out)
	}
}

func TestGraphRefsUnknownSymbol(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Only() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"refs", "NoSuchSym"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "did not resolve") {
		t.Errorf("unknown symbol must say it did not resolve to a graph node:\n%s", buf.String())
	}
}
```

If `graph_test.go` has no `writeRepoFile` helper, add one (or reuse the package's existing file-writing helper — check `helpers_test.go` first; if one exists under another name, use that name in the tests above instead):

```go
func writeRepoFile(t *testing.T, wt, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run TestGraphRefs -count=1 -v`
Expected: FAIL — `graph: unknown subcommand "refs"`

- [ ] **Step 3: Implement** — in `internal/cli/graph.go`, add a case to the subcommand switch (after `case "impls", "implementations":`'s block):

```go
	case "refs", "references":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		refLimit := 50
		var sym []string
		for _, tok := range rest {
			switch {
			case strings.HasPrefix(tok, "--limit="):
				refLimit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			case strings.HasPrefix(tok, "-"):
				continue // skip stray flags, don't fold them into the symbol
			default:
				sym = append(sym, tok)
			}
		}
		if refLimit <= 0 {
			refLimit = 50
		}
		refs, meta, err := a.Graph.References(ctx, wt, strings.Join(sym, " "), refLimit)
		if err != nil {
			return err
		}
		if !meta.Resolved {
			fmt.Fprintf(w, "(%q did not resolve to a graph node — hits below are text-only)\n", strings.Join(sym, " "))
		}
		if len(refs) == 0 {
			fmt.Fprintln(w, "(no references)")
			return nil
		}
		for _, r := range refs {
			line := fmt.Sprintf("%s:%d  [%s]", r.Path, r.Line, r.Kind)
			if r.From != "" {
				line += "  from " + r.From
			}
			if r.IsTest {
				line += "  [test]"
			}
			if r.Kind == "text" && r.Text != "" {
				line += "  " + r.Text
			}
			fmt.Fprintln(w, line)
		}
		if meta.Truncated {
			fmt.Fprintf(w, "… truncated at %d; raise --limit=\n", refLimit)
		}
		return nil
```

- [ ] **Step 4: Update the usage block** in `graphUsage`: change the subcommand list error string at the top of `Graph` from `(search|grep|callers|callees|impls|annotated|outline|arch|query|snippet)` to `(search|grep|refs|callers|callees|impls|annotated|outline|arch|query|snippet)`, add after the `impls` usage line:

```
  refs <symbol> [--limit=<n>]       every place a symbol is used: call sites, type usages,
                                    imports (graph edges) + a word-boundary text sweep;
                                    [test] marks hits in test code
```

and REPLACE the stale trailing Tip:

```
Tip: for precise references to a type, use ` + "`grep -r '\\bName\\b'`" + ` (word boundaries)
rather than a bare substring, which also matches NameFactory, GetName, etc.
```

with:

```
Tip: for references to a type/symbol, ` + "`refs <Name>`" + ` is the structural answer;
` + "`grep -r '\\bName\\b'`" + ` is the raw-text fallback.
```

- [ ] **Step 5: Run the tests** — `go test ./internal/cli/ -run TestGraphRefs -count=1 -v` → PASS, then `go test ./internal/cli/ -count=1` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/graph.go internal/cli/graph_test.go
git commit -m "feat(cli): graph refs — find_references parity with [test] markers"
```

---

### Task 2: CLI grep `--offset`, snippet path header, `[test]` + refs steer on callers/callees

**Files:**
- Modify: `internal/cli/graph.go`
- Test: `internal/cli/graph_test.go`

- [ ] **Step 1: Write the failing tests** — append to `internal/cli/graph_test.go`:

```go
func TestGraphGrepOffset(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "t.go", "package p\n// needle one\n// needle two\nfunc F() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"grep", "needle", "--offset=1", "--limit=1"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "t.go:3") || strings.Contains(out, "t.go:2") {
		t.Errorf("offset=1 must return only the second match (t.go:3):\n%s", out)
	}
}

func TestGraphSnippetPrintsPathHeader(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"snippet", "Target"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "// a.go:2-2\n") {
		t.Errorf("snippet must lead with its // path:start-end header:\n%s", buf.String())
	}
}

func TestGraphCallersMarksTestCallers(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	writeRepoFile(t, wt, "a_test.go", "package p\nfunc helperT() { Target() }\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"callers", "Target"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "helperT [test]") && !strings.Contains(out, "helperT [test·") {
		// label order: name, then [test], then [resolution·confidence] — assert leniently on "helperT" + "[test]"
		if !(strings.Contains(out, "helperT") && strings.Contains(out, "[test]")) {
			t.Errorf("test caller must carry a [test] marker:\n%s", out)
		}
	}
	if !strings.Contains(out, "every caller shown is test code") {
		t.Errorf("all-test callers must print the steer note:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run 'TestGraphGrepOffset|TestGraphSnippetPrintsPathHeader|TestGraphCallersMarksTestCallers' -count=1 -v` → all three FAIL.

- [ ] **Step 3: Implement grep `--offset`** — in the `case "grep", "content":` flag loop add:

```go
			case strings.HasPrefix(tok, "--offset="):
				opts.Offset, _ = strconv.Atoi(strings.TrimPrefix(tok, "--offset="))
```

and in `graphUsage`'s grep block add (after `--max-per-file=<n>`):

```
      --offset=<n>                  skip the first n matches (stable order; page with --offset/--limit)
```

Also extend the grep truncation note: after `note += "; raise --limit= or narrow with --glob=/--lang="` change to `note += "; page with --offset=/--limit= or narrow with --glob=/--lang="` and keep the per-file-capped clause as-is (per-file-capped matches need `--max-per-file=`, not offset — the existing clause already says that).

- [ ] **Step 4: Implement the snippet header** — in `case "snippet":` print a header before the source in BOTH branches:

single-arg branch, replace:

```go
			if res.Found {
				fmt.Fprintln(w, res.Snippet)
				return nil
			}
```

with:

```go
			if res.Found {
				fmt.Fprintf(w, "// %s:%d-%d\n", res.Path, res.StartLine, res.EndLine)
				fmt.Fprintln(w, res.Snippet)
				return nil
			}
```

two-arg branch, replace:

```go
			if !res.Found {
				return fmt.Errorf("graph snippet: symbol %q not found in %s", rest[1], rest[0])
			}
			fmt.Fprintln(w, res.Snippet)
			return nil
```

with:

```go
			if !res.Found {
				return fmt.Errorf("graph snippet: symbol %q not found in %s", rest[1], rest[0])
			}
			fmt.Fprintf(w, "// %s:%d-%d\n", res.Path, res.StartLine, res.EndLine)
			fmt.Fprintln(w, res.Snippet)
			return nil
```

(`res.Path`/`StartLine`/`EndLine` exist since round 1.) Update the snippet usage line tail to mention the header: change `overloads (one name, one file) print together` to `prints a // path:start-end header; overloads (one name, one file) print together`.

- [ ] **Step 5: Implement `[test]` markers + all-test steer on callers/callees** — in the `case "callers", "callees":` chain-label loop, after the `if n.Receiver != "" { ... }` block add:

```go
				if graph.IsTestPath(n.Path) {
					label += " [test]"
				}
```

Track all-test callers around the existing lines loop. Before the `lines :=` declaration add `anyCaller, allTest := false, true`; inside the chain loop (per chain, before rendering parts), for callers only:

```go
			if dir == "callers" && len(ch) >= 2 {
				anyCaller = true
				// ch is engine order: queried symbol first, callers after — every hop
				// past the root is a caller.
				for _, n := range ch[1:] {
					if !graph.IsTestPath(n.Path) {
						allTest = false
					}
				}
			}
```

and after the final `for _, line := range lines { ... }` print loop, before `return nil`:

```go
			if dir == "callers" && anyCaller && allTest {
				fmt.Fprintf(w, "note: every caller shown is test code — production may reach %s through a wrapper or sibling API; try `graph refs %s` for non-test references.\n", leafName(sym), leafName(sym))
			}
```

Also update the callers dead-end steer (the `case dir == "callers":` inside the root-only block) from the `graph grep -r '\b%s\b'` suggestion to:

```go
				fmt.Fprintf(w, "it may be an entrypoint, or callers bind dynamically and weren't statically resolved; `graph refs %s` finds the references the call edges miss.\n", leafName(sym))
```

and the type dead-end line: change `` `graph grep -r '\\b%s\\b'` (references) `` to `` `graph refs %s` (references) `` (keep the other suggestions).

- [ ] **Step 6: Run** — the three new tests PASS; whole package `go test ./internal/cli/ -count=1` PASS (update any existing test that pinned the old dead-end text — report which).

- [ ] **Step 7: Commit**

```bash
git add internal/cli/graph.go internal/cli/graph_test.go
git commit -m "feat(cli): grep --offset, snippet path header, [test] markers + refs steers on traces"
```

---

### Task 3: Migration v19 — `graph_deltas` table

**Files:**
- Modify: `internal/store/migrate.go`
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the failing test** — in `internal/store/migrate_test.go`, rename `TestSchemaVersionIs18` → `TestSchemaVersionIs19` and update its pinned values to `19` (both `schemaVersion` and `len(migrations)` — keep the test's existing structure). Append:

```go
func TestV19CreatesGraphDeltas(t *testing.T) {
	st := newTestStore(t) // use this file's existing store fixture; check its actual name first
	ctx := context.Background()
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('d1', '/wt', 'a.go', 'Target', 'function', 'added', 1)`); err != nil {
		t.Fatalf("graph_deltas not usable after migration: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('d2', '/wt', 'a.go', 'Target', 'function', 'renamed', 1)`); err == nil {
		t.Fatal("CHECK constraint must reject unknown change values")
	}
}
```

(Adapt the fixture call to whatever this test file already uses to open a migrated store — read the file's existing tests first and copy their opening pattern exactly.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/store/ -run 'TestSchemaVersionIs19|TestV19CreatesGraphDeltas' -count=1 -v` → FAIL (`no such table: graph_deltas`, version mismatch).

- [ ] **Step 3: Implement** — append to the `migrations` slice in `internal/store/migrate.go`:

```go
	// v19: graph_deltas journals symbol-level index changes (added/removed/changed
	// per file) so a session can show "what changed since you were last here"
	// without snapshots or re-parsing. Rows are written by writeFileGraph/deleteFile
	// at reindex time and pruned by session age; the table starts empty and is NOT
	// derived state worth wiping elsewhere. Memories are untouched. See the v5
	// comment for the standing convention.
	`
CREATE TABLE graph_deltas (
  id             TEXT PRIMARY KEY,
  worktree       TEXT NOT NULL,
  path           TEXT NOT NULL,
  qualified_name TEXT NOT NULL,
  kind           TEXT NOT NULL,
  change         TEXT NOT NULL CHECK (change IN ('added','removed','changed')),
  at             INTEGER NOT NULL
);
CREATE INDEX idx_graph_deltas_wt_at ON graph_deltas(worktree, at);
`,
```

and bump `const schemaVersion = 18` → `19`.

- [ ] **Step 4: Run the store suite** — `go test ./internal/store/ -count=1` → PASS. Note: `TestLatestGraphMigrationRebuildsIndex` seeds at user_version=10 and asserts the graph tables END empty — v19 creates a new empty table and repopulates nothing, so it must still pass; if it instead pins "the LAST migration wipes the graph", follow its own comment's convention and adjust it to target the latest wipe migration (v18), reporting the change.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): v19 graph_deltas journal table"
```

---

### Task 4: Engine — journal deltas at the write seam; `DeltasSince` / `DeltaBaseline` / `PruneDeltas`

**Files:**
- Create: `internal/graph/delta.go`
- Create: `internal/graph/delta_test.go`
- Modify: `internal/graph/index.go` (`writeFileGraph`)
- Modify: `internal/graph/resolve.go` (`deleteFile`)

- [ ] **Step 1: Write the failing tests** — create `internal/graph/delta_test.go`:

```go
package graph

import (
	"context"
	"testing"
)

func TestDeltaJournalAddedChangedRemoved(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	// First index of the file journals NOTHING (no old set — fresh clones and
	// wipe migrations must not flood the journal).
	indexInto(t, e, wt, "a.go", "package p\nfunc Keep() {}\nfunc Drop() {}\n")
	d, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(d) != 0 {
		t.Fatalf("first index journaled %d deltas, want 0: %+v", total, d)
	}

	// Reindex: Keep changes body, Drop disappears, Add appears.
	indexInto(t, e, wt, "a.go", "package p\nfunc Keep() { _ = 1 }\nfunc Add() {}\n")
	d, total, err = e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Keep": "changed", "Drop": "removed", "Add": "added"}
	if total != len(want) {
		t.Fatalf("total = %d, want %d: %+v", total, len(want), d)
	}
	for _, dd := range d {
		if want[dd.QualifiedName] != dd.Change {
			t.Errorf("%s = %s, want %s", dd.QualifiedName, dd.Change, want[dd.QualifiedName])
		}
		if dd.Path != "a.go" {
			t.Errorf("path = %q, want a.go", dd.Path)
		}
	}
}

func TestDeltaFoldLatestDisposition(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\n")

	// added then changed → reported once as added (it did not exist at baseline).
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\n")
	// added then removed → nets out to nothing.
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\nfunc C() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\nfunc B() { _ = 1 }\n")

	d, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, dd := range d {
		got[dd.QualifiedName] = dd.Change
	}
	if got["B"] != "added" {
		t.Errorf("B = %q, want added (added+changed folds to added)", got["B"])
	}
	if _, ok := got["C"]; ok {
		t.Error("C must net out (added then removed within the window)")
	}
	if total != len(d) {
		t.Errorf("total %d != len %d after folding", total, len(d))
	}
}

func TestDeltaJournalOnFileDelete(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "gone.go", "package p\nfunc Bye() {}\n")
	if err := e.deleteFile(ctx, wt, "gone.go"); err != nil {
		t.Fatal(err)
	}
	d, _, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, dd := range d {
		if dd.QualifiedName == "Bye" && dd.Change == "removed" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("deleted file's symbols must journal as removed: %+v", d)
	}
}

func TestPruneDeltas(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() {}\n")
	indexInto(t, e, wt, "a.go", "package p\nfunc A() { _ = 1 }\n") // journals 'changed'
	if err := e.PruneDeltas(ctx, wt, e.clk.Now().UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
	_, total, err := e.DeltasSince(ctx, wt, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("prune left %d deltas, want 0", total)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/graph/ -run TestDelta -count=1 -v` → FAIL (`e.DeltasSince undefined`).

- [ ] **Step 3: Implement** — create `internal/graph/delta.go`:

```go
package graph

import (
	"context"
	"fmt"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Delta is one symbol-level index change: a symbol that was added, removed, or
// changed (span/signature) since some point in time, as journaled at reindex.
type Delta struct {
	Path          string
	QualifiedName string
	Kind          string
	Change        string // added | removed | changed
}

// journalFileDeltas diffs a file's stored node set against the replacement and
// inserts one graph_deltas row per difference, on the caller's transaction.
// Identity is (qualified_name, kind); a span or signature difference is
// "changed". Called only when the file already had rows (first index of a file
// journals nothing — fresh clones and wipe-migration rebuilds must not flood
// the journal).
func journalFileDeltas(ctx context.Context, c store.Conn, worktree, path, fileID string,
	newNodes []nodeRow, now int64) error {
	type sig struct {
		startByte, endByte int64
		signature          string
	}
	old := map[string]sig{}
	rows, err := c.QueryContext(ctx,
		`SELECT qualified_name, kind, start_byte, end_byte, signature FROM nodes WHERE file_id = ?`, fileID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var qn, kind, sg string
		var sb, eb int64
		if err := rows.Scan(&qn, &kind, &sb, &eb, &sg); err != nil {
			rows.Close()
			return err
		}
		old[qn+"\x00"+kind] = sig{sb, eb, sg}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	emit := func(qn, kind, change string) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
			 VALUES (?,?,?,?,?,?,?)`, ids.New(), worktree, path, qn, kind, change, now)
		return err
	}

	seen := map[string]bool{}
	for _, n := range newNodes {
		key := n.qn + "\x00" + n.kind
		seen[key] = true
		o, ok := old[key]
		switch {
		case !ok:
			if err := emit(n.qn, n.kind, "added"); err != nil {
				return err
			}
		case o.startByte != n.startByte || o.endByte != n.endByte || o.signature != n.signature:
			if err := emit(n.qn, n.kind, "changed"); err != nil {
				return err
			}
		}
	}
	for key := range old {
		if seen[key] {
			continue
		}
		qn, kind := splitDeltaKey(key)
		if err := emit(qn, kind, "removed"); err != nil {
			return err
		}
	}
	return nil
}

// splitDeltaKey undoes the qn+"\x00"+kind composite.
func splitDeltaKey(key string) (qn, kind string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// DeltasSince returns the folded symbol-level changes journaled after sinceMs:
// one entry per (path, qualified_name, kind), where a symbol added within the
// window stays "added" through later changes, and one added AND removed within
// the window nets out entirely — the answer reads as a disposition relative to
// the baseline, not a raw event log. total is the folded count before limit.
func (e *Engine) DeltasSince(ctx context.Context, worktree string, sinceMs int64, limit int) ([]Delta, int, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, qualified_name, kind, change FROM graph_deltas
		  WHERE worktree = ? AND at >= ? ORDER BY at, path, qualified_name`,
		worktree, sinceMs)
	if err != nil {
		return nil, 0, err
	}
	type state struct {
		addedInWindow bool
		last          string
	}
	folded := map[string]*state{}
	var order []string
	for rows.Next() {
		var p, qn, kind, ch string
		if err := rows.Scan(&p, &qn, &kind, &ch); err != nil {
			rows.Close()
			return nil, 0, err
		}
		key := p + "\x00" + qn + "\x00" + kind
		st, ok := folded[key]
		if !ok {
			st = &state{}
			folded[key] = st
			order = append(order, key)
		}
		if ch == "added" {
			st.addedInWindow = true
		}
		st.last = ch
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var out []Delta
	for _, key := range order {
		st := folded[key]
		var change string
		switch {
		case st.addedInWindow && st.last == "removed":
			continue // net nothing: born and gone within the window
		case st.addedInWindow:
			change = "added"
		default:
			change = st.last
		}
		var p, rest string
		p, rest = splitDeltaKey(key)
		qn, kind := splitDeltaKey(rest)
		out = append(out, Delta{Path: p, QualifiedName: qn, Kind: kind, Change: change})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].QualifiedName < out[j].QualifiedName
	})
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// DeltaBaseline returns the started_at of the most recent session (for this
// worktree) that is older than the latest session AND whose window journaled at
// least one delta — the last session that did real work. /clear and
// open-and-close sessions journal nothing and are skipped, so the same delta
// keeps showing until a context has genuinely lived alongside the changes.
// ok=false when no such session exists (fresh repo, or no deltas ever).
//
// Sessions are the agents domain, but the walk needs the deltas table too and
// both MCP and the session-start hook ask this exact question — one home in the
// engine beats two copies of cross-table SQL.
func (e *Engine) DeltaBaseline(ctx context.Context, worktree string, nowMs int64) (int64, bool, error) {
	var latest int64
	err := e.st.DB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(started_at), 0) FROM sessions WHERE worktree = ?`, worktree).Scan(&latest)
	if err != nil {
		return 0, false, err
	}
	if latest == 0 {
		return 0, false, nil
	}
	var baseline int64
	err = e.st.DB.QueryRowContext(ctx, `
SELECT s.started_at FROM sessions s
 WHERE s.worktree = ? AND s.started_at < ?
   AND EXISTS (SELECT 1 FROM graph_deltas d
                WHERE d.worktree = s.worktree
                  AND d.at >= s.started_at AND d.at <= COALESCE(s.ended_at, ?))
 ORDER BY s.started_at DESC LIMIT 1`, worktree, latest, nowMs).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return baseline, true, nil
}

// PruneDeltas drops journal rows older than beforeMs for the worktree.
func (e *Engine) PruneDeltas(ctx context.Context, worktree string, beforeMs int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`DELETE FROM graph_deltas WHERE worktree = ? AND at < ?`, worktree, beforeMs)
		return err
	})
}
```

Imports for the file as assembled: `context`, `database/sql`, `errors`, `sort`, plus the repo's `ids` and `store` packages (drop `fmt` — nothing above uses it).

- [ ] **Step 4: Hook the journal into the write seam** — in `internal/graph/index.go` `writeFileGraph`, the `default:` branch (file row exists) currently starts with the FTS deletes. Insert the journal call FIRST in that branch, before any delete:

```go
	default:
		// Journal symbol-level deltas before the old rows are replaced — this is
		// the only moment both node sets exist. New files (ErrNoRows above) are
		// first-index and journal nothing.
		if err := journalFileDeltas(ctx, c, worktree, path, fileID, g.nodes, now); err != nil {
			return err
		}
```

In `internal/graph/resolve.go` `deleteFile`, inside the WriteTx before the FTS deletes, add:

```go
		// A vanished file's symbols journal as removed (same identity rules as
		// writeFileGraph; the file's id row still exists at this point).
		if _, err := c.ExecContext(ctx, `
INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
SELECT lower(hex(randomblob(16))), ?, ?, n.qualified_name, n.kind, 'removed', ?
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ?`,
			worktree, path, e.clk.Now().UnixMilli(), worktree, path); err != nil {
			return err
		}
```

(If `ids.New()` is preferred over `randomblob` for id-format consistency, select the nodes first and insert per-row with `ids.New()` — match what `ids.New()` produces; check one existing id and pick the consistent option, reporting which.)

- [ ] **Step 5: Run** — `go test ./internal/graph/ -run TestDelta -count=1 -v` → PASS; `go test ./internal/graph/ -count=1` → PASS (the journal must not break reconcile/index tests).

- [ ] **Step 6: Commit**

```bash
git add internal/graph/delta.go internal/graph/delta_test.go internal/graph/index.go internal/graph/resolve.go
git commit -m "feat(graph): journal symbol-level deltas at reindex; DeltasSince/DeltaBaseline/PruneDeltas"
```

---

### Task 5: Session-start delta tier (+ prune)

**Files:**
- Modify: `internal/agents/format.go`
- Modify: `internal/agents/handle.go` (`handleSessionStart`)
- Test: `internal/agents/format_test.go`, `internal/agents/handle_test.go`

- [ ] **Step 1: Write the failing format test** — append to `internal/agents/format_test.go`:

```go
func TestFormatDeltaTier(t *testing.T) {
	deltas := []graph.Delta{
		{Path: "internal/mcp/graph.go", QualifiedName: "findReferences", Kind: "function", Change: "added"},
		{Path: "internal/mcp/graph.go", QualifiedName: "getCodeSnippet", Kind: "function", Change: "changed"},
		{Path: "internal/parse/parse.go", QualifiedName: "ParseFile", Kind: "function", Change: "changed"},
	}
	out := formatDeltaTier(deltas, 3, 1749340800000) // 2025-06-08 (any fixed ms)
	if !strings.Contains(out, "Changed since your last working session") {
		t.Errorf("missing tier header: %q", out)
	}
	if !strings.Contains(out, "internal/mcp: +1 ~1") {
		t.Errorf("missing per-module counts: %q", out)
	}
	if !strings.Contains(out, "findReferences") {
		t.Errorf("heaviest module should name symbols: %q", out)
	}
	if !strings.Contains(out, "get_graph_delta") {
		t.Errorf("must point at the drill-down tool: %q", out)
	}
	if got := ledger.EstimateTokens(out); got > deltaTierBudget {
		t.Errorf("tier costs %d tokens, budget %d", got, deltaTierBudget)
	}
	if formatDeltaTier(nil, 0, 0) != "" {
		t.Error("empty delta must render nothing at all")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/agents/ -run TestFormatDeltaTier -count=1 -v` → FAIL (`undefined: formatDeltaTier`).

- [ ] **Step 3: Implement the tier** — in `internal/agents/format.go`:

Bump the reserve and document it:

```go
	cerebrumReserve    = 384 // headroom for the map (~165 tok) + recent-bugs (~60 tok) + delta (~150 tok) tiers
```

Add:

```go
// deltaTierBudget caps the session-start "what changed" tier. The full list is
// one get_graph_delta call away, so the tier only buys orientation.
const deltaTierBudget = 150

// formatDeltaTier renders the budgeted "changed since your last working
// session" block: per-module +added ~changed -removed counts (module = first
// path segment, matching how the code map names top-level modules), symbol
// names for the heaviest module only, and the drill-down pointer. Empty input
// renders nothing — a no-op session must not produce an empty list.
func formatDeltaTier(deltas []graph.Delta, total int, baselineMs int64) string {
	if len(deltas) == 0 {
		return ""
	}
	type agg struct {
		added, changed, removed int
		names                   []string
	}
	mods := map[string]*agg{}
	var order []string
	firstSeg := func(p string) string {
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				return p[:i]
			}
		}
		return p
	}
	for _, d := range deltas {
		m := firstSeg(d.Path)
		a, ok := mods[m]
		if !ok {
			a = &agg{}
			mods[m] = a
			order = append(order, m)
		}
		switch d.Change {
		case "added":
			a.added++
			a.names = append(a.names, leafName(d.QualifiedName))
		case "removed":
			a.removed++
		default:
			a.changed++
			a.names = append(a.names, leafName(d.QualifiedName))
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		ai, aj := mods[order[i]], mods[order[j]]
		return ai.added+ai.changed+ai.removed > aj.added+aj.changed+aj.removed
	})

	var b strings.Builder
	fmt.Fprintf(&b, "\nChanged since your last working session here (%s):\n",
		time.UnixMilli(baselineMs).Format("Jan 2"))
	const maxModLines = 5
	for i, m := range order {
		if i >= maxModLines {
			fmt.Fprintf(&b, "- … %d more modules\n", len(order)-maxModLines)
			break
		}
		a := mods[m]
		line := "- " + m + ":"
		if a.added > 0 {
			line += fmt.Sprintf(" +%d", a.added)
		}
		if a.changed > 0 {
			line += fmt.Sprintf(" ~%d", a.changed)
		}
		if a.removed > 0 {
			line += fmt.Sprintf(" -%d", a.removed)
		}
		if i == 0 && len(a.names) > 0 {
			n := a.names
			if len(n) > 3 {
				n = n[:3]
			}
			line += " (" + strings.Join(n, ", ") + ", …)"
			if len(a.names) <= 3 {
				line = strings.Replace(line, ", …)", ")", 1)
			}
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "(get_graph_delta lists all %d)\n", total)
	out := b.String()
	// Degrade under budget: drop the symbol names first, then the module tail.
	if ledger.EstimateTokens(out) > deltaTierBudget {
		if i := strings.Index(out, " ("); i >= 0 {
			if j := strings.Index(out[i:], ")"); j >= 0 {
				out = out[:i] + out[i+j+1:]
			}
		}
	}
	return out
}
```

(`leafName` — check whether the agents package already has one; if not, add the same five-line helper used in `internal/cli/graph.go:479`, unexported. `time`, `sort` imports as needed.)

Wire it into `formatSessionContext`: change the signature to

```go
func formatSessionContext(results []memory.RecallResult, arch graph.Architecture, bugs []*memory.Record,
	deltas []graph.Delta, deltaTotal int, deltaBaselineMs int64, budget int) (string, []string) {
```

and immediately after the code-map block (before the bugs block) add:

```go
	if tier := formatDeltaTier(deltas, deltaTotal, deltaBaselineMs); tier != "" {
		b.WriteString(tier)
	}
```

Update every existing `formatSessionContext` caller/test with `nil, 0, 0` for the new params (the compiler lists them).

- [ ] **Step 4: Write the failing session-start test** — append to `internal/agents/handle_test.go` (copy the file's existing session-start fixture pattern — it builds an app, fakes a session payload, and inspects `Result.Context`):

```go
func TestSessionStartShowsDeltaSinceLastWorkingSession(t *testing.T) {
	a := newTestApp(t, "main") // use this package's existing app fixture name
	ctx := context.Background()
	wt := a.Git.Worktree

	// Session 1 does real work: index a file, then re-index it changed.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	writeWorktreeFile(t, wt, "w.go", "package p\nfunc Work() {}\n")
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	writeWorktreeFile(t, wt, "w.go", "package p\nfunc Work() { _ = 1 }\nfunc Extra() {}\n")
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Session 2 is a no-op (/clear): no deltas in its window.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s2", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}

	// Session 3 must still show session 1's changes (baseline skips the no-op).
	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s3", Agent: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Changed since your last working session") ||
		!strings.Contains(res.Context, "Extra") && !strings.Contains(res.Context, "~") {
		t.Errorf("session 3 context missing the delta tier:\n%s", res.Context)
	}
}
```

(Adapt fixture/helper names to what `handle_test.go` actually uses — read its existing `handleSessionStart` tests first; `writeWorktreeFile` may be `writeFile`-style. Timing note: each `Handle(SessionStart)` stamps `started_at = clock now`; if the package uses a fake clock, advance it between sessions so windows don't collapse to one instant — follow the fixture's clock idiom.)

- [ ] **Step 5: Implement the wiring** — in `internal/agents/handle.go` `handleSessionStart`, after the `bugs, _ :=` line and before `formatSessionContext`:

```go
	// "What changed since you were last here": baseline = the last session that
	// journaled deltas, so /clear and open-and-close sessions are transparent
	// (the same delta keeps showing until a context has lived alongside it).
	var deltas []graph.Delta
	var deltaTotal int
	var deltaBaseline int64
	if at, ok, err := a.Graph.DeltaBaseline(ctx, a.Git.Worktree, now); err == nil && ok {
		deltaBaseline = at
		deltas, deltaTotal, _ = a.Graph.DeltasSince(ctx, a.Git.Worktree, at, 200)
	}
	// Retention: journal rows older than the 10th-most-recent session are no
	// longer reachable by any baseline walk — drop them (best-effort).
	if cutoff, err := tenthSessionStart(ctx, a); err == nil && cutoff > 0 {
		_ = a.Graph.PruneDeltas(ctx, a.Git.Worktree, cutoff)
	}
```

update the call: `out, emitted := formatSessionContext(results, arch, bugs, deltas, deltaTotal, deltaBaseline, budget)`, and add the helper at file scope:

```go
// tenthSessionStart returns the started_at of the worktree's 10th-most-recent
// session, or 0 when fewer exist. Older graph_deltas rows are unreachable by
// any DeltaBaseline walk and can be pruned.
func tenthSessionStart(ctx context.Context, a *app.App) (int64, error) {
	var at int64
	err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE worktree = ?
		  ORDER BY started_at DESC LIMIT 1 OFFSET 9`, a.Git.Worktree).Scan(&at)
	if err != nil {
		return 0, nil // fewer than 10 sessions: nothing to prune (sql.ErrNoRows folds here)
	}
	return at, nil
}
```

- [ ] **Step 6: Run** — both new tests PASS; `go test ./internal/agents/ -count=1` PASS (fix `formatSessionContext` call sites the compiler flags).

- [ ] **Step 7: Commit**

```bash
git add internal/agents/format.go internal/agents/format_test.go internal/agents/handle.go internal/agents/handle_test.go
git commit -m "feat(agents): budgeted graph-delta tier at session start (baseline skips no-op sessions)"
```

---

### Task 6: MCP `get_graph_delta`

**Files:**
- Modify: `internal/mcp/graph.go` (append handler + types)
- Modify: `internal/mcp/server.go` (register)
- Test: `internal/mcp/graph_test.go`

- [ ] **Step 1: Write the failing test** — append to `internal/mcp/graph_test.go`:

```go
func TestGetGraphDeltaHandler(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	ctx := context.Background()

	// A prior session whose window contains deltas, then a later session.
	now := a.Clock.Now().UnixMilli()
	if _, err := a.Store.DB.ExecContext(ctx,
		`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at) VALUES
		 ('prior','claude-code','main',?,?,?), ('current','claude-code','main',?,?,NULL)`,
		wt, now-10_000, now-5_000, wt, now-1_000); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc A() {}\n"), 0o644)
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	// Reindex with a change whose journal row lands inside the prior session's
	// window by inserting it directly (unit seam: the journal is engine-tested).
	if _, err := a.Store.DB.ExecContext(ctx,
		`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
		 VALUES ('gd1', ?, 'a.go', 'A', 'function', 'changed', ?)`, wt, now-7_000); err != nil {
		t.Fatal(err)
	}

	out, err := getGraphDelta(ctx, a, GetGraphDeltaInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.BaselineAt == "" || out.Count != 1 || out.Deltas[0].QualifiedName != "A" || out.Deltas[0].Change != "changed" {
		t.Fatalf("got %+v, want one 'changed' delta for A with a baseline", out)
	}
}

func TestGetGraphDeltaNoBaseline(t *testing.T) {
	a := newTestApp(t, "main")
	a.Git.Worktree = t.TempDir()
	out, err := getGraphDelta(context.Background(), a, GetGraphDeltaInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 || out.BaselineAt != "" || out.Note == "" {
		t.Fatalf("no prior working session must return an explanatory note, got %+v", out)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `undefined: getGraphDelta`.

- [ ] **Step 3: Implement** — append to `internal/mcp/graph.go`:

```go
// --- get_graph_delta ---

type GetGraphDeltaInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"max symbol-level changes to return (default 50)"`
}
type GraphDeltaEntry struct {
	Path          string `json:"path"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Change        string `json:"change"` // added | removed | changed
}
type GetGraphDeltaOutput struct {
	// BaselineAt is the start of the last session that changed the graph — the
	// same baseline the session-start tier uses. Empty when no prior working
	// session exists (see Note).
	BaselineAt string            `json:"baseline_at,omitempty"`
	Count      int               `json:"count"`
	Total      int               `json:"total"`
	Truncated  bool              `json:"truncated,omitempty"`
	Deltas     []GraphDeltaEntry `json:"deltas"`
	Note       string            `json:"note,omitempty"`
	Hint       string            `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func getGraphDelta(ctx context.Context, a *app.App, in GetGraphDeltaInput) (GetGraphDeltaOutput, error) {
	fresh := ensureFresh(ctx, a)
	now := a.Clock.Now().UnixMilli()
	at, ok, err := a.Graph.DeltaBaseline(ctx, a.Git.Worktree, now)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	if !ok {
		return GetGraphDeltaOutput{
			Note: "no prior working session has changed the code graph here — nothing to compare against.",
			Hint: fresh, Deltas: []GraphDeltaEntry{},
		}, nil
	}
	deltas, total, err := a.Graph.DeltasSince(ctx, a.Git.Worktree, at, in.Limit)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	out := GetGraphDeltaOutput{
		BaselineAt: time.UnixMilli(at).Format(time.RFC3339),
		Count:      len(deltas), Total: total, Truncated: total > len(deltas),
		Deltas: make([]GraphDeltaEntry, 0, len(deltas)), Hint: fresh,
	}
	for _, d := range deltas {
		out.Deltas = append(out.Deltas, GraphDeltaEntry{
			Path: d.Path, QualifiedName: d.QualifiedName, Kind: d.Kind, Change: d.Change,
		})
	}
	return out, nil
}
```

(add `"time"` to the file's imports if absent — post-merge it already imports it for `freshWait`.)

- [ ] **Step 4: Register** in `internal/mcp/server.go` after the `find_references` registration:

```go
	addTool(s, a, "get_graph_delta",
		"List what changed in the code graph since your last working session here — the symbol-level detail behind the session-start 'Changed since…' block (same baseline: the last session that actually changed the graph, so /clear or idle sessions don't reset it). Each entry is a symbol with its path, kind, and disposition relative to that baseline: added, removed, or changed; a symbol both added and removed within the window nets out. Use it to reorient on a branch before trusting recalled knowledge about code that may have moved. total/truncated report completeness; raise limit for more.",
		getGraphDelta)
```

- [ ] **Step 5: Run** — both tests PASS; `go test ./internal/mcp/ -count=1` PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/graph.go internal/mcp/graph_test.go internal/mcp/server.go
git commit -m "feat(mcp): get_graph_delta — symbol-level changes since the last working session"
```

---

### Task 7: Session-end nudge — gated block-once

**Files:**
- Modify: `internal/agents/event.go` (`Result`)
- Modify: `internal/agents/claudecode/adapter.go` (`Encode`)
- Modify: `internal/agents/handle.go` (`handleStop`, `handlePostEdit`)
- Test: `internal/agents/claudecode/hooks_test.go`, `internal/agents/handle_test.go`

- [ ] **Step 1: Write the failing handler tests** — append to `internal/agents/handle_test.go`:

```go
func TestStopNudgesOnceWhenEditsButNoMemories(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns1", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = a.Ledger.Incr(ctx, "ns1", "edits", 1)
	}
	res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Block || !strings.Contains(res.Reason, "recorded nothing") {
		t.Fatalf("3 edits + 0 memories must block-once with the nudge, got %+v", res)
	}
	// Second stop: never nudge twice.
	res, err = Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block || res.Reason != "" {
		t.Fatalf("second stop must pass through, got %+v", res)
	}
}

func TestStopDoesNotNudgeBelowThresholdOrWithMemories(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	// Below threshold: 2 edits, no nudge.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns2", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns2", "edits", 2)
	if res, _ := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns2"}); res.Block {
		t.Fatalf("2 edits must not nudge, got %+v", res)
	}

	// At threshold but a memory was recorded: no nudge.
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns3", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns3", "edits", 3)
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: "learned something", Branch: a.Git.Branch,
		Worktree: a.Git.Worktree, Author: "test", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	if res, _ := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns3"}); res.Block {
		t.Fatalf("a recorded memory must satisfy the nudge, got %+v", res)
	}
}

func TestStopActiveLoopGuardPassesThrough(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "ns4", Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ledger.Incr(ctx, "ns4", "edits", 5)
	res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "ns4", StopActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block {
		t.Fatal("stop_hook_active must always pass through (loop guard)")
	}
}
```

NOTE: this test calls `Engine.Record` with the FOUR-value signature Task 8 introduces. Tasks run in order 1→10, but 7 precedes 8 — so in THIS task use the current three-value form (`if _, _, err := a.Engine.Record(...)`) and let Task 8's call-site sweep update it. Also: the memory-count gate compares `memories.created_at` to the session row's `started_at` using `a.Clock` — if the test app uses a fake/frozen clock, both stamps share one instant and `>=` holds; that is the intended semantics (recorded during-or-after session start).

- [ ] **Step 2: Run to verify failure** — `res.Block undefined`.

- [ ] **Step 3: Implement the neutral types** — in `internal/agents/event.go`, extend `Result`:

```go
// Result is the agent-neutral hook output. Context is injected as the host's
// "additional context"; SystemMessage is shown to the human. Block asks the
// host to refuse this Stop once and feed Reason back to the model as an
// instruction — the only Stop channel an LLM acts on. Adapters without a block
// channel ignore Block/Reason.
type Result struct {
	Context       string
	SystemMessage string
	Block         bool
	Reason        string
}
```

- [ ] **Step 4: Implement the gating** — replace `handleStop` in `internal/agents/handle.go`:

```go
// stopNudgeEditThreshold is the minimum number of post-edit events in a session
// before an empty memory log warrants the block-once capture nudge.
const stopNudgeEditThreshold = 3

// handleStop closes the session row and, at most once per session, blocks the
// stop with a capture nudge when the session edited code but recorded nothing.
// The nudge must reach the MODEL, not the human — a systemMessage is only shown
// to the user, so the only acting channel is a block-once with the reason fed
// back as an instruction. ev.StopActive is the host's loop guard: when Claude
// Code re-invokes Stop after our block, always pass through.
func handleStop(ctx context.Context, a *app.App, ev Event) (Result, error) {
	if ev.SessionID == "" {
		return Result{}, nil
	}
	now := a.Clock.Now().UnixMilli()
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
			now, ev.SessionID)
		return err
	}); err != nil {
		return Result{}, err
	}
	if ev.StopActive {
		return Result{}, nil
	}
	edits, err := a.Ledger.Get(ctx, ev.SessionID, "edits")
	if err != nil || edits < stopNudgeEditThreshold {
		return Result{}, nil
	}
	if nudged, err := a.Ledger.Get(ctx, ev.SessionID, "stop_nudged"); err != nil || nudged > 0 {
		return Result{}, nil
	}
	var startedAt int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE id = ?`, ev.SessionID).Scan(&startedAt); err != nil {
		return Result{}, nil // unknown session row: never block on bookkeeping gaps
	}
	var recorded int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE created_at >= ? AND (branch IS NULL OR branch = ?)`,
		startedAt, a.Git.Branch).Scan(&recorded); err != nil || recorded > 0 {
		return Result{}, nil
	}
	_ = a.Ledger.Incr(ctx, ev.SessionID, "stop_nudged", 1)
	return Result{Block: true, Reason: fmt.Sprintf(
		"recarto: you made %d edits this session and recorded nothing. Record learnings, preferences, or bug fixes now (record_memory / record_bug) — or finish if there's genuinely nothing to keep.",
		edits)}, nil
}
```

(Check the `memories` timestamp column name in `internal/store/migrate.go`'s v1 block before writing the query — the engine stamps it in `insert`; if it is not `created_at`, use the actual name.) The old unconditional `SystemMessage` return is gone.

- [ ] **Step 5: Count edits** — in `handlePostEdit`, right after the worktree-containment check (`rel` computed, not `..`-prefixed), add:

```go
	if ev.SessionID != "" {
		// Per-session edit counter; the Stop hook's capture nudge keys on it.
		_ = a.Ledger.Incr(ctx, ev.SessionID, "edits", 1)
	}
```

- [ ] **Step 6: Encode the block** — in `internal/agents/claudecode/adapter.go` `Encode`, replace the `KindStop` branch:

```go
	if kind == agents.KindStop {
		if res.Block {
			// decision:block makes Claude Code continue one turn with reason as the
			// instruction — the only Stop output the model acts on.
			return json.NewEncoder(out).Encode(map[string]any{
				"decision": "block", "reason": res.Reason,
			})
		}
		if res.SystemMessage != "" {
			return json.NewEncoder(out).Encode(map[string]any{"systemMessage": res.SystemMessage})
		}
		return nil // nothing to say: emit nothing
	}
```

- [ ] **Step 7: Update/extend adapter tests** — in `internal/agents/claudecode/hooks_test.go`: any existing Stop test asserting the old static `systemMessage` text now expects pass-through (no output) or sets up the nudge conditions; add one end-to-end encode test:

```go
func TestStopEncodesBlockOnce(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, "main") // this package's existing fixture; check its name
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"e1","hook_event_name":"SessionStart","source":"startup"}`)
	if err := SessionStart(ctx, a, in, &out); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = a.Ledger.Incr(ctx, "e1", "edits", 1)
	}
	out.Reset()
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"e1"}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"decision":"block"`) || !strings.Contains(out.String(), "recorded nothing") {
		t.Fatalf("first gated stop must emit decision:block, got %q", out.String())
	}
	out.Reset()
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"e1","stop_hook_active":true}`), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "block") {
		t.Fatalf("stop_hook_active follow-up must not block, got %q", out.String())
	}
}
```

(Report exactly which existing Stop-test expectations you updated.)

- [ ] **Step 8: Run** — `go test ./internal/agents/... -count=1` → PASS (also check `internal/agents/opencode` + `copilot` adapters compile untouched — `Result`'s new fields are additive).

- [ ] **Step 9: Commit**

```bash
git add internal/agents/event.go internal/agents/handle.go internal/agents/handle_test.go internal/agents/claudecode/adapter.go internal/agents/claudecode/hooks_test.go
git commit -m "feat(agents): gated block-once capture nudge on Stop (edits>=3, nothing recorded, never twice)"
```

---

### Task 8: Engine — conflict candidates on `Record`

**Files:**
- Modify: `internal/memory/engine.go`
- Test: `internal/memory/engine_test.go` (or the package's record-focused test file — check where `Record` tests live first)
- Modify (call sites): `internal/mcp/handlers.go` (×2), `internal/mcp/handlers_feedback.go`, `internal/cli/remember.go`, `internal/cli/bug.go`, plus the Task 7 test's `Record` call

- [ ] **Step 1: Write the failing tests** — append to the memory package's record test file:

```go
func TestRecordSurfacesMiddleBandConflictCandidates(t *testing.T) {
	e := newTestEngine(t) // this package's existing engine fixture; check its name
	ctx := context.Background()
	_, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindPreference, Title: "tabs", Body: "the user always wants tabs for indentation in Go files",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Similar topic, opposite claim: middle-band similarity, NOT a near-dup.
	_, reinforced, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindPreference, Title: "spaces", Body: "the user always wants spaces for indentation in Go files",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reinforced {
		t.Fatal("opposite claim must not auto-reinforce")
	}
	if len(conflicts) == 0 {
		t.Fatalf("middle-band similar memory must surface as a conflict candidate")
	}
	if conflicts[0].Snippet == "" || conflicts[0].ID == "" || conflicts[0].Reason == "" {
		t.Errorf("candidate must carry id/snippet/reason: %+v", conflicts[0])
	}
}

func TestRecordSurfacesSharedRefConflicts(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	ref := []Ref{{Type: RefFile, Value: "internal/auth/login.go"}}
	if _, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindDecision, Title: "sessions in redis", Body: "store auth sessions in redis",
		Refs: ref, Author: "t", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindDecision, Title: "jwt only", Body: "drop server-side state entirely and use signed jwt",
		Refs: ref, Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawRefReason bool
	for _, c := range conflicts {
		if c.Reason == "shares refs" {
			sawRefReason = true
		}
	}
	if !sawRefReason {
		t.Fatalf("same-kind memory sharing a file ref must surface, got %+v", conflicts)
	}
}

func TestRecordNoConflictsForFactualKindsOrReinforce(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "npe", Body: "nil deref in parser on empty file",
		Author: "t", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "panic", Body: "panic slice bounds in lexer on empty file input",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("bug kind must not produce conflict candidates, got %+v", conflicts)
	}

	// Exact restatement reinforces and returns no candidates.
	body := "the user always wants conventional commit messages on every commit in this repo"
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindPreference, Body: body, Author: "t", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	_, reinforced, conflicts, err := e.Record(ctx, NewMemory{Kind: KindPreference, Body: body, Author: "t", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if !reinforced || len(conflicts) != 0 {
		t.Fatalf("reinforce path must return no candidates, got reinforced=%v %+v", reinforced, conflicts)
	}
}
```

(Check the actual `Ref` field/const names — `RefFile` etc. — in `internal/memory/record.go` and the engine-fixture name before finalizing; keep assertion semantics identical. The middle-band test's two bodies share most shingles by construction; if Jaccard lands below the 0.25 floor on the real shingle function, lengthen the shared phrasing until it doesn't — the floor is the contract, the fixture serves it.)

- [ ] **Step 2: Run to verify failure** — assignment-count mismatch on `Record`.

- [ ] **Step 3: Implement** — in `internal/memory/engine.go`:

```go
// ConflictCandidate is an existing memory the new one may contradict or
// supersede: similar enough to be about the same thing (or anchored to the same
// code), not similar enough to auto-reinforce. Judgment belongs to the
// recording agent — Recarto surfaces, never decides.
type ConflictCandidate struct {
	ID      string
	Title   string
	Snippet string // body prefix, ~160 chars
	Reason  string // "similar wording" | "shares refs"
}

// conflictKinds are the judgment-bearing kinds where an opposite claim is
// costly if it lingers. Bugs and todos are factual/transient and excluded.
var conflictKinds = map[Kind]bool{
	KindPreference: true, KindDecision: true, KindDoNotRepeat: true, KindLearning: true,
}

const (
	// conflictJaccardFloor..dedupThreshold is the "same topic, different claim"
	// middle band: below the floor is unrelated, at the threshold it reinforces.
	conflictJaccardFloor  = 0.25
	maxConflictCandidates = 3
	conflictSnippetLen    = 160
)
```

Refactor `findNearDup` so the middle band isn't discarded. Change its tail (the confirm loop) to collect scores and return them:

```go
// findNearDup returns the id of an active same-kind memory whose body has
// Jaccard >= dedupThreshold with set (the reinforce target), plus the
// middle-band candidates (conflictJaccardFloor <= j < dedupThreshold) that are
// about the same topic without being the same statement — the contradiction
// suspects Record surfaces.
func (e *Engine) findNearDup(ctx context.Context, c store.Conn, kind Kind,
	bands [lshBands]uint64, set map[string]struct{}) (string, bool, []ConflictCandidate, error) {
```

keep the band-gathering loop as-is, then replace the confirm loop with:

```go
	var mid []ConflictCandidate
	for _, id := range candidates {
		var title, body string
		if err := c.QueryRowContext(ctx,
			`SELECT title, body FROM memories WHERE id = ?`, id).Scan(&title, &body); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return "", false, nil, err
		}
		j := jaccard(set, shingleSet(body))
		if j >= dedupThreshold {
			return id, true, nil, nil
		}
		if j >= conflictJaccardFloor {
			mid = append(mid, ConflictCandidate{
				ID: id, Title: title, Snippet: bodySnippet(body), Reason: "similar wording",
			})
		}
	}
	return "", false, mid, nil
```

with the helper:

```go
// bodySnippet trims a body to the candidate-snippet budget on a rune boundary.
func bodySnippet(body string) string {
	r := []rune(body)
	if len(r) <= conflictSnippetLen {
		return body
	}
	return string(r[:conflictSnippetLen]) + "…"
}
```

Add the shared-refs pass:

```go
// refConflicts returns active same-kind memories (excluding selfID) anchored to
// any of the new memory's file/symbol refs — the structural "same topic" signal
// when wording differs entirely.
func (e *Engine) refConflicts(ctx context.Context, c store.Conn, kind Kind, selfID string, refs []Ref) ([]ConflictCandidate, error) {
	var args []any
	var or []string
	for _, r := range refs {
		if r.Type != RefFile && r.Type != RefSymbol {
			continue
		}
		or = append(or, "(r.ref_type = ? AND r.ref_value = ?)")
		args = append(args, string(r.Type), r.Value)
	}
	if len(or) == 0 {
		return nil, nil
	}
	q := `SELECT DISTINCT m.id, m.title, m.body
	        FROM memories m JOIN memory_refs r ON r.memory_id = m.id
	       WHERE m.kind = ? AND m.status = 'active' AND m.id != ? AND (` + strings.Join(or, " OR ") + `)
	       LIMIT 6`
	rows, err := c.QueryContext(ctx, q, append([]any{string(kind), selfID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConflictCandidate
	for rows.Next() {
		var id, title, body string
		if err := rows.Scan(&id, &title, &body); err != nil {
			return nil, err
		}
		out = append(out, ConflictCandidate{ID: id, Title: title, Snippet: bodySnippet(body), Reason: "shares refs"})
	}
	return out, rows.Err()
}
```

(Check the actual `Ref.Type` constant names — `RefFile`/`RefSymbol` or similar — in record.go and the `memories.status`/`kind` column spellings against an existing query in this file.) Then rework `Record`'s transaction body and signature:

```go
func (e *Engine) Record(ctx context.Context, nm NewMemory) (id string, reinforced bool, conflicts []ConflictCandidate, err error) {
```

inside the WriteTx, after the near-dup branch:

```go
		candID, found, mid, ferr := e.findNearDup(ctx, c, nm.Kind, bands, set)
		if ferr != nil {
			return ferr
		}
		if found {
			id, reinforced = candID, true
			return e.reinforce(ctx, c, candID, nm, now)
		}
		id, reinforced = ids.New(), false
		if err := e.insert(ctx, c, id, nm, searchText, sim, bands, now); err != nil {
			return err
		}
		// Advisory contradiction suspects, judged by the recording agent. Same
		// transaction, so the read is consistent with the insert; never blocks it.
		if conflictKinds[nm.Kind] {
			seen := map[string]bool{}
			for _, m := range mid {
				if !seen[m.ID] {
					seen[m.ID] = true
					conflicts = append(conflicts, m)
				}
			}
			if refCands, rerr := e.refConflicts(ctx, c, nm.Kind, id, nm.Refs); rerr == nil {
				for _, m := range refCands {
					if !seen[m.ID] {
						seen[m.ID] = true
						conflicts = append(conflicts, m)
					}
				}
			}
			if len(conflicts) > maxConflictCandidates {
				conflicts = conflicts[:maxConflictCandidates]
			}
		}
		return nil
```

- [ ] **Step 4: Sweep the call sites** — update every `Engine.Record` caller (the compiler lists them; known: `internal/mcp/handlers.go:66` and `:220`, `internal/mcp/handlers_feedback.go:66`, `internal/cli/remember.go:42`, `internal/cli/bug.go:76`, plus any test fixtures and Task 7's new test). Everyone except `internal/mcp/handlers.go:66` (record_memory — Task 9 consumes it) and `internal/cli/remember.go:42` discards with `_`:

```go
	id, reinforced, _, err := a.Engine.Record(ctx, memory.NewMemory{
```

- [ ] **Step 5: Run** — `go test ./internal/memory/ -run TestRecord -count=1 -v` → new tests PASS; `go build ./... && go test ./... -count=1` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/ internal/mcp/handlers.go internal/mcp/handlers_feedback.go internal/cli/remember.go internal/cli/bug.go internal/agents/handle_test.go
git commit -m "feat(memory): Record surfaces middle-band + shared-ref conflict candidates (advisory)"
```

---

### Task 9: Surface conflict candidates — MCP `record_memory` + CLI `remember`

**Files:**
- Modify: `internal/mcp/handlers.go`
- Modify: `internal/cli/remember.go`
- Test: `internal/mcp/handlers_test.go`, `internal/cli/forget_test.go`-style new test in the remember test file

- [ ] **Step 1: Write the failing MCP test** — append to `internal/mcp/handlers_test.go`:

```go
func TestRecordMemorySurfacesConflictCandidates(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "preference", Title: "tabs", Body: "the user always wants tabs for indentation in Go files",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "preference", Title: "spaces", Body: "the user always wants spaces for indentation in Go files",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ConflictCandidates) == 0 {
		t.Fatal("middle-band conflict must surface in record_memory output")
	}
	if out.Note == "" || !strings.Contains(out.Note, "forget_memory") {
		t.Errorf("note must teach the resolution path, got %q", out.Note)
	}
}
```

- [ ] **Step 2: Verify failure** (`out.ConflictCandidates undefined`), then **implement** in `internal/mcp/handlers.go`:

```go
// ConflictCandidateOut is one existing memory the new one may contradict.
type ConflictCandidateOut struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Reason  string `json:"reason"` // similar wording | shares refs
}
```

extend `RecordMemoryOutput`:

```go
	// ConflictCandidates are existing memories the new one may contradict or
	// supersede — similar topic, different claim. Advisory: the write already
	// happened; review and resolve via Note's instructions.
	ConflictCandidates []ConflictCandidateOut `json:"conflict_candidates,omitempty"`
	Note               string                 `json:"note,omitempty"`
```

and in `recordMemory`, capture and map:

```go
	id, reinforced, conflicts, err := a.Engine.Record(ctx, memory.NewMemory{ ... unchanged ... })
	if err != nil {
		return RecordMemoryOutput{}, err
	}
	out := RecordMemoryOutput{ID: id, Reinforced: reinforced}
	for _, c := range conflicts {
		out.ConflictCandidates = append(out.ConflictCandidates, ConflictCandidateOut{
			ID: c.ID, Title: c.Title, Snippet: c.Snippet, Reason: c.Reason,
		})
	}
	if len(out.ConflictCandidates) > 0 {
		out.Note = "Review these existing memories — if your new memory supersedes or contradicts one, call forget_memory(old_id) (or re-record with supersedes_id). If they genuinely coexist, no action needed."
	}
	return out, nil
```

Append one sentence to the `record_memory` tool description in `internal/mcp/server.go`:

` When the new memory looks like it may contradict an existing one (same topic, different claim — or anchored to the same code), the response lists conflict_candidates with a resolution note; review them rather than leaving both claims standing.`

- [ ] **Step 3: CLI render** — in `internal/cli/remember.go` after the existing success print (read the file; it prints id/reinforced), add:

```go
	for i, c := range conflicts {
		if i == 0 {
			fmt.Fprintln(w, "may contradict existing memories — review:")
		}
		title := c.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• %s  %q (%s): %s\n", c.ID, title, c.Reason, c.Snippet)
	}
	if len(conflicts) > 0 {
		fmt.Fprintln(w, "(if superseded: recarto forget <id>)")
	}
```

(using the `conflicts` value Task 8's sweep already receives there), plus a CLI test mirroring the MCP one (two `Remember` calls, assert "may contradict" in the second output — follow the file's existing test style).

- [ ] **Step 4: Run** — `go test ./internal/mcp/ ./internal/cli/ -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/handlers_test.go internal/mcp/server.go internal/cli/remember.go internal/cli/remember_test.go
git commit -m "feat(mcp,cli): surface conflict candidates + resolution note on record"
```

---

### Task 10: Full verification + live smoke

**Files:** none new (fixes only if verification finds issues)

- [ ] **Step 1: Full gate**

```bash
gofmt -l internal/ | grep -v -e handlers_feedback_test.go -e memory/compact.go   # must print nothing
go vet ./...
go test ./... -count=1
```

- [ ] **Step 2: Live smoke in a throwaway repo**

```bash
go build -o /tmp/recarto ./cmd/recarto
rm -rf /tmp/r2smoke && mkdir -p /tmp/r2smoke && cd /tmp/r2smoke && git init -q && git commit -q --allow-empty -m init
printf 'package p\nfunc One() {}\n' > a.go
echo '{"session_id":"sm1","source":"startup","hook_event_name":"SessionStart"}' | /tmp/recarto hook session-start --agent claude
printf 'package p\nfunc One() { _ = 1 }\nfunc Two() {}\n' > a.go
echo '{"session_id":"sm1","tool_input":{"file_path":"/tmp/r2smoke/a.go"}}' | /tmp/recarto hook post-edit --agent claude
echo '{"session_id":"sm2","source":"clear","hook_event_name":"SessionStart"}' | /tmp/recarto hook session-start --agent claude
echo '{"session_id":"sm3","source":"startup","hook_event_name":"SessionStart"}' | /tmp/recarto hook session-start --agent claude
```

Expected: the `sm3` output's additionalContext contains `Changed since your last working session` with `+1 ~1` (Two added, One changed) — and so does `sm2`'s (the work happened in sm1's window). Then the nudge:

```bash
for i in 1 2 3; do echo '{"session_id":"sm3","tool_input":{"file_path":"/tmp/r2smoke/a.go"}}' | /tmp/recarto hook post-edit --agent claude >/dev/null; done
echo '{"session_id":"sm3"}' | /tmp/recarto hook stop --agent claude        # expect {"decision":"block","reason":"recarto: you made 3 edits…"}
echo '{"session_id":"sm3","stop_hook_active":true}' | /tmp/recarto hook stop --agent claude   # expect no block
```

And CLI parity + conflicts, from the recarto repo root:

```bash
cd /home/claude/Projects/recarto-feature-improve-or-add-features
/tmp/recarto graph refs Engine.SearchContentWithMeta | head -5          # expect the internal/mcp/graph.go call site
/tmp/recarto graph grep needle --offset=1 --limit=1 >/dev/null          # exit 0
/tmp/recarto graph snippet ContentSearchOpts | head -2                  # expect "// internal/graph/content.go:…" header then "type ContentSearchOpts struct {"
cd /tmp/r2smoke
/tmp/recarto remember preference --title tabs --body "the user always wants tabs for indentation in Go files" >/dev/null
/tmp/recarto remember preference --title spaces --body "the user always wants spaces for indentation in Go files"   # expect "may contradict existing memories"
```

(Adapt the `remember` argument shapes to the actual CLI flags — `recarto remember --help`.) Paste the actual outputs in the report; if any expectation fails, STOP and fix before committing anything.

- [ ] **Step 3: Hook subcommand check** — `/tmp/recarto hook stop` exists already (installed hooks reference it); confirm `KindFromString` covers it (it does — `handleStop` is wired) and that nothing in `internal/agents/install.go`/`claudecode/install.go` needs new hook registrations (it doesn't — Stop and PostToolUse:Edit are already installed).

- [ ] **Step 4: Record the round-2 learning** — via the MCP `record_memory` if the server is connected, else `/tmp/recarto remember learning …` from the repo root: title "LLM-UX round 2 shipped: delta tier + get_graph_delta, block-once capture nudge, conflict candidates, CLI parity", body summarizing the baseline-skips-no-op-sessions rule and the block-once channel choice, files `internal/graph/delta.go`, `internal/agents/handle.go`, `internal/memory/engine.go`.

- [ ] **Step 5: Commit any verification fixes**

```bash
git add -A && git commit -m "chore: round-2 verification fixes"
```

(Skip the commit if the tree is clean.)

---

## Self-review notes

- **Spec coverage:** CLI parity (Tasks 1-2), graph delta journal/tier/tool + retention (Tasks 3-6), gated block-once nudge (Task 7), conflict candidates (Tasks 8-9), verification/smoke (Task 10). All spec sections covered; out-of-scope items untouched.
- **Ordering constraints:** Task 3 (table) before 4 (journal) before 5/6 (consumers). Task 7's test uses the pre-Task-8 `Record` signature and is swept in Task 8 — flagged inline in both tasks. Task 9 consumes Task 8's return value.
- **Known judgment calls for the executor (flagged inline):** exact fixture/helper names in cli/agents/memory test files; the `memories` timestamp column name; `ids.New()` vs `randomblob` in `deleteFile`'s journal insert; the middle-band test bodies must actually land ≥0.25 Jaccard under the real shingle function.
- **Token discipline:** the only ambient addition is the delta tier (≤150 tokens, rendered only when non-empty); `cerebrumReserve` bumped accordingly so the memories tier shrinks rather than the block growing.
