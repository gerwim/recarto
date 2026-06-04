# Receiver-Aware Call Resolution — Phase 2 (Resolver + Candidate Sets) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Resolve `x.Method()` to the correct `Type.Method` using the Phase 1 receiver/type-binding data, recording results as a confidence-ranked candidate set in `edge_candidates` (plus a denormalized top `dst_node_id`).

**Architecture:** Rewrite `Engine.Resolve` so it (a) still resolves non-call edges by name (unchanged behavior for extends/imports/references), and (b) for `calls` edges runs five set-based candidate strategies in priority order into `edge_candidates`, then denormalizes the best candidate to `edges.dst_node_id`. All strategies stay scoped to the language family. Trace/MCP consumption is **Phase 3** — this phase keeps `trace.go` working via the denormalized `dst_node_id`.

**Tech Stack:** Go, SQLite. Spec §5: `docs/superpowers/specs/2026-06-04-receiver-aware-call-resolution-design.md`. Builds on Phase 1 (schema v9, `edges.dst_recv`, `type_bindings`, `edge_candidates`).

**Key design fact:** a binding is in scope at a call iff `binding.scope ⊇ the calling method's span`. The calling method is `edges.src_node_id`; its `start_byte/end_byte` live on `nodes`. So scope matching needs no per-call byte — the strategies are set-based `INSERT ... SELECT`.

**Confidence bands:** high ≥0.8, medium 0.4–0.8, low <0.4. S1=0.95 self, S2=0.9 typed, S3=0.9 static, S4=0.5 iface, S5=0.2 name.

---

## File structure (this phase)

| File | Responsibility | Action |
|---|---|---|
| `internal/graph/resolve.go` | Rewrite `Resolve`: non-call name resolution + 5 call strategies → `edge_candidates` + top `dst_node_id` | Modify |
| `internal/graph/resolve_test.go` | Existing tests stay green; add candidate-strategy tests | Modify |
| `internal/graph/candidates_test.go` | New: one test per strategy (S1–S5), C# + Go | Create |

`trace.go`, `mcp/graph.go` are **not** changed here (Phase 3). The existing `TestResolveLinksCallEdges`, `TestResolvePrefersSameFile`, `TestResolvePrefersSameTopLevelDir`, `TestResolveDoesNotCrossLanguageFamily`, `TestResolveWithinWebFamily`, `TestTraceCallPath*` must stay green (the denormalized `dst_node_id` preserves their contract).

---

## Shared SQL fragments

**Language-family predicate** (reused; `ef` = edge's file, `mf` = match's file):
```
( ef.language = mf.language
  OR ( ef.language IN ('typescript','tsx','javascript')
       AND mf.language IN ('typescript','tsx','javascript') ) )
```

**Callable kinds** for a method target: `m.kind IN ('method','property','constructor','function')`.

**Qualified-name suffix match** for type `T` + method `dst_name`:
```
( m.qualified_name = T || '.' || ed.dst_name OR m.qualified_name LIKE '%.' || T || '.' || ed.dst_name )
```
(`T` is `tb.type_name` for S2/S4, `ed.dst_recv` for S3.)

---

## Task 1: Rewrite Resolve — reset + non-call edges + denormalize (no strategies yet)

This task restructures `Resolve` while preserving existing behavior, so all current tests stay green before any strategy is added.

**Files:**
- Modify: `internal/graph/resolve.go`
- Test: existing `internal/graph/resolve_test.go` (must stay green)

- [ ] **Step 1: Restructure `Resolve`**

Replace the body of `Resolve` in `internal/graph/resolve.go` with the staged version below. It keeps the **exact existing name-match UPDATE** but scopes it to non-`calls` edges, then adds a parallel name-match UPDATE for `calls` edges (temporary — strategies replace it in Tasks 2–6 via candidates), then a no-op candidate clear. The doc comment is updated.

```go
// Resolve links reference edges to a defining node within the same worktree.
// Non-call edges (extends/implements/imports/references) bind dst_node_id by
// matching dst_name to a node name/qualified_name, scoped to the language family,
// preferring same-file then same-directory then a stable id order. Call edges are
// resolved into a confidence-ranked candidate set (edge_candidates) by the
// strategy passes below; edges.dst_node_id is then denormalized to the best
// candidate so single-target readers keep working. dst_name/dst_recv are retained
// so trace fallbacks still work where resolution is partial.
func (e *Engine) Resolve(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		// Idempotent re-resolve: clear this worktree's call candidates and reset
		// the call edges' denormalized target before recomputing.
		if _, err := c.ExecContext(ctx, `
DELETE FROM edge_candidates WHERE edge_id IN (
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.kind = 'calls')`, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = NULL
 WHERE kind = 'calls' AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree); err != nil {
			return err
		}

		// Non-call edges: unchanged name-match resolution.
		if err := resolveNonCallEdges(ctx, c, worktree); err != nil {
			return err
		}

		// Call edges: candidate strategies (added in Tasks 2–6), highest first.
		for _, strat := range callStrategies {
			if _, err := c.ExecContext(ctx, strat, worktree, worktree); err != nil {
				return err
			}
		}

		// Denormalize: edges.dst_node_id = best candidate (confidence desc, then a
		// stable id order) for each call edge.
		_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT ec.dst_node_id FROM edge_candidates ec
   WHERE ec.edge_id = edges.id
   ORDER BY ec.confidence DESC, ec.dst_node_id LIMIT 1)
 WHERE kind = 'calls' AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree)
		return err
	})
}
```

- [ ] **Step 2: Add `resolveNonCallEdges` (the existing UPDATE, scoped to non-calls)**

Add to `internal/graph/resolve.go`. This is the current `Resolve` UPDATE verbatim, with `AND edges.kind <> 'calls'` added:

```go
func resolveNonCallEdges(ctx context.Context, c store.Conn, worktree string) error {
	_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT n.id
    FROM nodes n
    JOIN files f  ON f.id = n.file_id
    JOIN files ef ON ef.id = edges.file_id
   WHERE f.worktree = ?
     AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
     AND ( ef.language = f.language
           OR ( ef.language IN ('typescript','tsx','javascript')
                AND f.language IN ('typescript','tsx','javascript') ) )
   ORDER BY
     CASE WHEN f.id = edges.file_id THEN 0 ELSE 1 END,
     CASE WHEN substr(f.path,1,instr(f.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
          THEN 0 ELSE 1 END,
     n.id
   LIMIT 1)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND kind <> 'calls'
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree, worktree)
	return err
}
```

- [ ] **Step 3: Add the (initially S5-only) `callStrategies` slice**

So call edges still resolve by name in the candidate model (preserving `TestResolveLinksCallEdges` etc.), start `callStrategies` with the S5 name fallback. Each strategy is a parameterized statement taking `(worktree, worktree)`. Add to `internal/graph/resolve.go`:

```go
// callStrategies are INSERT OR IGNORE passes into edge_candidates, applied in
// priority order. Each binds (worktree, worktree). Higher-confidence strategies
// run first; OR IGNORE keeps the first (best) confidence for an (edge,target).
var callStrategies = []string{
	stratNameFallback,
}

// S5: name-only fallback — any same-name callable in the family, capped per edge
// (locality-ordered), confidence 0.2. Only fires for edges with no better
// candidate yet (NOT EXISTS guard keeps it from diluting resolved edges).
const stratNameFallback = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT edge_id, dst_node_id, 0.2, 'name' FROM (
  SELECT ed.id AS edge_id, m.id AS dst_node_id,
    ROW_NUMBER() OVER (PARTITION BY ed.id ORDER BY
      CASE WHEN mf.id = ed.file_id THEN 0 ELSE 1 END,
      CASE WHEN substr(mf.path,1,instr(mf.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
           THEN 0 ELSE 1 END,
      m.id) AS rn
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
               AND m.name = ed.dst_name
  JOIN files mf ON mf.id = m.file_id
  WHERE ed.kind = 'calls' AND ed.dst_name <> ''
    AND ef.worktree = ? AND mf.worktree = ?
    AND ( ef.language = mf.language
          OR ( ef.language IN ('typescript','tsx','javascript')
               AND mf.language IN ('typescript','tsx','javascript') ) )
    AND NOT EXISTS (SELECT 1 FROM edge_candidates ec WHERE ec.edge_id = ed.id)
) WHERE rn <= 10`
```

Ensure `resolve.go` imports `context` and `store` (already does).

- [ ] **Step 4: Run existing resolve + trace tests**

Run: `go test ./internal/graph/ -run 'TestResolve|TestTraceCallPath' -v`
Expected: PASS — the candidate model reproduces name resolution; `dst_node_id` denormalization keeps `TestResolveLinksCallEdges`, `TestResolvePrefersSameFile`, `TestResolvePrefersSameTopLevelDir`, family tests, and trace tests green.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/resolve.go
git commit -m "refactor(graph): Resolve writes call candidates (S5 name) + denormalized dst_node_id

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: S2 — typed receiver (the core strategy)

**Files:**
- Modify: `internal/graph/resolve.go` (prepend `stratTypedReceiver` to `callStrategies`)
- Test: `internal/graph/candidates_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/graph/candidates_test.go`:

```go
package graph

import (
	"context"
	"testing"
)

// candidateMethods returns the resolution methods recorded for the call edge
// whose dst_name + dst_recv match, joined to the target's qualified name.
func candidateTargets(t *testing.T, e *Engine, dstName, recv string) map[string]string {
	t.Helper()
	rows, err := e.st.DB.Query(`
SELECT n.qualified_name, ec.method
  FROM edges ed
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes n ON n.id = ec.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name=? AND ed.dst_recv=?`, dstName, recv)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var qn, method string
		if err := rows.Scan(&qn, &method); err != nil {
			t.Fatal(err)
		}
		out[qn] = method
	}
	return out
}

func TestResolveS2TypedReceiverCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// _dep is a field of type Dep; _dep.Run() must resolve to Dep.Run (typed),
	// NOT to the collide-named Other.Run.
	src := []byte("namespace N;\n" +
		"class Dep { public void Run() {} }\n" +
		"class Other { public void Run() {} }\n" +
		"class Svc { private readonly Dep _dep; void M() { _dep.Run(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Run", "_dep")
	if got["N.Dep.Run"] != "typed" {
		t.Errorf("Run via _dep should have typed candidate N.Dep.Run; got %v", got)
	}
	if _, bad := got["N.Other.Run"]; bad {
		t.Errorf("Run via _dep should NOT bind Other.Run; got %v", got)
	}
}

func TestResolveS2TypedReceiverGo(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	if err := e.IndexFile(ctx, "/wt", "saga.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "step", "s")
	if got["Saga.step"] != "typed" {
		t.Errorf("s.step() should resolve typed to Saga.step; got %v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/graph/ -run TestResolveS2TypedReceiver -v`
Expected: FAIL — only the S5 `name` candidate exists (method is `name`, and C# binds both Dep.Run and Other.Run).

- [ ] **Step 3: Add `stratTypedReceiver` and prepend it**

In `internal/graph/resolve.go`, change the slice to:
```go
var callStrategies = []string{
	stratTypedReceiver,
	stratNameFallback,
}
```
and add:
```go
// S2: typed receiver — the receiver identifier resolves, via a type_binding in
// scope at the call (binding.scope contains the calling method's span), to type
// T; bind to T.<method>. Confidence 0.9.
const stratTypedReceiver = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'typed'
  FROM edges ed
  JOIN files ef  ON ef.id = ed.file_id
  JOIN nodes src ON src.id = ed.src_node_id
  JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = tb.type_name || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || tb.type_name || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/graph/ -run 'TestResolveS2TypedReceiver|TestResolve|TestTraceCallPath' -v`
Expected: PASS (new S2 tests + all existing).

- [ ] **Step 5: Commit**

```bash
git add internal/graph/resolve.go internal/graph/candidates_test.go
git commit -m "feat(graph): S2 typed-receiver call resolution via type_bindings

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: S1 — implicit self / `this` (C# sibling methods)

**Files:** Modify `internal/graph/resolve.go`; test in `candidates_test.go`.

- [ ] **Step 1: Failing test**

```go
func TestResolveS1SelfCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Bare Helper() and this.Helper() inside Svc must bind to Svc.Helper (self),
	// not to Other.Helper.
	src := []byte("namespace N;\n" +
		"class Other { void Helper() {} }\n" +
		"class Svc { void Helper() {} void M() { Helper(); this.Helper(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	// bare call: dst_recv ""
	bare := candidateTargets(t, e, "Helper", "")
	if bare["N.Svc.Helper"] != "self" {
		t.Errorf("bare Helper() should bind self to N.Svc.Helper; got %v", bare)
	}
	// this. call: dst_recv "this"
	thisc := candidateTargets(t, e, "Helper", "this")
	if thisc["N.Svc.Helper"] != "self" {
		t.Errorf("this.Helper() should bind self to N.Svc.Helper; got %v", thisc)
	}
}
```

- [ ] **Step 2: Run — FAIL** (`go test ./internal/graph/ -run TestResolveS1Self -v`): only `name` candidates, includes Other.Helper.

- [ ] **Step 3: Add `stratSelf`, prepend before S2**

```go
var callStrategies = []string{
	stratSelf,
	stratTypedReceiver,
	stratNameFallback,
}
```
```go
// S1: implicit self / this — a bare M() or this.M() inside a class binds to a
// sibling method of the same class (linked via contains edges: class -> method).
// Confidence 0.95. (C#; Go self-calls use an explicit receiver and go via S2.)
const stratSelf = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.95, 'self'
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN edges pc ON pc.kind = 'contains' AND pc.dst_node_id = ed.src_node_id
  JOIN edges cm ON cm.kind = 'contains' AND cm.src_node_id = pc.src_node_id
  JOIN nodes m  ON m.id = cm.dst_node_id
       AND m.kind IN ('method','property','constructor')
       AND m.name = ed.dst_name
 WHERE ed.kind = 'calls' AND (ed.dst_recv = '' OR ed.dst_recv = 'this')
   AND ef.worktree = ?
   AND ? IS NOT NULL`
```
Note: the second `?` is unused by this statement but kept so every entry in `callStrategies` has the same `(worktree, worktree)` arity; `? IS NOT NULL` is always true for a non-empty worktree string. (Alternatively, refactor the dispatch to pass arity per strategy — but uniform arity keeps the loop simple.)

- [ ] **Step 4: Run — PASS** (`go test ./internal/graph/ -run 'TestResolveS1Self|TestResolve|TestTraceCallPath' -v`).

- [ ] **Step 5: Commit** (`feat(graph): S1 self/this call resolution via contains edges`).

---

## Task 4: S3 — static type-name receiver

**Files:** Modify `internal/graph/resolve.go`; test in `candidates_test.go`.

- [ ] **Step 1: Failing test**

```go
func TestResolveS3StaticCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Factory { public static Thing Create() { return null; } }\n" +
		"class Svc { void M() { Factory.Create(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Create", "Factory")
	if got["N.Factory.Create"] != "static" {
		t.Errorf("Factory.Create() should bind static to N.Factory.Create; got %v", got)
	}
}
```

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Add `stratStatic`** (after S2, before S5):
```go
var callStrategies = []string{
	stratSelf,
	stratTypedReceiver,
	stratStatic,
	stratNameFallback,
}
```
```go
// S3: static type-name receiver — the receiver token is itself a type name T
// (Foo.Create()); bind to T.<method>. Confidence 0.9. A receiver that is a
// variable (no matching type node) simply yields no rows here.
const stratStatic = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'static'
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = ed.dst_recv || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || ed.dst_recv || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`
```

- [ ] **Step 4: Run — PASS** (S3 + all prior).
- [ ] **Step 5: Commit** (`feat(graph): S3 static type-name call resolution`).

---

## Task 5: S4 — interface/base expansion (C# only)

**Files:** Modify `internal/graph/resolve.go`; test in `candidates_test.go`.

- [ ] **Step 1: Failing test**

```go
func TestResolveS4InterfaceExpansionCSharp(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// _h is typed as the interface IHandler; impls A and B both define Handle.
	// Expect medium-confidence iface candidates for BOTH A.Handle and B.Handle.
	src := []byte("namespace N;\n" +
		"interface IHandler { void Handle(); }\n" +
		"class A : IHandler { public void Handle() {} }\n" +
		"class B : IHandler { public void Handle() {} }\n" +
		"class Svc { private readonly IHandler _h; void M() { _h.Handle(); } }")
	if err := e.IndexFile(ctx, "/wt", "H.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Handle", "_h")
	if got["N.A.Handle"] != "iface" || got["N.B.Handle"] != "iface" {
		t.Errorf("_h.Handle() should expand to A.Handle and B.Handle (iface); got %v", got)
	}
}
```

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Add `stratInterface`** (after S3, before S5):
```go
var callStrategies = []string{
	stratSelf,
	stratTypedReceiver,
	stratStatic,
	stratInterface,
	stratNameFallback,
}
```
```go
// S4: interface/base expansion (C#) — the receiver's bound type T is an interface
// or abstract class; expand to <method> on each implementing/deriving type, found
// via the extends edges (X extends/implements T) and X's contained methods.
// Confidence 0.5 (a set of possible dispatch targets).
const stratInterface = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.5, 'iface'
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes src ON src.id = ed.src_node_id
  JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  JOIN nodes iface ON iface.name = tb.type_name AND iface.kind IN ('interface','class')
  JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = ed.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?`
```
Note: `iface.kind IN ('interface','class')` covers C# interfaces and abstract base classes (both index as their declaration kind; abstract base is `class`). Go has no `implements`/`extends` for interfaces, so this yields nothing for Go (correct — Go interface dispatch stays S5).

- [ ] **Step 4: Run — PASS** (S4 + all prior). Verify the S2 test still holds: `_h` has no `IHandler.Handle` typed candidate (interface has the decl, not an impl), so only iface candidates appear — adjust the S2 test expectation only if it overlaps (it does not; different fixture).
- [ ] **Step 5: Commit** (`feat(graph): S4 C# interface/base call expansion`).

---

## Task 6: Verify denormalized top candidate + full suite

**Files:** Test in `candidates_test.go`.

- [ ] **Step 1: Failing test — best candidate wins `dst_node_id`**

```go
func TestResolveDenormalizesBestCandidate(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Dep { public void Run() {} }\n" +
		"class Other { public void Run() {} }\n" +
		"class Svc { private readonly Dep _dep; void M() { _dep.Run(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var qn string
	if err := e.st.DB.QueryRow(`
SELECT n.qualified_name FROM edges ed JOIN nodes n ON n.id = ed.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name='Run' AND ed.dst_recv='_dep'`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != "N.Dep.Run" {
		t.Errorf("denormalized dst_node_id = %q, want N.Dep.Run (typed beats name)", qn)
	}
}
```

- [ ] **Step 2: Run — should PASS** (S2 typed 0.9 outranks S5 name 0.2 in the denormalization ORDER BY). If it fails, the denormalization ORDER BY in Task 1 is wrong — fix there.

- [ ] **Step 3: Full suite**

Run: `go build ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 4: Commit** (`test(graph): denormalized best-candidate resolution`).

---

## End-of-phase verification

- [ ] **Live smoke** (throwaway dir, never the repo root). Index the Phase-1 demo repo, then:
```bash
sqlite3 -column -header "$DB" "SELECT ed.dst_name, ed.dst_recv, ec.method, ec.confidence, n.qualified_name
  FROM edges ed JOIN edge_candidates ec ON ec.edge_id=ed.id JOIN nodes n ON n.id=ec.dst_node_id
 WHERE ed.kind='calls' ORDER BY ed.dst_name;"
```
Expected: `step`/`s` → `Saga.step` (typed); `Run`/`_dep` → `Dep.Run` (typed). `edge_candidates` non-empty.

- [ ] **Review pass** over the resolver SQL (family scoping present on every cross-node strategy; `OR IGNORE` dedup; NOT EXISTS guard on S5).

## Phase 2 → Phase 3 handoff

`edge_candidates` is now populated with confidence + method; `edges.dst_node_id` holds the best candidate. **Phase 3** rewrites `trace.go`/`mcp` to consume the candidate set: confidence labels, adaptive floor, uncertainty-terminal tree with a global budget, candidate listing on low-confidence leaves, and `min_confidence` + tool coaching (spec §6).

## Self-review notes (author)

- **Spec §5 coverage:** S1→Task 3, S2→Task 2, S3→Task 4, S4→Task 5, S5→Task 1, denormalized top→Tasks 1 & 6. Non-call edge resolution preserved (Task 1).
- **Backward-compat:** existing resolve/trace tests gated in Tasks 1–5 (must stay green); `dst_node_id` denormalization preserves single-target readers and `query.go`.
- **Uniform strategy arity:** every `callStrategies` entry binds `(worktree, worktree)`; S1 uses the second only as an always-true guard (documented).
- **Scope match without call byte:** `binding.scope ⊇ src_node.span` — relies on Phase 1 setting local/param scope = enclosing-method span and field scope = class span.
- **Type consistency:** `edge_candidates(edge_id,dst_node_id,confidence,method)` matches Phase 1's schema; method tags `self|typed|static|iface|name` match spec §6.
