# Session Context Injection & Bug Visibility — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make recarto orient Claude at session start (cerebrum memories + module map + recent bugs, under a tunable budget) and surface prior bugs the moment Claude reads/edits/greps the relevant code — to save tokens.

**Architecture:** A new opt-in `Baseline` recall mode seeds empty-query session-start recall with durable "cerebrum" kinds. New engine helpers (`BugsForCode`, `RecentBugs`) plus a path-normalization helper (`RelFileRef`) power just-in-time bug surfacing in the `post-read`/`post-edit`/`augment` hooks. The session-start hook renders a tiered, trimmable context block. `bug add` and `record_bug` gain symbol anchoring.

**Tech Stack:** Go 1.26, modernc SQLite (FTS5), tree-sitter (existing), standard `testing`.

**Spec:** `docs/superpowers/specs/2026-06-03-recarto-session-context-injection-design.md`

---

## File Structure

- `internal/memory/refs.go` **(create)** — `RelFileRef` path-normalization helper.
- `internal/memory/recall.go` **(modify)** — `RecallQuery.Baseline` + `baselineIDs`.
- `internal/memory/bugsurface.go` **(create)** — `BugsForCode`, `RecentBugs`.
- `internal/cli/bug.go` **(modify)** — `--symbols` + file normalization.
- `internal/mcp/handlers.go` **(modify)** — symbol anchoring + file normalization for `record_bug`/`record_memory`.
- `internal/agents/claudecode/hooks.go` **(modify)** — tiered `SessionStart`, `sessionBudget`, `formatSessionContext`, `formatBugWarnings`, `PostRead` bug surfacing.
- `internal/agents/claudecode/graphhooks.go` **(modify)** — `PostEdit` + `Augment` bug surfacing.

Tests live beside each file (`*_test.go`), matching the repo convention.

---

## Task 1: `RelFileRef` path-normalization helper

**Files:**
- Create: `internal/memory/refs.go`
- Test: `internal/memory/refs_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/memory/refs_test.go`:

```go
package memory

import "testing"

func TestRelFileRef(t *testing.T) {
	cases := []struct{ worktree, path, want string }{
		{"/repo", "/repo/internal/foo.go", "internal/foo.go"}, // abs under worktree → relative
		{"/repo", "internal/foo.go", "internal/foo.go"},       // already relative → unchanged
		{"/repo", "/other/x.go", "/other/x.go"},               // abs outside worktree → unchanged
		{"", "internal/foo.go", "internal/foo.go"},            // no worktree → unchanged
	}
	for _, c := range cases {
		if got := RelFileRef(c.worktree, c.path); got != c.want {
			t.Errorf("RelFileRef(%q,%q)=%q, want %q", c.worktree, c.path, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestRelFileRef`
Expected: FAIL — `undefined: RelFileRef`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/memory/refs.go`:

```go
package memory

import (
	"path/filepath"
	"strings"
)

// RelFileRef normalizes a file path to the repo-relative, forward-slash form used
// as a memory_refs file value. An absolute path under worktree becomes relative;
// anything else (already relative, or outside the worktree) is returned slash-
// normalized. This lets a bug recorded as "internal/foo.go" match a hook that
// reports the absolute path "/abs/worktree/internal/foo.go".
func RelFileRef(worktree, path string) string {
	if worktree != "" && filepath.IsAbs(path) {
		if rel, err := filepath.Rel(worktree, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestRelFileRef`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/refs.go internal/memory/refs_test.go
git commit -m "feat(memory): RelFileRef normalizes file refs to repo-relative"
```

---

## Task 2: Baseline recall mode

**Files:**
- Modify: `internal/memory/recall.go` (add field to `RecallQuery`; seed candidates in `Recall`; add `baselineIDs`)
- Test: `internal/memory/recall_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/memory/recall_test.go`:

```go
func TestBaselineRecallSurfacesCerebrumKinds(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	for _, nm := range []NewMemory{
		{Kind: KindPreference, Title: "pref", Body: "use tabs not spaces here", Branch: "main", Source: "cli"},
		{Kind: KindLearning, Title: "learn", Body: "store lives outside the worktree", Branch: "main", Source: "cli"},
		{Kind: KindDecision, Title: "decide", Body: "chose sqlite over flat files", Branch: "main", Source: "cli"},
		{Kind: KindAction, Title: "act", Body: "ran the formatter once today", Branch: "main", Source: "cli"},
	} {
		if _, _, err := e.Record(ctx, nm); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	got := map[Kind]bool{}
	for _, r := range res {
		got[r.Record.Kind] = true
	}
	for _, k := range []Kind{KindPreference, KindLearning, KindDecision} {
		if !got[k] {
			t.Errorf("baseline recall missing kind %s", k)
		}
	}
	if got[KindAction] {
		t.Errorf("baseline recall must exclude action (ephemeral)")
	}
}

func TestBaselineRecallExcludesBug(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "keep", Body: "a durable learning fact", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "boom", Body: "npe in the request handler", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Record.Kind == KindBug {
			t.Errorf("baseline recall must exclude bug, got %q", r.Record.Title)
		}
	}
}

func TestBaselineRecallBranchVisible(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "global", Body: "alpha unique global one", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "onbranch", Body: "beta unique feature two", Branch: "feature", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "otherbranch", Body: "gamma unique main three", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "feature", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, r := range res {
		titles[r.Record.Title] = true
	}
	if !titles["global"] || !titles["onbranch"] {
		t.Errorf("want global+onbranch visible on feature branch; got %v", titles)
	}
	if titles["otherbranch"] {
		t.Errorf("main-branch memory leaked onto feature branch")
	}
}

func TestBaselineRecallRespectsBudgetAndPinsFirst(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	pinID, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat, Title: "never", Body: "never hardcode secrets in source", Branch: "main", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf("decision number %d with a fairly long body to consume token budget quickly", i)
		if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision, Title: fmt.Sprintf("d%d", i), Body: body, Branch: "main", Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 20})
	if err != nil {
		t.Fatal(err)
	}
	hasPin := false
	for _, r := range res {
		if r.Record.ID == pinID {
			hasPin = true
		}
	}
	if !hasPin {
		t.Errorf("pinned do_not_repeat must be included regardless of budget")
	}
	if len(res) > 3 {
		t.Errorf("budget not enforced: got %d results", len(res))
	}
}
```

Add `"fmt"` to the import block of `recall_test.go` (it currently imports `context`, `testing`, `time`, plus the internal packages).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run TestBaselineRecall`
Expected: FAIL — `unknown field 'Baseline' in struct literal`.

- [ ] **Step 3: Add the `Baseline` field**

In `internal/memory/recall.go`, in the `RecallQuery` struct, add the field after `Branch`:

```go
	Branch      string // current branch, for branch-affinity boost
	Baseline    bool   // session-start: also seed candidates with recent cerebrum memories (no query needed)
	Limit       int    // max lexical candidates (0 → default)
	TokenBudget int    // 0 → default
```

- [ ] **Step 4: Seed baseline candidates in `Recall`**

In `internal/memory/recall.go`, in `Recall`, immediately after the `pinned` loop that fills `idset` (the loop `for _, id := range pinned { idset[id] = true }`), insert:

```go
	if q.Baseline {
		base, err := e.baselineIDs(ctx, q.Branch, limit)
		if err != nil {
			return nil, err
		}
		for _, id := range base {
			idset[id] = true
		}
	}
```

- [ ] **Step 5: Add the `baselineIDs` helper**

In `internal/memory/recall.go`, add after the existing `pinnedIDs` function:

```go
// baselineIDs returns active, branch-visible "cerebrum" memories — preference,
// learning, decision, and (unpinned) do_not_repeat — most-recently-updated first,
// capped at limit. It seeds an empty-query session-start recall so durable
// knowledge surfaces without a task hint. Ephemeral kinds (action) and bugs are
// intentionally excluded: bugs surface just-in-time on code contact instead.
func (e *Engine) baselineIDs(ctx context.Context, branch string, limit int) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE status='active'
		    AND kind IN ('preference','learning','decision','do_not_repeat')
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC
		  LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestBaselineRecall`
Expected: PASS (4 tests).

- [ ] **Step 7: Run the full memory package to confirm no regressions**

Run: `go test ./internal/memory/`
Expected: PASS (existing recall tests don't set `Baseline`, so behavior is unchanged).

- [ ] **Step 8: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go
git commit -m "feat(memory): RecallQuery.Baseline seeds session-start cerebrum recall"
```

---

## Task 3: `BugsForCode` and `RecentBugs` engine helpers

**Files:**
- Create: `internal/memory/bugsurface.go`
- Test: `internal/memory/bugsurface_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/bugsurface_test.go`:

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestBugsForCodeMatchesByFileRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "nil deref", Body: "nil deref in parser — fix: guard",
		Refs: []Ref{{Type: RefFile, Value: "internal/parse/parse.go"}},
		Branch: "main", Source: "cli", Extra: map[string]any{"fix": "guard"},
	}); err != nil {
		t.Fatal(err)
	}
	// unrelated bug on another file must not match
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "other", Body: "an unrelated bug elsewhere",
		Refs: []Ref{{Type: RefFile, Value: "internal/store/store.go"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	// a learning on the SAME file must not be returned (bugs only)
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "note", Body: "a note about the parser file",
		Refs: []Ref{{Type: RefFile, Value: "internal/parse/parse.go"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.BugsForCode(ctx, "main", []string{"internal/parse/parse.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "nil deref" {
		t.Fatalf("want exactly 1 bug 'nil deref', got %d: %+v", len(bugs), bugs)
	}
}

func TestBugsForCodeMatchesBySymbolRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "off by one", Body: "off by one in Tokenize loop",
		Refs: []Ref{{Type: RefSymbol, Value: "Tokenize"}}, Branch: "main", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "off by one" {
		t.Fatalf("want 1 bug 'off by one', got %d: %+v", len(bugs), bugs)
	}
}

func TestRecentBugsOrderedByRecency(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "older", Body: "older bug body alpha", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "newer", Body: "newer bug body beta", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	bugs, err := e.RecentBugs(ctx, "main", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 2 || bugs[0].Title != "newer" {
		t.Fatalf("want 2 bugs, newer first; got %d: %+v", len(bugs), bugs)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run "TestBugsForCode|TestRecentBugs"`
Expected: FAIL — `e.BugsForCode undefined` / `e.RecentBugs undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/memory/bugsurface.go`:

```go
package memory

import (
	"context"
	"sort"
)

// BugsForCode returns active kind=bug memories that reference any of the given
// files or symbols, branch-visible (branch-scoped or repo-global), most-recently-
// updated first. It powers just-in-time bug surfacing in the read/edit/grep hooks:
// a bug recorded against a file/symbol reappears the moment Claude touches it.
func (e *Engine) BugsForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
	type qref struct{ t, v string }
	var qrefs []qref
	for _, f := range files {
		qrefs = append(qrefs, qref{string(RefFile), f})
	}
	for _, s := range symbols {
		qrefs = append(qrefs, qref{string(RefSymbol), s})
	}
	if len(qrefs) == 0 {
		return nil, nil
	}
	idset := map[string]bool{}
	for _, qr := range qrefs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT m.id FROM memory_refs r
			   JOIN memories m ON m.id = r.memory_id
			  WHERE r.ref_type=? AND r.ref_value=?
			    AND m.kind='bug' AND m.status='active'
			    AND (m.branch IS NULL OR m.branch=?)`,
			qr.t, qr.v, branch)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			idset[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	recs, err := e.loadByIDs(ctx, idset)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].UpdatedAt > recs[j].UpdatedAt })
	return recs, nil
}

// RecentBugs returns up to limit active, branch-visible bug memories, most recent
// first — the ambient "bugs we've bitten on" tier for session start.
func (e *Engine) RecentBugs(ctx context.Context, branch string, limit int) ([]*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		selectMemoriesSQL+`
		  WHERE status='active' AND kind='bug'
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	return recs, err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run "TestBugsForCode|TestRecentBugs"`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/memory/bugsurface.go internal/memory/bugsurface_test.go
git commit -m "feat(memory): BugsForCode + RecentBugs for just-in-time bug surfacing"
```

---

## Task 4: `--symbols` on `bug add` + file normalization

**Files:**
- Modify: `internal/cli/bug.go` (`bugAdd`)
- Test: `internal/cli/bug_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/bug_test.go` (it already constructs an App via the package's test helper — reuse the same helper the existing `TestBugAddThenSearch` uses; here we assert the symbol ref is created and recallable by symbol):

```go
func TestBugAddAcceptsSymbols(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := Bug(ctx, a, &bytes.Buffer{}, []string{"add",
		"--error", "panic in Tokenize",
		"--fix", "guard the slice",
		"--symbols", "Tokenize,Lexer",
	}); err != nil {
		t.Fatal(err)
	}
	// The bug is now retrievable by one of its symbol refs.
	bugs, err := a.Engine.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "panic in Tokenize" {
		t.Fatalf("want 1 bug by symbol Tokenize, got %d: %+v", len(bugs), bugs)
	}
}
```

> Note: `internal/cli/bug_test.go` already imports `bytes`, `context`, `testing` and uses the package's `newTestApp` helper (see `internal/cli/helpers_test.go`). If `bytes` is not yet imported there, add it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestBugAddAcceptsSymbols`
Expected: FAIL — `flag provided but not defined: -symbols`.

- [ ] **Step 3: Implement `--symbols` + file normalization**

In `internal/cli/bug.go`, in `bugAdd`, add the flag next to the others:

```go
	file := fs.String("file", "", "file that was fixed")
	symbols := fs.String("symbols", "", "comma-separated related symbol names")
	tags := fs.String("tags", "", "comma-separated tags")
```

Then replace the structural-ref block:

```go
	var refs []memory.Ref
	if *file != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: *file})
	}
```

with:

```go
	var refs []memory.Ref
	if *file != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(a.Git.Worktree, *file)})
	}
	for _, s := range splitCSV(*symbols) {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestBugAddAcceptsSymbols`
Expected: PASS.

- [ ] **Step 5: Run the cli package to confirm no regressions**

Run: `go test ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/bug.go internal/cli/bug_test.go
git commit -m "feat(cli): bug add --symbols + repo-relative file ref"
```

---

## Task 5: `record_bug` / `record_memory` symbol anchoring + normalization (MCP)

**Files:**
- Modify: `internal/mcp/handlers.go` (`RecordBugInput`, `recordBug`, `refsFrom`, `recordMemory`)
- Test: `internal/mcp/handlers_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/mcp/handlers_test.go` (reuse the package's existing test App helper — the same one used by current handler tests):

```go
func TestRecordBugCapturesSymbols(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordBug(ctx, a, RecordBugInput{
		ErrorMessage: "panic in Tokenize",
		Fix:          "guard the slice",
		Symbols:      []string{"Tokenize"},
	}); err != nil {
		t.Fatal(err)
	}
	bugs, err := a.Engine.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "panic in Tokenize" {
		t.Fatalf("want 1 bug by symbol, got %d: %+v", len(bugs), bugs)
	}
}
```

> Note: confirm the helper name in `internal/mcp/handlers_test.go` / `server_test.go`. If it differs from `newTestApp`, use that package's existing helper verbatim — do not introduce a new one.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestRecordBugCapturesSymbols`
Expected: FAIL — `unknown field 'Symbols' in struct literal of type RecordBugInput`.

- [ ] **Step 3: Add `Symbols` to `RecordBugInput`**

In `internal/mcp/handlers.go`, in `RecordBugInput`, add after `File`:

```go
	File         string   `json:"file,omitempty" jsonschema:"the file that was fixed"`
	Symbols      []string `json:"symbols,omitempty" jsonschema:"related symbol names"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
```

- [ ] **Step 4: Build refs (file + symbols) in `recordBug`**

In `recordBug`, replace:

```go
	var refs []memory.Ref
	if in.File != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: in.File})
	}
```

with:

```go
	var refs []memory.Ref
	if in.File != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(a.Git.Worktree, in.File)})
	}
	for _, s := range in.Symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
```

- [ ] **Step 5: Normalize file refs in the generic `record_memory` path**

In `internal/mcp/handlers.go`, change `refsFrom` to normalize file values against the worktree. Replace the signature and file loop:

```go
// refsFrom builds structural refs from file/symbol/tag value lists. File values
// are normalized to repo-relative form so they match hook-reported absolute paths.
func refsFrom(worktree string, files, symbols, tags []string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range files {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(worktree, f)})
	}
	for _, s := range symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range tags {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
}
```

Then update its only caller in `recordMemory`:

```go
		Refs:         refsFrom(a.Git.Worktree, in.Files, in.Symbols, in.Tags),
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/mcp/ -run TestRecordBugCapturesSymbols`
Expected: PASS.

- [ ] **Step 7: Run the mcp package to confirm no regressions**

Run: `go test ./internal/mcp/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/handlers_test.go
git commit -m "feat(mcp): record_bug symbol anchoring + repo-relative file refs"
```

---

## Task 6: Tiered session-start context

**Files:**
- Modify: `internal/agents/claudecode/hooks.go` (imports, constants, `sessionBudget`, `SessionStart`, `formatSessionContext`, add `trimOneLine`)
- Test: `internal/agents/claudecode/hooks_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Append to `internal/agents/claudecode/hooks_test.go`:

```go
func TestSessionStartInjectsCerebrumAndRecentBugs(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindDecision, Title: "Use SQLite", Body: "chose sqlite with wal", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "Store outside tree", Body: "db lives outside the worktree", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindBug, Title: "panic in handler", Body: "panic in handler — fix: guard", Source: "cli", Extra: map[string]any{"fix": "guard"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"s1","source":"startup"}`), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"Use SQLite", "Store outside tree", "panic in handler", "record_memory"} {
		if !strings.Contains(s, want) {
			t.Errorf("session-start output missing %q:\n%s", want, s)
		}
	}
}

func TestSessionBudgetEnvOverride(t *testing.T) {
	t.Setenv("RECARTO_SESSION_BUDGET", "50")
	if got := sessionBudget(); got != 50 {
		t.Errorf("sessionBudget() = %d, want 50", got)
	}
	t.Setenv("RECARTO_SESSION_BUDGET", "")
	if got := sessionBudget(); got != sessionStartBudget {
		t.Errorf("sessionBudget() default = %d, want %d", got, sessionStartBudget)
	}
}

func TestFormatSessionContextDegradesToTitles(t *testing.T) {
	results := []memory.RecallResult{
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "AAA", Body: "short body"}},
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "BBB", Body: strings.Repeat("x", 2000)}},
	}
	s := formatSessionContext(results, graph.Architecture{}, nil, 60)
	if !strings.Contains(s, "AAA") || !strings.Contains(s, "short body") {
		t.Errorf("first entry should render with its body:\n%s", s)
	}
	if !strings.Contains(s, "BBB") {
		t.Errorf("second entry title should still appear:\n%s", s)
	}
	if strings.Contains(s, strings.Repeat("x", 100)) {
		t.Errorf("second entry body should be dropped under tight budget:\n%s", s)
	}
}
```

Add `"git.gerwim.nl/gerwim/recarto/internal/graph"` to the imports of `hooks_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agents/claudecode/ -run "TestSessionStartInjectsCerebrumAndRecentBugs|TestSessionBudgetEnvOverride|TestFormatSessionContextDegradesToTitles"`
Expected: FAIL — `too many arguments in call to formatSessionContext` / `undefined: sessionBudget`.

- [ ] **Step 3: Update imports + constants in `hooks.go`**

In `internal/agents/claudecode/hooks.go`, add `"os"` and `"strconv"` to the import block (it already imports `context`, `encoding/json`, `fmt`, `io`, `strings`, and the internal `app`, `graph`, `ledger`, `memory`, `store`).

Replace the single constant line:

```go
// sessionStartBudget is the token budget for the session-start recall slice (spec §9.2).
const sessionStartBudget = 1500
```

with:

```go
// Session-start tier sizing. sessionStartBudget is the default token budget for
// the injected block (override via RECARTO_SESSION_BUDGET). The map/bug tiers are
// bounded by line caps; cerebrumReserve keeps headroom for them.
const (
	sessionStartBudget = 1500
	recentBugLimit     = 5
	archModuleLimit    = 8
	archDepLimit       = 8
	cerebrumReserve    = 200
	cerebrumBodyMax    = 160
)

// sessionBudget is the session-start token budget: RECARTO_SESSION_BUDGET if set
// to a positive integer, else the default.
func sessionBudget() int {
	if v := os.Getenv("RECARTO_SESSION_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return sessionStartBudget
}
```

- [ ] **Step 4: Rewrite the recall+render block of `SessionStart`**

In `SessionStart`, replace:

```go
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		TokenBudget: sessionStartBudget,
	})
	if err != nil {
		return err
	}
	return writeHookContext(out, "SessionStart", formatSessionContext(results))
```

with:

```go
	budget := sessionBudget()
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		Baseline:    true,
		TokenBudget: budget,
	})
	if err != nil {
		return err
	}
	// Orientation map + recent bugs are best-effort tiers; failure omits the tier.
	arch, _ := a.Graph.Architecture(ctx, a.Git.Worktree, "")
	bugs, _ := a.Engine.RecentBugs(ctx, a.Git.Branch, recentBugLimit)
	return writeHookContext(out, "SessionStart", formatSessionContext(results, arch, bugs, budget))
```

- [ ] **Step 5: Replace `formatSessionContext` and add `trimOneLine`**

In `internal/agents/claudecode/hooks.go`, replace the entire existing `formatSessionContext` function with:

```go
// formatSessionContext renders the tiered session-start block: pinned do_not_repeat
// (always, full body), cerebrum memories (title + trimmed body, degrading to
// titles-only then dropping under budget pressure), a bounded module map, and a
// titles-only recent-bugs tier — followed by the record nudge.
func formatSessionContext(results []memory.RecallResult, arch graph.Architecture, bugs []*memory.Record, budget int) string {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")

	budgetCap := budget - cerebrumReserve
	if budgetCap < 0 {
		budgetCap = budget
	}
	used := 0
	wroteAny := false
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		if r.Record.Kind == memory.KindDoNotRepeat && r.Record.Pinned {
			line := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, r.Record.Body)
			b.WriteString(line)
			used += ledger.EstimateTokens(line)
			wroteAny = true
			continue
		}
		full := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, trimOneLine(r.Record.Body))
		titleOnly := fmt.Sprintf("- [%s] %s\n", r.Record.Kind, title)
		switch {
		case used+ledger.EstimateTokens(full) <= budgetCap:
			b.WriteString(full)
			used += ledger.EstimateTokens(full)
			wroteAny = true
		case used+ledger.EstimateTokens(titleOnly) <= budgetCap:
			b.WriteString(titleOnly)
			used += ledger.EstimateTokens(titleOnly)
			wroteAny = true
		}
	}
	if !wroteAny {
		b.WriteString("(no prior memories yet)\n")
	}

	if len(arch.Modules) > 0 {
		b.WriteString("\nCode map (modules):\n")
		for i, m := range arch.Modules {
			if i >= archModuleLimit {
				break
			}
			fmt.Fprintf(&b, "- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for i, d := range arch.Deps {
			if i >= archDepLimit {
				break
			}
			fmt.Fprintf(&b, "  %s → %s (%d)\n", d.From, d.To, d.Count)
		}
		b.WriteString("Query the code graph (search/snippet/callers) instead of reading whole files; grep/glob auto-injects matching symbols.\n")
	}

	if len(bugs) > 0 {
		b.WriteString("\nRecent bugs (search_bugs for detail):\n")
		for _, r := range bugs {
			t := r.Title
			if t == "" {
				t = "(untitled)"
			}
			fmt.Fprintf(&b, "- %s\n", t)
		}
	}

	b.WriteString("\nRecord new learnings with the record_memory tool and bug fixes with record_bug.")
	return b.String()
}

// trimOneLine collapses a memory body to its first line, capped at cerebrumBodyMax
// runes, for the budget-conscious cerebrum tier.
func trimOneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > cerebrumBodyMax {
		s = s[:cerebrumBodyMax] + "…"
	}
	return s
}
```

- [ ] **Step 6: Run the new tests**

Run: `go test ./internal/agents/claudecode/ -run "TestSessionStartInjectsCerebrumAndRecentBugs|TestSessionBudgetEnvOverride|TestFormatSessionContextDegradesToTitles"`
Expected: PASS (3 tests).

- [ ] **Step 7: Run the whole package to confirm existing hook tests still pass**

Run: `go test ./internal/agents/claudecode/`
Expected: PASS. (`TestSessionStartInjectsRecallAndOpensSession` still passes: the pinned do_not_repeat renders full; the nudge is unchanged.)

- [ ] **Step 8: Commit**

```bash
git add internal/agents/claudecode/hooks.go internal/agents/claudecode/hooks_test.go
git commit -m "feat(hooks): tiered budgeted session-start context (cerebrum, map, recent bugs)"
```

---

## Task 7: PostRead bug surfacing

**Files:**
- Modify: `internal/agents/claudecode/hooks.go` (`PostRead`, add `formatBugWarnings`)
- Test: `internal/agents/claudecode/hooks_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/agents/claudecode/hooks_test.go`:

```go
func TestPostReadSurfacesBugForFile(t *testing.T) {
	a := newTestApp(t, "main") // worktree "/repo"
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "nil deref here", Body: "nil deref — fix: guard",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "internal/store/store.go"}},
		Source: "cli", Extra: map[string]any{"fix": "guard"},
	}); err != nil {
		t.Fatal(err)
	}
	// Claude reports an ABSOLUTE path; normalization must still match the rel ref.
	payload := `{"session_id":"s1","tool_input":{"file_path":"/repo/internal/store/store.go"},"tool_result":"package store"}`
	var out bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "prior bug") || !strings.Contains(s, "nil deref here") {
		t.Errorf("post-read should surface the prior bug:\n%s", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/claudecode/ -run TestPostReadSurfacesBugForFile`
Expected: FAIL — output does not contain "prior bug".

- [ ] **Step 3: Rewrite `PostRead` to combine the re-read warning and bug surfacing**

In `internal/agents/claudecode/hooks.go`, replace the body of `PostRead` from `n, err := a.Ledger.Get(...)` to the end of the function with:

```go
	n, err := a.Ledger.Get(ctx, scope, "read:"+file)
	if err != nil {
		return err
	}

	var msgs []string
	if n > 1 {
		_ = a.Ledger.Incr(ctx, "lifetime", "repeated_reads_blocked", 1)
		msgs = append(msgs, fmt.Sprintf("You have already read %s in this session (%d times, ~%d tokens each). "+
			"Rely on the earlier read instead of reading it again.", file, n, tokens))
	}
	rel := memory.RelFileRef(a.Git.Worktree, file)
	if found, err := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil && len(found) > 0 {
		msgs = append(msgs, formatBugWarnings(rel, found))
	}
	if len(msgs) == 0 {
		return nil
	}
	return writeHookContext(out, "PostToolUse", strings.Join(msgs, "\n"))
```

(Delete the old `if n <= 1 { return nil }` block and the old single-message `return writeHookContext(...)` it replaced.)

- [ ] **Step 4: Add `formatBugWarnings`**

In `internal/agents/claudecode/hooks.go`, add:

```go
// formatBugWarnings renders prior bugs tied to a code location as advisory lines.
func formatBugWarnings(loc string, bugs []*memory.Record) string {
	lines := make([]string, 0, len(bugs))
	for _, r := range bugs {
		if fix, _ := r.Extra["fix"].(string); fix != "" {
			lines = append(lines, fmt.Sprintf("⚠ prior bug in %s: %s — fix: %s", loc, r.Title, fix))
		} else {
			lines = append(lines, fmt.Sprintf("⚠ prior bug in %s: %s", loc, r.Title))
		}
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 5: Run the new test + the existing re-read test**

Run: `go test ./internal/agents/claudecode/ -run "TestPostReadSurfacesBugForFile|TestPostReadWarnsOnRepeat"`
Expected: PASS. (`TestPostReadWarnsOnRepeat` reads `internal/store/store.go` with no bug recorded → first read silent, second read warns; `repeated_reads_blocked == 1`.)

- [ ] **Step 6: Commit**

```bash
git add internal/agents/claudecode/hooks.go internal/agents/claudecode/hooks_test.go
git commit -m "feat(hooks): post-read surfaces prior bugs for the file being read"
```

---

## Task 8: PostEdit + Augment bug surfacing

**Files:**
- Modify: `internal/agents/claudecode/graphhooks.go` (`PostEdit`, `Augment`, imports)
- Test: `internal/agents/claudecode/graphhooks_test.go` (append)

- [ ] **Step 1: Write the failing tests**

Append to `internal/agents/claudecode/graphhooks_test.go`:

```go
func TestPostEditSurfacesBugForEditedFile(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc F(){}"), 0o644)
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "edge case in F", Body: "edge case in F — fix: clamp",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a.go"}},
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	var out bytes.Buffer
	if err := PostEdit(ctx, a, strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "edge case in F") {
		t.Errorf("post-edit should surface the prior bug for the edited file:\n%s", out.String())
	}
}

func TestAugmentSurfacesBugsMatchingPattern(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	ctx := context.Background()
	PostEdit(ctx, a, strings.NewReader(`{"session_id":"s1","tool_input":{"file_path":"`+filepath.Join(wt, "a.go")+`"}}`), &bytes.Buffer{})
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Authenticate bypass", Body: "Authenticate bypass when token empty — fix: reject",
		Source: "cli", Extra: map[string]any{"fix": "reject"},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Augment(ctx, a, strings.NewReader(`{"session_id":"s1","tool_input":{"pattern":"Authenticate"}}`), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Authenticate") { // graph symbol match (existing behavior)
		t.Errorf("augment lost its symbol match:\n%s", s)
	}
	if !strings.Contains(s, "Authenticate bypass") { // new: matching bug
		t.Errorf("augment should surface the matching bug:\n%s", s)
	}
}
```

Add `"git.gerwim.nl/gerwim/recarto/internal/memory"` to the imports of `graphhooks_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agents/claudecode/ -run "TestPostEditSurfacesBugForEditedFile|TestAugmentSurfacesBugsMatchingPattern"`
Expected: FAIL — PostEdit produces no output; Augment lacks the bug line.

- [ ] **Step 3: Add the `memory` import to `graphhooks.go`**

In `internal/agents/claudecode/graphhooks.go`, add `"git.gerwim.nl/gerwim/recarto/internal/memory"` to the import block (it currently imports `context`, `fmt`, `io`, `os`, `path/filepath`, `strings`, and the internal `app`).

- [ ] **Step 4: Surface bugs in `PostEdit` (before the re-index)**

In `PostEdit`, change the signature to use the writer and insert bug surfacing after computing `rel`. Replace:

```go
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
```

with:

```go
func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
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
	// Surface any prior bug tied to this file before re-indexing (best-effort).
	// This matches stored refs by path and does not require the file on disk.
	if found, berr := a.Engine.BugsForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil && len(found) > 0 {
		_ = writeHookContext(out, "PostToolUse", formatBugWarnings(rel, found))
	}
	content, err := os.ReadFile(abs)
```

(The rest of `PostEdit` — `IndexFile` then `Resolve` — is unchanged.)

- [ ] **Step 5: Rewrite `Augment` to add matching bugs and not early-return**

Replace the entire `Augment` function with:

```go
// Augment runs a code-graph search matching the agent's Grep/Glob pattern and
// also surfaces any kind=bug memories matching that pattern, injecting both as
// advisory additionalContext (non-blocking).
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

	var b strings.Builder
	if res, err := a.Graph.Search(ctx, a.Git.Worktree, pattern, "", "", 8); err == nil && len(res) > 0 {
		b.WriteString("Recarto code graph — symbols matching your search:\n")
		for _, r := range res {
			fmt.Fprintf(&b, "- [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
		}
		b.WriteString("Use get_code_snippet to read one symbol instead of the whole file.")
	}

	if bugRes, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: pattern, Kind: memory.KindBug, Branch: a.Git.Branch, Limit: 5,
	}); err == nil {
		var bugLines []string
		for _, r := range bugRes {
			if r.Record.Kind != memory.KindBug { // recall always includes pinned do_not_repeat
				continue
			}
			if fix, _ := r.Record.Extra["fix"].(string); fix != "" {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s — fix: %s", r.Record.Title, fix))
			} else {
				bugLines = append(bugLines, fmt.Sprintf("- ⚠ %s", r.Record.Title))
			}
		}
		if len(bugLines) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("Related bugs (search_bugs for detail):\n")
			b.WriteString(strings.Join(bugLines, "\n"))
		}
	}

	if b.Len() == 0 {
		return nil
	}
	return writeHookContext(out, "PostToolUse", b.String())
}
```

- [ ] **Step 6: Run the new tests + the existing graph-hook tests**

Run: `go test ./internal/agents/claudecode/ -run "TestPostEdit|TestAugment"`
Expected: PASS. (`TestPostEditIndexesFile` still indexes `Edited`; `TestAugmentInjectsSearchContext` still mentions `Authenticate`.)

- [ ] **Step 7: Commit**

```bash
git add internal/agents/claudecode/graphhooks.go internal/agents/claudecode/graphhooks_test.go
git commit -m "feat(hooks): post-edit + augment surface prior bugs just-in-time"
```

---

## Task 9: Full suite + manual smoke test

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: PASS — all packages `ok` (no `FAIL`).

- [ ] **Step 2: Build the binary**

Run: `go build -o /tmp/recarto-sci ./cmd/recarto`
Expected: exit 0.

- [ ] **Step 3: Manual smoke — session-start tiers + bug surfacing**

Run (in a throwaway cache so the real store is untouched):

```bash
export RECARTO_CACHE_DIR=$(mktemp -d)
cd /home/claude/Projects/recarto
/tmp/recarto-sci index >/dev/null
/tmp/recarto-sci remember decision --title "Use SQLite" --body "chose sqlite with wal for concurrency"
/tmp/recarto-sci bug add --error "panic in Architecture" --fix "guard nil module map" --file internal/graph/architecture.go --symbols Architecture
echo '{"session_id":"smoke","source":"startup"}' | /tmp/recarto-sci hook session-start
echo '{"session_id":"smoke","tool_input":{"file_path":"'"$PWD"'/internal/graph/architecture.go"},"tool_result":"x"}' | /tmp/recarto-sci hook post-read
echo '{"tool_input":{"pattern":"Architecture"}}' | /tmp/recarto-sci hook augment
```

Expected:
- `session-start` JSON contains "Use SQLite", a "Code map (modules)" section, and a "Recent bugs" line with "panic in Architecture".
- `post-read` JSON contains "⚠ prior bug in internal/graph/architecture.go: panic in Architecture".
- `augment` JSON contains both the `Architecture` symbol match and "Related bugs" with "panic in Architecture".

- [ ] **Step 4: Final commit (if any docs/notes changed)**

If nothing changed in this task, skip. Otherwise:

```bash
git add -A
git commit -m "chore: session-context injection verification notes"
```

---

## Self-Review (completed during authoring)

- **Spec coverage:** §3.1 tiers → Task 6; §3.2 baseline → Task 2; §3.3 just-in-time surfacing → Tasks 7 (post-read), 8 (post-edit, augment); §3.4 symbol anchoring → Tasks 4 (cli), 5 (mcp); §3.5 normalization → Task 1 + applied in Tasks 4/5/7/8; §7 tests → each task; §8 measurement → existing ledger counters (no code). All covered.
- **Type consistency:** `RelFileRef`, `baselineIDs`, `BugsForCode`, `RecentBugs`, `formatSessionContext(results, arch, bugs, budget)`, `formatBugWarnings(loc, bugs)`, `sessionBudget()`, `trimOneLine` are defined once and used with matching signatures throughout.
- **Placeholder scan:** none — every code/test step is complete.
- **Known assumptions to verify at execution time:** the exact test-helper name in `internal/mcp/handlers_test.go` (use whatever that package already uses, not a new helper); that `internal/cli/bug_test.go` imports `bytes` (add if missing).
