# `todo` Memory Kind Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a sixth durable memory kind, `todo`, for open work that surfaces (at session start and on code contact) until it is explicitly completed or decays.

**Architecture:** A new `kind='todo'` and `status='done'` on the existing `memories` table (CHECKs widened via a `writable_schema` migration). Open todos join session-start baseline recall and just-in-time code-contact surfacing (mirroring bugs); completion is one engine method exposed via an MCP tool and a CLI. Agent awareness propagates through the single shared `Instructions` constant and the shared MCP/handle.go cores — no per-host adapter edits.

**Tech Stack:** Go, modernc.org/sqlite (SQLite), Model Context Protocol Go SDK.

**Spec:** `docs/superpowers/specs/2026-06-05-recarto-todo-memory-kind-design.md`

**Conventions for every task:** Tests are white-box (`package memory` etc.) and use the existing helpers (`newTestEngine(t, clk)`, `mustExec`). Run the whole package's tests before committing. Commit messages follow the repo's `type(scope): subject` style and end with the `Co-Authored-By` trailer.

---

## Task 1: Register the `todo` kind

**Files:**
- Modify: `internal/memory/record.go:7-22`
- Test: `internal/memory/record_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create/append `internal/memory/record_test.go`:

```go
package memory

import "testing"

func TestKindTodoIsValid(t *testing.T) {
	if !KindTodo.Valid() {
		t.Fatal("KindTodo should be a valid kind")
	}
	if KindTodo != "todo" {
		t.Fatalf("KindTodo = %q, want \"todo\"", KindTodo)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestKindTodoIsValid -v`
Expected: FAIL — `undefined: KindTodo`.

- [ ] **Step 3: Add the constant and register it**

In `internal/memory/record.go`, add `KindTodo` to the const block and to `validKinds`:

```go
const (
	KindPreference  Kind = "preference"
	KindLearning    Kind = "learning"
	KindDecision    Kind = "decision"
	KindDoNotRepeat Kind = "do_not_repeat"
	KindBug         Kind = "bug"
	KindAction      Kind = "action"
	KindTodo        Kind = "todo"
)

var validKinds = map[Kind]bool{
	KindPreference: true, KindLearning: true, KindDecision: true,
	KindDoNotRepeat: true, KindBug: true, KindAction: true, KindTodo: true,
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestKindTodoIsValid -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/record.go internal/memory/record_test.go
git commit -m "feat(memory): register todo kind

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Migration v12 — widen the `kind` and `status` CHECKs

The `memories` table CHECK constraints must admit `'todo'` (kind) and `'done'` (status). SQLite cannot `ALTER` a CHECK, and a table rebuild is unsafe here (migrations run inside one `BEGIN IMMEDIATE`, so `PRAGMA foreign_keys=OFF` is a no-op, and dropping `memories` would cascade-delete `memory_refs`/`memory_minhash`). We edit the stored schema text in place with `writable_schema`, swapping only the two CHECK value-lists via `replace()` — this preserves the columns added by earlier `ALTER` migrations and is unique within the `memories` row. The edit bumps SQLite's schema cookie, so all connections reparse automatically.

This migration is **not** a graph-rebuilding migration, which breaks the assumption baked into `TestLatestGraphMigrationRebuildsIndex` (that the *last* migration wipes the graph). That test is updated to seed from the last graph migration (v11) instead of `schemaVersion-1`.

**Files:**
- Modify: `internal/store/migrate.go` (append to the `migrations` slice, after the v11 entry, before the closing `}`)
- Modify: `internal/store/migrate_test.go` (`TestLatestGraphMigrationRebuildsIndex`)
- Test: `internal/store/migrate_test.go` (new tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/migrate_test.go`:

```go
func TestMigrateV12AllowsTodoAndDone(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// New kind accepted.
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('t1','todo','do the thing','body',1,1)`)
	// New status accepted.
	mustExec(t, st, `UPDATE memories SET status='done' WHERE id='t1'`)

	// Old CHECKs still enforced: a bogus kind/status is rejected.
	if _, err := st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid kind")
	}
	if _, err := st.DB.Exec(`UPDATE memories SET status='bogus' WHERE id='t1'`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid status")
	}
}

func TestV12MigrationPreservesMemoriesAndColumns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('m1','learning','keep me','body',1,1)`)
	// A column added by an ALTER migration must survive the writable_schema edit.
	mustExec(t, st, `UPDATE memories SET last_used_epoch=7 WHERE id='m1'`)
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1))
	st.Close()

	st2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var epoch int
	if err := st2.DB.QueryRow(`SELECT last_used_epoch FROM memories WHERE id='m1'`).Scan(&epoch); err != nil {
		t.Fatalf("row/column lost after migration: %v", err)
	}
	if epoch != 7 {
		t.Errorf("last_used_epoch = %d, want 7", epoch)
	}
	// And the widened CHECK is live after the re-run.
	mustExec(t, st2, `INSERT INTO memories(id,kind,status,created_at,updated_at)
		VALUES('t2','todo','done',1,1)`)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestMigrateV12AllowsTodoAndDone|TestV12MigrationPreservesMemoriesAndColumns' -v`
Expected: FAIL — the `todo` insert is rejected by the current CHECK.

- [ ] **Step 3: Append the v12 migration**

In `internal/store/migrate.go`, add this entry as the final element of the `migrations` slice (immediately before the closing `}` on the line after the v11 block):

```go
	// v12 (todo kind): widen the memories CHECK constraints to admit kind='todo'
	// and status='done'. SQLite cannot ALTER a CHECK, and a table rebuild is unsafe
	// here (migrations run inside one BEGIN IMMEDIATE, so PRAGMA foreign_keys=OFF is
	// a no-op, and dropping memories would cascade-delete memory_refs/memory_minhash).
	// Instead edit the stored schema text via writable_schema, swapping ONLY the two
	// CHECK value-lists with replace() — this preserves columns appended by earlier
	// ALTER migrations and bumps the schema cookie so all connections reparse. The
	// `sql NOT LIKE` guards keep the edit idempotent (safe to re-run). This is NOT a
	// graph migration; the derived graph is left untouched.
	`
PRAGMA writable_schema=ON;
UPDATE sqlite_master
   SET sql = replace(sql,
     '''preference'',''learning'',''decision'',''do_not_repeat'',''bug'',''action''',
     '''preference'',''learning'',''decision'',''do_not_repeat'',''bug'',''action'',''todo''')
 WHERE type='table' AND name='memories' AND sql NOT LIKE '%''todo''%';
UPDATE sqlite_master
   SET sql = replace(sql,
     '''active'',''superseded'',''archived''',
     '''active'',''superseded'',''archived'',''done''')
 WHERE type='table' AND name='memories' AND sql NOT LIKE '%''done''%';
PRAGMA writable_schema=OFF;
`,
```

- [ ] **Step 4: Fix `TestLatestGraphMigrationRebuildsIndex` to seed from the last graph migration**

v12 is not a graph migration, so seeding `schemaVersion-1` would run only v12 and the graph would not be wiped. Seed from v11 (the last graph migration) instead. In `internal/store/migrate_test.go`, inside `TestLatestGraphMigrationRebuildsIndex`, replace:

```go
	// Pretend this store was written by the immediately-previous binary version.
	mustExec(t, st, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1))
```

with:

```go
	// v11 is the last graph-rebuilding migration; v12 (todo CHECK widening) is not.
	// Seed just before v11 so the reopen runs v11's graph wipe (and v12 after it).
	mustExec(t, st, `PRAGMA user_version = 10`)
```

- [ ] **Step 5: Run the store package tests**

Run: `go test ./internal/store/ -v`
Expected: PASS — including `TestMigrateIsIdempotent`, `TestLatestGraphMigrationRebuildsIndex`, and the two new tests.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): v12 migration widens memories CHECK for todo/done

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `Engine.CompleteTodo`

**Files:**
- Create: `internal/memory/todo.go`
- Test: `internal/memory/todo_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/memory/todo_test.go`:

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestCompleteTodoTransitionsActiveToDone(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id, _, err := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "wire the thing", Body: "wire the thing up", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.CompleteTodo(ctx, id)
	if err != nil || !ok {
		t.Fatalf("CompleteTodo = (%v, %v), want (true, nil)", ok, err)
	}
	rec, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "done" {
		t.Errorf("status = %q, want \"done\"", rec.Status)
	}

	// Idempotent / safe: completing a non-active (or unknown) todo reports false.
	ok, err = e.CompleteTodo(ctx, id)
	if err != nil || ok {
		t.Fatalf("second CompleteTodo = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = e.CompleteTodo(ctx, "no-such-id")
	if err != nil || ok {
		t.Fatalf("CompleteTodo(unknown) = (%v, %v), want (false, nil)", ok, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestCompleteTodoTransitionsActiveToDone -v`
Expected: FAIL — `e.CompleteTodo undefined`.

- [ ] **Step 3: Implement `CompleteTodo`**

Create `internal/memory/todo.go`:

```go
package memory

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// CompleteTodo marks an active todo as done (a terminal status, distinct from the
// 'archived' status decay produces, so finished work is distinguishable from faded
// work). It is safe to call repeatedly: it returns (false, nil) when no active
// todo with that id exists. The row is dropped from the lexical index so a
// completed todo stops surfacing as a recall candidate (recall also filters by
// status, but this keeps the candidate pool clean — mirrors supersede in insert).
func (e *Engine) CompleteTodo(ctx context.Context, id string) (bool, error) {
	var done bool
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		res, err := c.ExecContext(ctx,
			`UPDATE memories SET status='done', updated_at=?
			   WHERE id=? AND kind='todo' AND status='active'`,
			e.clk.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		done = true
		return e.lex.Delete(ctx, c, id)
	})
	return done, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestCompleteTodoTransitionsActiveToDone -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/todo.go internal/memory/todo_test.go
git commit -m "feat(memory): CompleteTodo transitions active todo to done

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Todo surfacing queries — `ActiveTodos` and `TodosForCode`

`ActiveTodos` powers `recarto todo list`; `TodosForCode` powers just-in-time code-contact surfacing. Both mirror the existing bug queries in `bugsurface.go` (reuse `selectMemoriesSQL`, `scanMemories`, `loadByIDs`).

**Files:**
- Modify: `internal/memory/todo.go`
- Test: `internal/memory/todo_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/memory/todo_test.go`:

```go
func TestActiveTodosExcludesDone(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	openID, _, _ := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "open one", Body: "open one body", Branch: "main", Source: "cli"})
	doneID, _, _ := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "done one", Body: "done one body", Branch: "main", Source: "cli"})
	if _, err := e.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	got, err := e.ActiveTodos(ctx, "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != openID {
		t.Fatalf("ActiveTodos returned %d rows; want only the open todo %s", len(got), openID)
	}
	_ = doneID
}

func TestTodosForCodeMatchesRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	hitID, _, _ := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "refactor handler", Body: "refactor the handler",
		Refs: []Ref{{Type: RefFile, Value: "internal/agents/handle.go"}}, Branch: "main", Source: "cli",
	})
	// Unrelated ref — must not surface.
	_, _, _ = e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "other", Body: "other body",
		Refs: []Ref{{Type: RefFile, Value: "internal/store/store.go"}}, Branch: "main", Source: "cli",
	})

	got, err := e.TodosForCode(ctx, "main", []string{"internal/agents/handle.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != hitID {
		t.Fatalf("TodosForCode returned %d rows; want only %s", len(got), hitID)
	}

	// A completed todo no longer surfaces on code contact.
	if _, err := e.CompleteTodo(ctx, hitID); err != nil {
		t.Fatal(err)
	}
	got, err = e.TodosForCode(ctx, "main", []string{"internal/agents/handle.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("completed todo still surfaced: %d rows", len(got))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'TestActiveTodos|TestTodosForCode' -v`
Expected: FAIL — `e.ActiveTodos` / `e.TodosForCode` undefined.

- [ ] **Step 3: Implement the two queries**

Append to `internal/memory/todo.go` (add `"sort"` to the import block):

```go
// ActiveTodos returns up to limit active, branch-visible todos, most recently
// updated first — the open-work list for `recarto todo list` and a tier the
// session-start baseline also draws on.
func (e *Engine) ActiveTodos(ctx context.Context, branch string, limit int) ([]*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		selectMemoriesSQL+`
		  WHERE status='active' AND kind='todo'
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// TodosForCode returns active kind=todo memories referencing any of the given
// files or symbols, branch-visible, most-recently-updated first. It powers
// just-in-time todo surfacing in the read/edit hooks: a todo anchored to a
// file/symbol reappears the moment that code is touched (mirrors BugsForCode).
func (e *Engine) TodosForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
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
			    AND m.kind='todo' AND m.status='active'
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestActiveTodos|TestTodosForCode' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/todo.go internal/memory/todo_test.go
git commit -m "feat(memory): ActiveTodos + TodosForCode surfacing queries

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Surface open todos in session-start baseline recall

Open todos should inject at session start alongside preferences/learnings/decisions. Only `baselineIDs` has a kind allowlist; task-hinted recall is lexical and already returns matching todos with no kind gate, so this one edit covers both spec surfacing requirements for the recall path.

**Files:**
- Modify: `internal/memory/recall.go:284-294` (`baselineIDs`)
- Test: `internal/memory/recall_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/memory/recall_test.go`:

```go
func TestBaselineRecallIncludesOpenTodo(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(5_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	todoID, _, err := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "finish the parser", Body: "finish the parser work", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	doneID, _, _ := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "already finished", Body: "already finished work", Branch: "main", Source: "cli",
	})
	if _, err := e.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	results, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var sawOpen, sawDone bool
	for _, r := range results {
		switch r.Record.ID {
		case todoID:
			sawOpen = true
		case doneID:
			sawDone = true
		}
	}
	if !sawOpen {
		t.Error("open todo missing from session-start baseline recall")
	}
	if sawDone {
		t.Error("done todo must not appear in baseline recall")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestBaselineRecallIncludesOpenTodo -v`
Expected: FAIL — the open todo is absent (kind not in the allowlist).

- [ ] **Step 3: Add `todo` to the baseline allowlist**

In `internal/memory/recall.go`, in `baselineIDs`, change the `kind IN` clause:

```go
		    AND kind IN ('preference','learning','decision','do_not_repeat','todo')
```

Also update the `baselineIDs` doc comment so it reflects that open todos are included:

```go
// baselineIDs returns active, branch-visible "cerebrum" memories — preference,
// learning, decision, (unpinned) do_not_repeat, and open todos — most-recently-
// updated first, capped at limit. It seeds an empty-query session-start recall so
// durable knowledge and open work surface without a task hint. Ephemeral kinds
// (action) and bugs are excluded: bugs surface just-in-time on code contact.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestBaselineRecallIncludesOpenTodo -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go
git commit -m "feat(memory): surface open todos in session-start baseline recall

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Decay — make `todo` evictable at the standard half-life

Untouched todos should fade like other decayable kinds. Add `KindTodo` to `evictable` with the standard 30-epoch half-life (the default `halfLifeEpochs` returns for non-action kinds, so only `evictable` needs editing).

**Files:**
- Modify: `internal/memory/compact.go:97-103` (`evictable`)
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/memory/compact_test.go`:

```go
func TestTodoIsEvictable(t *testing.T) {
	if !evictable(KindTodo) {
		t.Error("todo should be evictable so untouched todos decay")
	}
	if got := halfLifeEpochs(KindTodo); got != halfLifeStdEpochs {
		t.Errorf("todo half-life = %v, want standard %v", got, halfLifeStdEpochs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestTodoIsEvictable -v`
Expected: FAIL — `evictable(KindTodo)` is false.

- [ ] **Step 3: Add `KindTodo` to `evictable`**

In `internal/memory/compact.go`, update `evictable` (and its comment):

```go
// evictable reports whether a kind may be archived by decay. do_not_repeat is
// pinned; decision and bug carry durable rationale/fix history and are exempt
// (they can still be deduped). action/preference/learning decay, and so do todos:
// an untouched todo fades (reinforcing it via a near-dup record refreshes it).
func evictable(k Kind) bool {
	switch k {
	case KindAction, KindPreference, KindLearning, KindTodo:
		return true
	}
	return false
}
```

(`halfLifeEpochs` needs no change: it already returns `halfLifeStdEpochs` for every non-action kind, including `todo`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestTodoIsEvictable -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): decay untouched todos at the standard half-life

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: MCP `complete_todo` tool

**Files:**
- Modify: `internal/mcp/handlers.go` (add input/output types + handler)
- Modify: `internal/mcp/server.go` (register the tool; update `record_memory` description)
- Test: `internal/mcp/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/mcp/handlers_test.go` (follow the file's existing app/engine test setup; if it has a helper like `newTestApp(t)`, reuse it — otherwise mirror the setup used by the existing `recordMemory`/`getMemory` tests in that file):

```go
func TestCompleteTodoHandler(t *testing.T) {
	a := newTestApp(t) // reuse the existing handlers_test app helper
	ctx := context.Background()

	id, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "ship it", Body: "ship the feature", Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := completeTodo(ctx, a, CompleteTodoInput{ID: id})
	if err != nil || !out.Completed {
		t.Fatalf("completeTodo = (%+v, %v), want Completed=true", out, err)
	}
	rec, _ := a.Engine.GetByID(ctx, id)
	if rec.Status != "done" {
		t.Errorf("status = %q, want \"done\"", rec.Status)
	}

	out, err = completeTodo(ctx, a, CompleteTodoInput{ID: "missing"})
	if err != nil || out.Completed {
		t.Fatalf("completeTodo(missing) = (%+v, %v), want Completed=false", out, err)
	}
}
```

> If `handlers_test.go` has no `newTestApp` helper, check how the existing tests in that file construct `*app.App` and copy that exact construction inline.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestCompleteTodoHandler -v`
Expected: FAIL — `completeTodo` / `CompleteTodoInput` undefined.

- [ ] **Step 3: Add the handler**

Append to `internal/mcp/handlers.go`:

```go
// CompleteTodoInput is the argument to the complete_todo tool.
type CompleteTodoInput struct {
	ID string `json:"id" jsonschema:"id of the todo memory to mark done"`
}

// CompleteTodoOutput reports whether an active todo was transitioned to done.
type CompleteTodoOutput struct {
	Completed bool `json:"completed"`
}

func completeTodo(ctx context.Context, a *app.App, in CompleteTodoInput) (CompleteTodoOutput, error) {
	ok, err := a.Engine.CompleteTodo(ctx, in.ID)
	if err != nil {
		return CompleteTodoOutput{}, err
	}
	return CompleteTodoOutput{Completed: ok}, nil
}
```

- [ ] **Step 4: Register the tool and refresh the record_memory description**

In `internal/mcp/server.go`, update the `record_memory` description to list `todo`, and register `complete_todo`:

```go
	addTool(s, a, "record_memory",
		"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, action, or todo (open work that resurfaces until done).",
		recordMemory)
```

Add, alongside the other `addTool` calls:

```go
	addTool(s, a, "complete_todo",
		"Mark a todo memory done (active → done) so it stops surfacing. No-op if the id is not an active todo.",
		completeTodo)
```

- [ ] **Step 5: Run the mcp package tests**

Run: `go test ./internal/mcp/ -v`
Expected: PASS — including `TestCompleteTodoHandler`.

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/server.go internal/mcp/handlers_test.go
git commit -m "feat(mcp): complete_todo tool; list todo in record_memory desc

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: CLI `recarto todo done|list`

**Files:**
- Create: `internal/cli/todo.go`
- Modify: `internal/cli/cli.go` (dispatch + `known()` set + usage)
- Modify: `internal/cli/remember.go:19` (kind-list error string)
- Test: `internal/cli/todo_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/cli/todo_test.go` (mirror the construction used by existing CLI tests such as `memory_test.go` for building `*app.App` and capturing output; reuse that file's helper if present):

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestTodoListAndDone(t *testing.T) {
	a := newTestCLIApp(t) // reuse the helper the other cli tests use
	ctx := context.Background()

	id, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "write docs", Body: "write the docs", Branch: a.Git.Branch, Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := Todo(ctx, a, &list, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), "write docs") || !strings.Contains(list.String(), id) {
		t.Fatalf("todo list missing the open todo:\n%s", list.String())
	}

	var done bytes.Buffer
	if err := Todo(ctx, a, &done, []string{"done", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(done.String(), "done") {
		t.Fatalf("todo done output unexpected:\n%s", done.String())
	}

	var list2 bytes.Buffer
	if err := Todo(ctx, a, &list2, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list2.String(), id) {
		t.Fatalf("completed todo still listed:\n%s", list2.String())
	}
}
```

> If the CLI tests use a different app-helper name, substitute it. Check `internal/cli/memory_test.go` for the exact constructor and copy it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestTodoListAndDone -v`
Expected: FAIL — `Todo` undefined.

- [ ] **Step 3: Implement the `todo` command**

Create `internal/cli/todo.go`:

```go
package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// todoListLimit caps `recarto todo list` output.
const todoListLimit = 50

// Todo dispatches `todo list` and `todo done <id>`.
//
//	recarto todo list            # list active todos for the current branch
//	recarto todo done <id>       # mark a todo done
func Todo(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("todo: subcommand required (list|done)")
	}
	switch args[0] {
	case "list":
		return todoList(ctx, a, w)
	case "done":
		return todoDone(ctx, a, w, args[1:])
	default:
		return fmt.Errorf("todo: unknown subcommand %q (list|done)", args[0])
	}
}

func todoList(ctx context.Context, a *app.App, w io.Writer) error {
	todos, err := a.Engine.ActiveTodos(ctx, a.Git.Branch, todoListLimit)
	if err != nil {
		return err
	}
	if len(todos) == 0 {
		fmt.Fprintln(w, "(no open todos)")
		return nil
	}
	for _, r := range todos {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• %s  %s\n", r.ID, title)
	}
	return nil
}

func todoDone(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("todo done: id required")
	}
	ok, err := a.Engine.CompleteTodo(ctx, args[0])
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(w, "no active todo with id %s\n", args[0])
		return nil
	}
	fmt.Fprintf(w, "done %s\n", args[0])
	return nil
}
```

- [ ] **Step 4: Wire dispatch and usage**

In `internal/cli/cli.go`:
- Add `"todo"` to the `known()` command set (the `case "status", ..., "snapshot":` list on line 15).
- Add a dispatch case in the `switch cmd` (line 60+), next to the others:

```go
	case "todo":
		return Todo(ctx, a, w, rest)
```

  (Use the same args variable the neighboring cases pass — match how `case "snapshot":` calls its handler.)
- Add `"todo"` to the unknown-command hint string (line 45) and add a usage line near the `snapshot` usage line (line 115):

```go
	fmt.Fprintln(w, "  todo list|done    list open todos, or mark one done")
```

Also in `internal/cli/remember.go:19`, extend the kind list in the error string:

```go
		return fmt.Errorf("remember: kind required (preference|learning|decision|do_not_repeat|bug|action|todo)")
```

- [ ] **Step 5: Run the cli package tests**

Run: `go test ./internal/cli/ -v`
Expected: PASS — including `TestTodoListAndDone`.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/todo.go internal/cli/cli.go internal/cli/remember.go internal/cli/todo_test.go
git commit -m "feat(cli): recarto todo list|done

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Just-in-time todo surfacing on code contact

A todo anchored to a file resurfaces when that file is read or edited, mirroring bug surfacing in `handlePostRead` / `handlePostEdit`.

**Files:**
- Modify: `internal/agents/format.go` (add `formatTodoReminders`)
- Modify: `internal/agents/handle.go` (`handlePostRead` ~line 118, `handlePostEdit` ~line 145)
- Test: `internal/agents/format_test.go`, `internal/agents/handle_test.go` (or the existing handle/hook test file)

- [ ] **Step 1: Write the failing tests**

Append to `internal/agents/format_test.go`:

```go
func TestFormatTodoReminders(t *testing.T) {
	out := formatTodoReminders("internal/agents/handle.go", []*memory.Record{
		{Title: "refactor the dispatch"},
	})
	if !strings.Contains(out, "internal/agents/handle.go") || !strings.Contains(out, "refactor the dispatch") {
		t.Fatalf("unexpected reminder text: %q", out)
	}
}
```

Append a handle-level test to the file where `handlePostRead` is exercised (e.g. `internal/agents/handle_test.go`; mirror an existing PostRead bug-surfacing test there for app/event setup):

```go
func TestPostReadSurfacesTodo(t *testing.T) {
	a := newTestAgentApp(t) // reuse the helper used by the existing handlePostRead tests
	ctx := context.Background()

	rel := "internal/agents/handle.go"
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "split this file", Body: "split handle.go",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: rel}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := handlePostRead(ctx, a, Event{FilePath: filepath.Join(a.Git.Worktree, rel), SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "split this file") {
		t.Fatalf("todo not surfaced on read; got: %q", res.Context)
	}
}
```

> Match the actual helper/event-construction used by the neighboring bug-surfacing tests in that file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agents/ -run 'TestFormatTodoReminders|TestPostReadSurfacesTodo' -v`
Expected: FAIL — `formatTodoReminders` undefined and no todo in read context.

- [ ] **Step 3: Add the formatter**

Append to `internal/agents/format.go`:

```go
// formatTodoReminders renders open todos anchored to loc as one-line reminders,
// the just-in-time counterpart to formatBugWarnings for the todo kind.
func formatTodoReminders(loc string, todos []*memory.Record) string {
	lines := make([]string, 0, len(todos))
	for _, r := range todos {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		lines = append(lines, fmt.Sprintf("📝 open todo for %s: %s", loc, title))
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: Wire it into the read and edit hooks**

In `internal/agents/handle.go`, in `handlePostRead`, immediately after the `BugsForCode` block (~line 118–120) add:

```go
	if found, err := a.Engine.TodosForCode(ctx, a.Git.Branch, []string{rel}, nil); err == nil && len(found) > 0 {
		msgs = append(msgs, formatTodoReminders(rel, found))
	}
```

In `handlePostEdit`, immediately after the `BugsForCode` block (~line 145–147) add (appending to `res.Context` so it composes with any bug text):

```go
	if found, berr := a.Engine.TodosForCode(ctx, a.Git.Branch, []string{rel}, nil); berr == nil && len(found) > 0 {
		if res.Context != "" {
			res.Context += "\n"
		}
		res.Context += formatTodoReminders(rel, found)
	}
```

- [ ] **Step 5: Run the agents package tests**

Run: `go test ./internal/agents/ -v`
Expected: PASS — including the two new tests.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/format.go internal/agents/handle.go internal/agents/format_test.go internal/agents/handle_test.go
git commit -m "feat(agents): surface open todos on read/edit code contact

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: Agent awareness across hosts (shared `Instructions`)

One edit to the shared `Instructions` constant tells every host (Claude, opencode, Copilot) about the todo kind and the complete tool, via the installer's `WriteInstructionsBlock`.

**Files:**
- Modify: `internal/agents/instructions.go:18-33`
- Test: `internal/agents/instructions_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create/append `internal/agents/instructions_test.go`:

```go
package agents

import (
	"strings"
	"testing"
)

func TestInstructionsMentionTodo(t *testing.T) {
	for _, want := range []string{"todo", "complete_todo"} {
		if !strings.Contains(Instructions, want) {
			t.Errorf("Instructions block should mention %q so agents know todos exist", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run TestInstructionsMentionTodo -v`
Expected: FAIL — `Instructions` does not mention todos.

- [ ] **Step 3: Add a todo line to the Instructions constant**

In `internal/agents/instructions.go`, add a bullet to the `Instructions` constant (inside the existing backtick block, after the `Recall` bullet on line 26):

```go
- **Track open work** as a ` + "`todo`" + ` memory (` + "`record_memory`" + ` with kind ` + "`todo`" + `): it
  resurfaces at session start and when you touch its referenced files. Close it with
  ` + "`complete_todo`" + ` (or ` + "`recarto todo done <id>`" + `) when the work is finished.
```

- [ ] **Step 4: Run the agents package tests**

Run: `go test ./internal/agents/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/instructions.go internal/agents/instructions_test.go
git commit -m "feat(agents): document the todo kind in the shared instructions block

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: Confirm snapshot export/import handles todos with no code change

The spec asserts snapshot export/import needs no kind-specific change (it exports only active rows and dedups by content). Lock that with a test rather than new code.

**Files:**
- Test: `internal/snapshot/snapshot_test.go` (append; mirror the existing export/import test setup)

- [ ] **Step 1: Write the test**

Append to `internal/snapshot/snapshot_test.go` (reuse the file's existing engine/round-trip helpers — match how the current export/import tests build the engine and call `Export`/`Import`):

```go
func TestSnapshotRoundTripsActiveTodoNotDone(t *testing.T) {
	// Source store with one open and one completed todo.
	src := newTestEngine(t) // use this file's existing helper
	ctx := context.Background()

	openID, _, err := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "open work", Body: "open work body", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	doneID, _, _ := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "finished work", Body: "finished work body", Branch: "main", Source: "cli",
	})
	if _, err := src.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Export(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), doneID) {
		t.Error("export should not include a done todo (only active rows export)")
	}

	dst := newTestEngine(t)
	if _, _, err := Import(ctx, dst, &buf); err != nil {
		t.Fatal(err)
	}
	got, err := dst.ActiveTodos(ctx, "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "open work" {
		t.Fatalf("expected the open todo to round-trip; got %d rows", len(got))
	}
	_ = openID
}
```

> Match `Export`/`Import` signatures and the engine helper to the existing tests in this file. If `Export` returns a single count, adjust the assignment accordingly.

- [ ] **Step 2: Run the snapshot tests**

Run: `go test ./internal/snapshot/ -v`
Expected: PASS. If the done todo *does* appear in the export, that means snapshot export is not status-filtered — STOP and report; the spec assumed only active rows export, and that assumption would need revisiting before proceeding.

- [ ] **Step 3: Commit**

```bash
git add internal/snapshot/snapshot_test.go
git commit -m "test(snapshot): active todos round-trip; done todos excluded

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 12: Full build + test sweep

- [ ] **Step 1: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output, exit 0.

- [ ] **Step 2: Run the whole test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 3: Manual smoke test of the CLI**

Run:
```bash
go run ./cmd/recarto remember todo --title "smoke" --body "smoke test todo"
go run ./cmd/recarto todo list
```
Expected: the second command lists the `smoke` todo with its id. Then:
```bash
go run ./cmd/recarto todo done <id-from-list>
go run ./cmd/recarto todo list
```
Expected: `done <id>`, then `(no open todos)` (or the list without that id).

- [ ] **Step 4: Commit any final fixes**

If steps 1–3 required changes, commit them:

```bash
git add -A
git commit -m "chore(todo): build/test sweep fixes

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Notes for the implementer

- **Test helpers vary per package.** Several tasks say "reuse the existing helper" (`newTestApp`, `newTestCLIApp`, `newTestAgentApp`, the snapshot engine helper). These names are placeholders — open the neighboring `_test.go` file first and copy the exact constructor/event setup the existing tests use. Do not invent a helper.
- **Migration safety.** The v12 `replace()` targets are the exact CHECK value-lists as written in the v1 `CREATE TABLE memories` (single-quoted, comma-separated, no spaces). They are unique within the `memories` row. The `sql NOT LIKE` guards make the migration idempotent.
- **No status type.** Statuses are raw string literals throughout the codebase (`'active'`, `'archived'`, …); this plan keeps that convention and uses the literal `"done"`.
