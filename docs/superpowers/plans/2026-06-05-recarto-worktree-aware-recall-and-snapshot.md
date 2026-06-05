# Worktree-Aware Recall & Shareable Memory Snapshot — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make recall worktree-aware (caveat/demote memories whose referenced code is absent from the current worktree) and add a committable team-memory snapshot with dedup-aware import.

**Architecture:** Recall gains an optional `RefResolver` (graph-backed, queries the `files`/`nodes` tables in the shared store) that reports which of a memory's code refs are missing from the current worktree; missing refs produce a caveat and, when *all* code refs dangle, a score demotion (pinned/bug exempted from demotion but still caveated). A new `internal/snapshot` package exports active+pinned memories to `.recarto/memory.jsonl` and imports them back through a new `Engine.ImportMemory` that reuses the existing MinHash near-dup path.

**Tech Stack:** Go 1.26.4, pure-Go SQLite store, stdlib `flag`/`encoding/json`, existing `internal/memory` fingerprint + ledger.

**Spec:** `docs/superpowers/specs/2026-06-05-recarto-worktree-aware-recall-and-snapshot-design.md`

**Conventions verified in the codebase:**
- Memory tests: `newTestEngine(t, clk)` (`internal/memory/engine_test.go:14`) returns `*Engine`; same-package tests reach `e.st` (the `*store.Store`) directly.
- App/CLI tests: `newTestApp(t, branch)` (`internal/app/app_test.go:38`, `internal/cli/helpers_test.go:38`).
- File refs are repo-relative forward-slash on both sides: memory via `memory.RelFileRef`, graph `files.path` via git ls-files. Equality join, no normalization.
- Run full gate before each commit: `gofmt -l .` (must be empty), `go vet ./...`, `go test ./...`, and `go test -race ./internal/memory/... ./internal/app/... ./internal/snapshot/...` for touched packages.

---

## Task 1: RefResolver foundation (interface, result field, engine wiring)

No behavior change yet — just the seams.

**Files:**
- Create: `internal/memory/resolve.go`
- Modify: `internal/memory/recall.go` (add `Worktree` to `RecallQuery`; add `DanglingRefs` to `RecallResult`)
- Modify: `internal/memory/engine.go` (add `res RefResolver` field + nothing else)
- Test: `internal/memory/resolve_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/memory/resolve_test.go
package memory

import (
	"context"
	"testing"
)

// fakeResolver returns the refs whose Value is in the dangling set.
type fakeResolver struct{ dangling map[string]bool }

func (f fakeResolver) Dangling(_ context.Context, _ string, refs []Ref) ([]Ref, error) {
	var out []Ref
	for _, r := range refs {
		if f.dangling[r.Value] {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestCodeRefsOfFiltersTags(t *testing.T) {
	refs := []Ref{
		{Type: RefFile, Value: "a.go"},
		{Type: RefTag, Value: "perf"},
		{Type: RefSymbol, Value: "Foo"},
	}
	got := codeRefsOf(refs)
	if len(got) != 2 {
		t.Fatalf("want 2 code refs, got %d: %v", len(got), got)
	}
}

func TestCaveatFormat(t *testing.T) {
	r := RecallResult{
		Record:       &Record{Branch: "feat/x"},
		DanglingRefs: []Ref{{Type: RefFile, Value: "internal/foo.go"}},
	}
	c := r.Caveat()
	if c == "" || !contains(c, "feat/x") || !contains(c, "internal/foo.go") {
		t.Fatalf("unexpected caveat: %q", c)
	}
	empty := RecallResult{Record: &Record{}}
	if empty.Caveat() != "" {
		t.Fatalf("want empty caveat for no dangling refs, got %q", empty.Caveat())
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestCodeRefsOf|TestCaveat' -v`
Expected: FAIL — `undefined: codeRefsOf`, `RecallResult has no field DanglingRefs`, `r.Caveat undefined`.

- [ ] **Step 3: Add the field to RecallResult and Worktree to RecallQuery**

In `internal/memory/recall.go`, add to `RecallQuery` (after `Branch string ...`):

```go
	Worktree    string // current worktree absolute path, for ref-resolvability
```

Add to `RecallResult`:

```go
	DanglingRefs []Ref // code refs absent from the current worktree (nil if none)
```

- [ ] **Step 4: Create resolve.go**

```go
// internal/memory/resolve.go
package memory

import (
	"context"
	"fmt"
	"strings"
)

// RefResolver reports which of a memory's code refs point at code NOT present in
// a worktree. The graph-backed implementation lives in internal/app and queries
// the code graph (files/nodes) in the shared store; tests inject a fake. A nil
// resolver, or one whose worktree has no graph rows, disables the check so recall
// behaves exactly as before (graceful degradation, spec §5.5).
type RefResolver interface {
	// Dangling returns the subset of refs whose referenced code is absent from
	// worktree. Implementations MUST return nil (nothing dangles) when worktree
	// has no graph rows. tag refs are not code and are never returned.
	Dangling(ctx context.Context, worktree string, refs []Ref) ([]Ref, error)
}

// SetRefResolver installs the resolvability backend (called once during wiring).
func (e *Engine) SetRefResolver(r RefResolver) { e.res = r }

// codeRefsOf returns only file/symbol refs (tags are not code).
func codeRefsOf(refs []Ref) []Ref {
	var out []Ref
	for _, r := range refs {
		if r.Type == RefFile || r.Type == RefSymbol {
			out = append(out, r)
		}
	}
	return out
}

// Caveat renders the dangling-reference warning for a recalled memory, or "" when
// nothing dangles.
func (r RecallResult) Caveat() string {
	if len(r.DanglingRefs) == 0 {
		return ""
	}
	vals := make([]string, 0, len(r.DanglingRefs))
	for _, d := range r.DanglingRefs {
		vals = append(vals, d.Value)
	}
	branch := r.Record.Branch
	if branch == "" {
		branch = "unknown"
	}
	return fmt.Sprintf("⚠ references code not present in this worktree (branch %s): %s — may not apply to the current branch",
		branch, strings.Join(vals, ", "))
}
```

- [ ] **Step 5: Add the resolver field to Engine**

In `internal/memory/engine.go`, add a field to the `Engine` struct (leave `New` untouched — `res` stays nil by default):

```go
type Engine struct {
	st  *store.Store
	led *ledger.Ledger
	clk clock.Clock
	lex Lexical
	res RefResolver // optional; nil → dangling-ref check disabled
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestCodeRefsOf|TestCaveat' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/memory/resolve.go internal/memory/recall.go internal/memory/engine.go internal/memory/resolve_test.go
git commit -m "feat(memory): RefResolver seam + dangling-ref caveat scaffolding"
```

---

## Task 2: Apply dangling-ref demotion + caveat in Recall

**Files:**
- Modify: `internal/memory/recall.go` (scoring loop)
- Test: `internal/memory/recall_resolve_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/memory/recall_resolve_test.go
package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func recordWithRef(t *testing.T, e *Engine, kind Kind, title string, ref Ref, pinned bool) {
	t.Helper()
	if _, _, err := e.Record(context.Background(), NewMemory{
		Kind: kind, Title: title, Body: title + " body about " + ref.Value,
		Refs: []Ref{ref}, Pinned: pinned,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecallDemotesAllDanglingNonPinned(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindLearning, "learn-present", Ref{RefFile, "present.go"}, false)
	recordWithRef(t, e, KindLearning, "learn-absent", Ref{RefFile, "absent.go"}, false)
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true}})

	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var present, absent RecallResult
	for _, r := range res {
		switch r.Record.Title {
		case "learn-present":
			present = r
		case "learn-absent":
			absent = r
		}
	}
	if present.Record == nil || absent.Record == nil {
		t.Fatalf("both memories should surface; got %d results", len(res))
	}
	if len(absent.DanglingRefs) != 1 {
		t.Fatalf("absent should have 1 dangling ref, got %d", len(absent.DanglingRefs))
	}
	if len(present.DanglingRefs) != 0 {
		t.Fatalf("present should have no dangling refs")
	}
	if absent.Score >= present.Score {
		t.Fatalf("absent (all refs dangling) should be demoted below present: %.4f vs %.4f", absent.Score, present.Score)
	}
}

func TestRecallCaveatsButDoesNotDemotePinnedOrBug(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	// A pinned do_not_repeat and a bug, both referencing absent code.
	recordWithRef(t, e, KindDoNotRepeat, "dnr-absent", Ref{RefFile, "absent.go"}, true)
	recordWithRef(t, e, KindBug, "bug-absent", Ref{RefFile, "absent.go"}, false)
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true}})

	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]RecallResult{}
	for _, r := range res {
		seen[r.Record.Title] = r
	}
	for _, title := range []string{"dnr-absent", "bug-absent"} {
		r, ok := seen[title]
		if !ok {
			t.Fatalf("%s should still surface", title)
		}
		if len(r.DanglingRefs) != 1 {
			t.Fatalf("%s should carry the dangling-ref caveat", title)
		}
	}
}

func TestRecallNoResolverIsInert(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindLearning, "learn-absent", Ref{RefFile, "absent.go"}, false)
	// No SetRefResolver call.
	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if len(r.DanglingRefs) != 0 {
			t.Fatalf("no resolver → no dangling refs, got %v", r.DanglingRefs)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestRecall -v`
Expected: FAIL — `absent` not demoted / `DanglingRefs` always empty (logic not wired yet).

- [ ] **Step 3: Wire the check into the scoring loop**

In `internal/memory/recall.go`, add the demotion constant near the other weights:

```go
	// danglingDemotion multiplies the score of a non-pinned, non-bug memory whose
	// every code ref is absent from the current worktree (spec §5.3).
	danglingDemotion = 0.25
```

Replace the tail of the scoring loop (the `scored = append(...)` line) with:

```go
		var dangling []Ref
		if e.res != nil && q.Worktree != "" {
			code := codeRefsOf(r.Refs)
			if len(code) > 0 {
				if d, derr := e.res.Dangling(ctx, q.Worktree, code); derr == nil && len(d) > 0 {
					dangling = d
					privileged := r.Pinned || r.Kind == KindBug
					if len(d) == len(code) && !privileged {
						score *= danglingDemotion
					}
				}
			}
		}
		scored = append(scored, RecallResult{Record: r, Score: score, Signals: sig, DanglingRefs: dangling})
```

(Resolver errors are best-effort: on `derr != nil` we leave `dangling` nil and behave as today.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestRecall -v`
Expected: PASS.

- [ ] **Step 5: Run the package + race**

Run: `go test ./internal/memory/... && go test -race ./internal/memory/...`
Expected: PASS (golden recall tests unaffected — resolver is nil there).

- [ ] **Step 6: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_resolve_test.go
git commit -m "feat(memory): demote + caveat memories whose code refs are absent from the worktree"
```

---

## Task 3: Graph-backed RefResolver + app wiring

**Files:**
- Create: `internal/app/refresolver.go`
- Modify: `internal/app/app.go` (call `eng.SetRefResolver` in `Open`)
- Test: `internal/app/refresolver_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/app/refresolver_test.go
package app

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func seedFile(t *testing.T, a *App, worktree, path string) {
	t.Helper()
	_, err := a.Store.DB.ExecContext(context.Background(),
		`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		path, worktree, path, "go", "h", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func seedNode(t *testing.T, a *App, fileID, name string) {
	t.Helper()
	_, err := a.Store.DB.ExecContext(context.Background(),
		`INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		name, fileID, "func", name, name, "", 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGraphResolverDangling(t *testing.T) {
	a := newTestApp(t, "main")
	wt := a.Git.Worktree
	seedFile(t, a, wt, "present.go")
	seedNode(t, a, "present.go", "Foo")
	r := graphRefResolver{st: a.Store}

	refs := []memory.Ref{
		{Type: memory.RefFile, Value: "present.go"},
		{Type: memory.RefFile, Value: "absent.go"},
		{Type: memory.RefSymbol, Value: "Foo"},
		{Type: memory.RefSymbol, Value: "Bar"},
	}
	d, err := r.Dangling(context.Background(), wt, refs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range d {
		got[x.Value] = true
	}
	if !got["absent.go"] || !got["Bar"] || got["present.go"] || got["Foo"] {
		t.Fatalf("unexpected dangling set: %v", got)
	}
}

func TestGraphResolverDegradesWhenNoGraph(t *testing.T) {
	a := newTestApp(t, "main")
	r := graphRefResolver{st: a.Store}
	d, err := r.Dangling(context.Background(), "/empty-worktree", []memory.Ref{
		{Type: memory.RefFile, Value: "absent.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 0 {
		t.Fatalf("no graph rows → nothing dangles, got %v", d)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestGraphResolver -v`
Expected: FAIL — `undefined: graphRefResolver`.

- [ ] **Step 3: Implement the resolver**

```go
// internal/app/refresolver.go
package app

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// graphRefResolver answers memory.RefResolver from the code graph (files/nodes)
// in the shared store, scoped to a worktree. When the worktree has no graph rows
// it reports nothing as dangling, so recall degrades to its pre-resolvability
// behavior (spec §5.5).
type graphRefResolver struct{ st *store.Store }

func (g graphRefResolver) Dangling(ctx context.Context, worktree string, refs []memory.Ref) ([]memory.Ref, error) {
	var n int
	if err := g.st.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM files WHERE worktree=?`, worktree).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil // ungraphed worktree → nothing dangles
	}
	var dangling []memory.Ref
	for _, r := range refs {
		present, err := g.present(ctx, worktree, r)
		if err != nil {
			return nil, err
		}
		if !present {
			dangling = append(dangling, r)
		}
	}
	return dangling, nil
}

func (g graphRefResolver) present(ctx context.Context, worktree string, r memory.Ref) (bool, error) {
	var q string
	var args []any
	switch r.Type {
	case memory.RefFile:
		q = `SELECT 1 FROM files WHERE worktree=? AND path=? LIMIT 1`
		args = []any{worktree, r.Value}
	case memory.RefSymbol:
		q = `SELECT 1 FROM nodes n JOIN files f ON n.file_id=f.id
		      WHERE f.worktree=? AND (n.name=? OR n.qualified_name=?) LIMIT 1`
		args = []any{worktree, r.Value, r.Value}
	default:
		return true, nil // not code
	}
	var x int
	err := g.st.DB.QueryRowContext(ctx, q, args...).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
```

- [ ] **Step 4: Wire it into Open**

In `internal/app/app.go` `Open`, immediately after the `gr, err := graph.New(...)` block (and its error check) and before `a := &App{...}`, add:

```go
	eng.SetRefResolver(graphRefResolver{st: st})
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/app/ -run TestGraphResolver -v`
Expected: PASS.

- [ ] **Step 6: Run package + race**

Run: `go test ./internal/app/... && go test -race ./internal/app/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/app/refresolver.go internal/app/app.go internal/app/refresolver_test.go
git commit -m "feat(app): graph-backed RefResolver wired into recall"
```

---

## Task 4: Surface caveats (MCP, CLI recall, session-start)

**Files:**
- Modify: `internal/mcp/handlers.go` (`MemoryView.Caveat`, set `Worktree`, set caveat)
- Modify: `internal/cli/recall.go` (pass `Worktree`, print caveat)
- Modify: `internal/agents/handle.go` (pass `Worktree` on the two recall calls)
- Modify: `internal/agents/format.go` (`formatSessionContext` prints caveats)
- Test: `internal/cli/recall_test.go` (add a caveat case), `internal/agents/format_test.go` (add a caveat case)

- [ ] **Step 1: Write the failing test (CLI)**

```go
// internal/cli/recall_test.go — add this test
func TestRecallPrintsCaveat(t *testing.T) {
	a := newTestApp(t, "feat/x")
	ctx := context.Background()
	// Seed a learning that references absent code; no graph rows → but force a
	// graph file for a DIFFERENT path so the worktree is "graphed" yet the ref dangles.
	if _, err := a.Store.DB.ExecContext(ctx,
		`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		 VALUES ('p',?,?,?,?,?,?,?)`, a.Git.Worktree, "present.go", "go", "h", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "ghost", Body: "ghost learning about absent.go",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Recall(ctx, a, &buf, []string{"ghost", "learning"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not present in this worktree") {
		t.Fatalf("expected caveat in output, got:\n%s", buf.String())
	}
}
```

(Add imports `bytes`, `strings`, `context`, and the `memory` package if not already present in the test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestRecallPrintsCaveat -v`
Expected: FAIL — no caveat printed (Worktree not passed / not rendered).

- [ ] **Step 3: CLI — pass Worktree and print the caveat**

In `internal/cli/recall.go`, add `Worktree: a.Git.Worktree,` to the `RecallQuery`. Then inside the results loop, after printing the body, add:

```go
		if c := r.Caveat(); c != "" {
			fmt.Fprintf(w, "    %s\n", c)
		}
```

- [ ] **Step 4: MCP — add the field and set it**

In `internal/mcp/handlers.go`, add to the `MemoryView` struct:

```go
	Caveat string `json:"caveat,omitempty"`
```

In `recallMemory`, add `Worktree: a.Git.Worktree,` to the `RecallQuery`, and in the results loop set the caveat:

```go
		v := viewOf(r.Record)
		v.Score = r.Score
		v.Caveat = r.Caveat()
		out.Memories = append(out.Memories, v)
```

- [ ] **Step 5: Session-start — pass Worktree and render caveats**

In `internal/agents/handle.go`, add `Worktree: a.Git.Worktree,` to BOTH `RecallQuery` literals (the baseline recall in `handleSessionStart` near line 73, and the bug recall in `handleAugment` near line 174).

In `internal/agents/format.go`, in `formatSessionContext`, where each recalled memory line is written, append its caveat when present. Add after the line that writes a memory entry:

```go
		if c := r.Caveat(); c != "" {
			fmt.Fprintf(&b, "  %s\n", c)
		}
```

(Match `b`/builder variable name and loop variable `r` already used in `formatSessionContext`.)

- [ ] **Step 6: Add the session-format test**

```go
// internal/agents/format_test.go — add this test
func TestFormatSessionContextIncludesCaveat(t *testing.T) {
	results := []memory.RecallResult{{
		Record:       &memory.Record{Kind: memory.KindLearning, Title: "ghost", Branch: "feat/x"},
		Score:        0.5,
		DanglingRefs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}}
	out := formatSessionContext(results, graph.Architecture{}, nil, 1500)
	if !strings.Contains(out, "not present in this worktree") {
		t.Fatalf("expected caveat in session context, got:\n%s", out)
	}
}
```

(Add imports as needed: `memory`, `graph`, `strings`, `testing`.)

- [ ] **Step 7: Run tests**

Run: `go test ./internal/cli/ ./internal/mcp/ ./internal/agents/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/handlers.go internal/cli/recall.go internal/agents/handle.go internal/agents/format.go internal/cli/recall_test.go internal/agents/format_test.go
git commit -m "feat(surfaces): render dangling-ref caveats in MCP, CLI recall, and session-start"
```

---

## Task 5: Merge core — Engine.ImportMemory (dedup-aware import)

Reuses `findNearDup`, `insert`, and `reinforce` so there is no second dedup path.

**Files:**
- Create: `internal/memory/importmem.go`
- Test: `internal/memory/importmem_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/memory/importmem_test.go
package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestImportMemoryInsertReinforceSkip(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()
	im := ImportMemory{
		ID: "snap-1", Kind: KindLearning, Title: "t", Body: "a durable learning about caching layers",
		Branch: "main", Author: "alice", Source: "snapshot", Confidence: 0.8,
		ReinforcementCount: 3, CreatedAt: 1000,
		Refs: []Ref{{Type: RefFile, Value: "cache.go"}},
	}

	// First import → inserted, metadata preserved.
	out, id, err := e.ImportMemory(ctx, im)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportInserted || id != "snap-1" {
		t.Fatalf("want inserted snap-1, got %v %s", out, id)
	}
	rec, _ := e.GetByID(ctx, "snap-1")
	if rec == nil || rec.CreatedAt != 1000 || rec.ReinforcementCount != 3 || rec.Confidence != 0.8 {
		t.Fatalf("metadata not preserved: %+v", rec)
	}

	// Re-import identical id → skipped (idempotent), count unchanged.
	out, _, err = e.ImportMemory(ctx, im)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportSkipped {
		t.Fatalf("re-import same id should skip, got %v", out)
	}

	// A near-duplicate body under a DIFFERENT id → reinforce existing.
	dup := im
	dup.ID = "snap-2"
	dup.Body = "a durable learning about caching layers!"
	out, _, err = e.ImportMemory(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportReinforced {
		t.Fatalf("near-dup should reinforce, got %v", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/memory/ -run TestImportMemory -v`
Expected: FAIL — `undefined: ImportMemory` / `ImportInserted`.

- [ ] **Step 3: Implement ImportMemory**

```go
// internal/memory/importmem.go
package memory

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// ImportMemory is a portable memory record from a team snapshot (spec §7).
type ImportMemory struct {
	ID                 string
	Kind               Kind
	Title              string
	Body               string
	Refs               []Ref
	Branch             string
	Author             string
	Source             string
	Confidence         float64
	ReinforcementCount int
	Pinned             bool
	CreatedAt          int64
	Extra              map[string]any
}

// ImportOutcome is the result of merging one snapshot record.
type ImportOutcome int

const (
	ImportSkipped   ImportOutcome = iota // an id already present locally (idempotent re-import)
	ImportInserted                       // new memory, snapshot metadata preserved
	ImportReinforced                     // near-duplicate of an existing memory → reinforced
)

// ImportMemory merges one snapshot record into the store, reusing the same
// MinHash near-dup path as Record. Identity is the snapshot id (idempotent
// re-import); a different id whose body is a near-duplicate reinforces the
// existing memory so independently-authored copies converge (spec §6/§7.4).
func (e *Engine) ImportMemory(ctx context.Context, im ImportMemory) (ImportOutcome, string, error) {
	if !im.Kind.Valid() {
		return ImportSkipped, "", errInvalidKind
	}
	nm := NewMemory{
		Kind: im.Kind, Title: im.Title, Body: im.Body, Refs: im.Refs,
		Branch: im.Branch, Author: im.Author, Source: im.Source,
		Pinned: im.Pinned, Extra: im.Extra,
	}
	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	bands := bandHashes(minHashSignature(set))
	sim := simHash64(set)

	outcome := ImportSkipped
	retID := im.ID
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		// Already imported under this id → no-op (true idempotency).
		var exists int
		if err := c.QueryRowContext(ctx,
			`SELECT 1 FROM memories WHERE id=? LIMIT 1`, im.ID).Scan(&exists); err == nil {
			outcome = ImportSkipped
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		candID, found, ferr := e.findNearDup(ctx, c, im.Kind, bands, set)
		if ferr != nil {
			return ferr
		}
		if found {
			outcome, retID = ImportReinforced, candID
			return e.reinforce(ctx, c, candID, nm, im.CreatedAt)
		}

		// Insert preserving the snapshot id + created_at, then restore the
		// snapshot's confidence/reinforcement_count (insert hardcodes 1.0/0).
		if err := e.insert(ctx, c, im.ID, nm, searchText, sim, bands, im.CreatedAt); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET confidence=?, reinforcement_count=? WHERE id=?`,
			clampConfidence(im.Confidence), im.ReinforcementCount, im.ID); err != nil {
			return err
		}
		outcome = ImportInserted
		return nil
	})
	if err != nil {
		return ImportSkipped, "", err
	}
	return outcome, retID, nil
}

func clampConfidence(c float64) float64 {
	if c <= 0 {
		return 1.0 // missing/zero → default
	}
	if c > 1.0 {
		return 1.0
	}
	return c
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/memory/ -run TestImportMemory -v`
Expected: PASS.

- [ ] **Step 5: Run package + race**

Run: `go test ./internal/memory/... && go test -race ./internal/memory/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/importmem.go internal/memory/importmem_test.go
git commit -m "feat(memory): ImportMemory merge core (insert/reinforce/skip) reusing MinHash dedup"
```

---

## Task 6: Snapshot package — Export + Import

**Files:**
- Create: `internal/snapshot/snapshot.go`
- Test: `internal/snapshot/snapshot_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/snapshot_test.go
package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func newEngine(t *testing.T) *memory.Engine {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return memory.New(st, ledger.New(st), clock.Real{})
}

func TestExportThenImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t)
	if _, _, err := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "L", Body: "learned about retries and backoff",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "retry.go"}},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := Export(ctx, src, &buf)
	if err != nil || n != 1 {
		t.Fatalf("export: n=%d err=%v", n, err)
	}
	if strings.Contains(buf.String(), "last_used_epoch") || strings.Contains(buf.String(), "recall_count") {
		t.Fatalf("snapshot must not contain local activity fields:\n%s", buf.String())
	}

	dst := newEngine(t)
	ins, rein, skip, err := Import(ctx, dst, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 || rein != 0 || skip != 0 {
		t.Fatalf("import counts: ins=%d rein=%d skip=%d", ins, rein, skip)
	}
	got, _ := dst.List(ctx, memory.ListFilter{})
	if len(got) != 1 || got[0].Title != "L" || len(got[0].Refs) != 1 {
		t.Fatalf("imported record wrong: %+v", got)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t)
	src.Record(ctx, memory.NewMemory{Kind: memory.KindDecision, Title: "D", Body: "chose stdlib flag over cobra"})
	var buf bytes.Buffer
	Export(ctx, src, &buf)
	data := buf.Bytes()

	dst := newEngine(t)
	Import(ctx, dst, bytes.NewReader(data))
	_, _, skip, err := Import(ctx, dst, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if skip != 1 {
		t.Fatalf("second import should skip the already-present record, got skip=%d", skip)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/snapshot/ -v`
Expected: FAIL — package/`Export`/`Import` undefined.

- [ ] **Step 3: Implement the snapshot package**

```go
// internal/snapshot/snapshot.go

// Package snapshot serializes active memories to a committable JSONL file
// (.recarto/memory.jsonl) and imports them back through the memory engine's
// dedup-aware ImportMemory. Local activity signals (last_used_epoch, recall_count,
// archived_epoch) and derived indexes (FTS/MinHash) are intentionally excluded:
// the former leak usage and churn, the latter are recomputed on import (spec §7).
package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// line is the portable wire shape of one memory.
type line struct {
	ID                 string          `json:"id"`
	Kind               string          `json:"kind"`
	Title              string          `json:"title"`
	Body               string          `json:"body"`
	Refs               []refLine       `json:"refs,omitempty"`
	Branch             string          `json:"branch,omitempty"`
	Author             string          `json:"author,omitempty"`
	Source             string          `json:"source,omitempty"`
	Confidence         float64         `json:"confidence"`
	ReinforcementCount int             `json:"reinforcement_count"`
	Pinned             bool            `json:"pinned,omitempty"`
	CreatedAt          int64           `json:"created_at"`
	Extra              map[string]any  `json:"extra,omitempty"`
}

type refLine struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Export writes all active memories (pinned included) as id-sorted JSONL and
// returns the count. Deterministic ordering keeps git diffs minimal.
func Export(ctx context.Context, eng *memory.Engine, w io.Writer) (int, error) {
	recs, err := eng.List(ctx, memory.ListFilter{Status: "active"})
	if err != nil {
		return 0, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, r := range recs {
		l := line{
			ID: r.ID, Kind: string(r.Kind), Title: r.Title, Body: r.Body,
			Branch: r.Branch, Author: r.Author, Source: r.Source,
			Confidence: r.Confidence, ReinforcementCount: r.ReinforcementCount,
			Pinned: r.Pinned, CreatedAt: r.CreatedAt, Extra: r.Extra,
		}
		for _, ref := range r.Refs {
			l.Refs = append(l.Refs, refLine{Type: string(ref.Type), Value: ref.Value})
		}
		if err := enc.Encode(&l); err != nil {
			return 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return len(recs), nil
}

// Import reads JSONL and merges each record via ImportMemory, returning the
// per-outcome tallies.
func Import(ctx context.Context, eng *memory.Engine, r io.Reader) (inserted, reinforced, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow large memory bodies
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l line
		if uerr := json.Unmarshal(raw, &l); uerr != nil {
			return inserted, reinforced, skipped, uerr
		}
		im := memory.ImportMemory{
			ID: l.ID, Kind: memory.Kind(l.Kind), Title: l.Title, Body: l.Body,
			Branch: l.Branch, Author: l.Author, Source: l.Source,
			Confidence: l.Confidence, ReinforcementCount: l.ReinforcementCount,
			Pinned: l.Pinned, CreatedAt: l.CreatedAt, Extra: l.Extra,
		}
		for _, ref := range l.Refs {
			im.Refs = append(im.Refs, memory.Ref{Type: memory.RefType(ref.Type), Value: ref.Value})
		}
		out, _, ierr := eng.ImportMemory(ctx, im)
		if ierr != nil {
			return inserted, reinforced, skipped, ierr
		}
		switch out {
		case memory.ImportInserted:
			inserted++
		case memory.ImportReinforced:
			reinforced++
		case memory.ImportSkipped:
			skipped++
		}
	}
	return inserted, reinforced, skipped, sc.Err()
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/snapshot/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/snapshot/snapshot.go internal/snapshot/snapshot_test.go
git commit -m "feat(snapshot): JSONL export + dedup-aware import of team memories"
```

---

## Task 7: CLI `recarto snapshot export|import`

**Files:**
- Create: `internal/cli/snapshot.go`
- Modify: `internal/cli/cli.go` (`known()` + dispatch switch)
- Test: `internal/cli/snapshot_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/cli/snapshot_test.go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestSnapshotExportThenImport(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, "main")
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "L", Body: "shareable learning about queues",
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Snapshot(ctx, a, &buf, []string{"export"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Git.Worktree, ".recarto", "memory.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not written: %v", err)
	}
	if !strings.Contains(buf.String(), "1") {
		t.Fatalf("export should report a count, got: %s", buf.String())
	}
	// Import is a no-op idempotent re-merge on the same store.
	if err := Snapshot(ctx, a, &buf, []string{"import"}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestSnapshotExport -v`
Expected: FAIL — `undefined: Snapshot`.

- [ ] **Step 3: Implement the CLI command**

```go
// internal/cli/snapshot.go
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/snapshot"
)

// Snapshot exports or imports the committable team-memory file.
//
//	recarto snapshot export   # write .recarto/memory.jsonl
//	recarto snapshot import   # merge .recarto/memory.jsonl into the local store
func Snapshot(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recarto snapshot export|import")
	}
	path := filepath.Join(a.Git.Worktree, ".recarto", "memory.jsonl")
	switch args[0] {
	case "export":
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := snapshot.Export(ctx, a.Engine, f)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "exported %d memories to %s\n", n, path)
		return nil
	case "import":
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		ins, rein, skip, err := snapshot.Import(ctx, a.Engine, f)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "imported %s: %d new, %d reinforced, %d skipped\n", path, ins, rein, skip)
		return nil
	default:
		return fmt.Errorf("unknown snapshot subcommand %q (export|import)", args[0])
	}
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/cli.go`, add `"snapshot"` to the `known()` switch list (line 15), and add a case to the dispatch switch:

```go
	case "snapshot":
		return Snapshot(ctx, a, out, rest)
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/cli/ -run TestSnapshot -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/snapshot.go internal/cli/cli.go internal/cli/snapshot_test.go
git commit -m "feat(cli): recarto snapshot export|import"
```

---

## Task 8: Lazy snapshot import at session-start

Auto-merge `.recarto/memory.jsonl` when its content hash changed since last import (tracked in `meta.snapshot_hash`).

**Files:**
- Create: `internal/snapshot/sync.go`
- Modify: `internal/agents/handle.go` (`handleSessionStart` calls the sync, best-effort)
- Test: `internal/snapshot/sync_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/sync_test.go
package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func TestSyncImportsOnceThenSkipsUntilChanged(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := memory.New(st, ledger.New(st), clock.Real{})

	dir := t.TempDir()
	path := filepath.Join(dir, ".recarto", "memory.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"id":"x","kind":"learning","title":"T","body":"shared knowledge","confidence":1,"created_at":1}`+"\n"), 0o644)

	ins, err := SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 {
		t.Fatalf("first sync should import 1, got %d", ins)
	}
	// Unchanged file → no re-import.
	ins, err = SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 {
		t.Fatalf("unchanged file should not re-import, got %d", ins)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/snapshot/ -run TestSync -v`
Expected: FAIL — `undefined: SyncOnSessionStart`.

- [ ] **Step 3: Implement sync**

```go
// internal/snapshot/sync.go
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

const metaSnapshotHash = "snapshot_hash"

// SyncOnSessionStart imports <repoRoot>/.recarto/memory.jsonl when its content
// hash differs from meta.snapshot_hash, then records the new hash. It returns the
// number of newly-inserted memories. A missing file is not an error (returns 0).
// Best-effort: callers ignore the error so a malformed snapshot never blocks a
// session.
func SyncOnSessionStart(ctx context.Context, eng *memory.Engine, st *store.Store, repoRoot string) (int, error) {
	path := filepath.Join(repoRoot, ".recarto", "memory.jsonl")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	var prev string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key=?`, metaSnapshotHash).Scan(&prev); err == nil {
		if prev == hash {
			return 0, nil // unchanged since last import
		}
	}

	ins, _, _, ierr := Import(ctx, eng, bytesReader(data))
	if ierr != nil {
		return 0, ierr
	}
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		_, e := c.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaSnapshotHash, hash)
		return e
	}); err != nil {
		return ins, err
	}
	return ins, nil
}

// bytesReader avoids importing bytes in callers; small local helper.
func bytesReader(b []byte) *bytesRdr { return &bytesRdr{b: b} }

type bytesRdr struct {
	b []byte
	i int
}

func (r *bytesRdr) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, errEOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

var errEOF = errors.New("EOF")
```

> Note: if you prefer, replace `bytesReader`/`bytesRdr` with the stdlib `bytes.NewReader` (import `bytes`) — it is equivalent. The local helper exists only to keep the import list minimal; use whichever matches house style. Verify `io.EOF` semantics if you keep the local reader — simplest is to `import "bytes"` and call `bytes.NewReader(data)`.

- [ ] **Step 4: Prefer stdlib bytes.Reader (simplify)**

Replace the `bytesReader` helper and its usage with the stdlib reader to avoid a hand-rolled EOF:

```go
import "bytes"
// ...
ins, _, _, ierr := Import(ctx, eng, bytes.NewReader(data))
```

Delete the `bytesReader`, `bytesRdr`, and `errEOF` definitions.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/snapshot/ -run TestSync -v`
Expected: PASS.

- [ ] **Step 6: Call it from session-start (best-effort)**

In `internal/agents/handle.go` `handleSessionStart`, immediately after the graph reconcile line (`a.Graph.ReconcileIfStale(...)`) and before the compaction call, add:

```go
	// Absorb a teammate's committed memory snapshot when it changed (best-effort).
	_, _ = snapshot.SyncOnSessionStart(ctx, a.Engine, a.Store, a.Git.Worktree)
```

Add the import `"git.gerwim.nl/gerwim/recarto/internal/snapshot"` to `handle.go`.

- [ ] **Step 7: Run the agents package**

Run: `go test ./internal/agents/... -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/snapshot/sync.go internal/snapshot/sync_test.go internal/agents/handle.go
git commit -m "feat(snapshot): lazy session-start import on snapshot-hash change"
```

---

## Task 9: Full verification + plan close-out

**Files:** none (verification only)

- [ ] **Step 1: Format check**

Run: `gofmt -l internal/ cmd/`
Expected: empty output. If any file is listed, run `gofmt -w` on it and amend the relevant commit.

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: no findings.

- [ ] **Step 3: Full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 4: Race on touched packages**

Run: `go test -race ./internal/memory/... ./internal/app/... ./internal/agents/... ./internal/snapshot/... ./internal/cli/...`
Expected: PASS.

- [ ] **Step 5: Binary smoke**

Run:
```bash
go build -o /tmp/recarto ./cmd/recarto && cd $(mktemp -d) && git init -q && /tmp/recarto remember --kind learning "smoke test memory" && /tmp/recarto snapshot export && /tmp/recarto snapshot import && /tmp/recarto recall smoke
```
Expected: export reports 1 memory, import reports `0 new, … 1 skipped` (idempotent), recall prints the memory.

- [ ] **Step 6: Final commit (if any fixups)**

```bash
git add -A
git commit -m "chore: gofmt/vet/test green for worktree-aware recall + snapshot"
```

---

## Self-Review (completed during planning)

**Spec coverage:**
- §5 worktree-aware recall → Tasks 1–4 (interface, demotion+caveat, graph resolver, surfaces). Graceful degradation (§5.5) → Task 3 Step 3 (`count(files)==0 → nil`) + Task 2 (nil resolver inert).
- §6 shared merge core → Task 5 (`ImportMemory` reuses `findNearDup`/`insert`/`reinforce`).
- §7 team snapshot → Tasks 6–8 (export filter = active+pinned via `List`; field exclusions asserted; lazy import via `meta.snapshot_hash`).
- §8 surfaces → Task 4 (MCP/CLI/session-start) + Task 7 (CLI snapshot).
- §9 data model → no schema columns; new `meta.snapshot_hash` (Task 8).

**Deviations from spec (intentional, noted for review):**
1. **Resolver lives behind `memory.RefResolver` and is implemented in `internal/app`** (not a standalone `internal/graphref` package) — same layering outcome, fewer moving parts.
2. **The "merge core" is `Engine.ImportMemory` in the memory package, not a separate `internal/merge` package.** `findNearDup`/`insert`/`reinforce` are unexported Engine methods; reusing them in-package is the DRY choice and avoids duplicating the dedup. The snapshot package depends only on the public `ImportMemory`.
3. **Import outcomes are Inserted | Reinforced | Skipped** (spec §6 listed "Superseded"). Cross-machine supersede is dropped: exported records are active-only and superseded-by ids do not line up across stores. Flagged for the reviewer.

**Placeholder scan:** none — every code step is complete. (Task 8 deliberately shows a helper then replaces it with stdlib `bytes.NewReader` in Step 4; end state has no hand-rolled reader.)

**Type consistency:** `RefResolver.Dangling`, `RecallResult.DanglingRefs`, `RecallResult.Caveat()`, `ImportMemory`/`ImportOutcome`/`ImportInserted|Reinforced|Skipped`, `snapshot.Export`/`Import`/`SyncOnSessionStart`, `Snapshot` CLI — names match across all tasks.
