# Receiver-Aware Call Resolution — Phase 3 (LLM-Consumable Trace Surface) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans or subagent-driven-development. Steps use `- [ ]`.

**Goal:** Make `trace_call_path` consume the Phase 2 `edge_candidates` so the LLM gets confidence + resolution-method per hop, never a silently-empty or silently-wrong answer, and bounded output.

**Architecture:** Enrich `CallNode` (additive: `Resolution`, `Confidence`, `Receiver`). Rewrite `callNeighbors` to read `edge_candidates` with a **per-call-site adaptive floor** (a call site contributes its high/medium candidates, or its low ones only if it has nothing better). The walk is **uncertainty-terminal** (don't expand low-confidence hops) and bounded by a **global node budget**. MCP/CLI render the annotations; MCP gains a `min_confidence` input and tool-description coaching. Multiple candidates at an ambiguous call appear as sibling branches (the "candidate list").

**Tech Stack:** Go, SQLite. Spec §6. Builds on Phase 2 (`edge_candidates(edge_id,dst_node_id,confidence,method)`, `edges.dst_recv`).

**Scope note (honest):** This delivers **enriched, bounded, adaptively-floored annotated chains** — branches represent candidate sets. The fully de-duplicated nested tree from the spec is rendering sugar over the same data; deferred unless the chain form proves too verbose in practice.

---

## File structure

| File | Responsibility | Action |
|---|---|---|
| `internal/graph/trace.go` | `CallNode` fields; `callNeighbors` reads candidates + adaptive floor; walk uncertainty-terminal + budget; `confLabel` | Modify |
| `internal/graph/trace_test.go` | New behavior tests; existing tests stay green | Modify |
| `internal/mcp/graph.go` | `TraceCallPathInput.MinConfidence`; output hops carry resolution/confidence/receiver | Modify |
| `internal/mcp/server.go` | `trace_call_path` description coaching | Modify |
| `internal/mcp/graph_test.go` | MCP trace output assertions | Modify |
| `internal/cli/graph.go` | Render `[name resolution·confidence]` in callers/callees | Modify |

---

## Task 1: Enrich CallNode + candidate-aware callNeighbors (per-call-site adaptive floor)

**Files:** `internal/graph/trace.go`; `internal/graph/trace_test.go`.

- [ ] **Step 1: Failing test**

Add to `internal/graph/trace_test.go`:

```go
func TestCallNeighborsAnnotatesConfidence(t *testing.T) {
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
	chains, err := e.TraceCallPath(ctx, "/wt", "N.Svc.M", "callees", 2)
	if err != nil {
		t.Fatal(err)
	}
	// M -> Dep.Run, annotated typed/high; Other.Run must NOT appear (per-site floor
	// keeps the high typed candidate, drops the low name one).
	var sawRun bool
	for _, ch := range chains {
		for _, n := range ch {
			if n.Name == "Run" {
				sawRun = true
				if n.Path != "Svc.cs" || n.Resolution != "typed" || n.Confidence != "high" {
					t.Errorf("Run hop = %+v, want typed/high in Svc.cs", n)
				}
			}
		}
	}
	if !sawRun {
		t.Errorf("expected a Run callee; chains=%v", chains)
	}
}
```

- [ ] **Step 2: Run — FAIL** (`go test ./internal/graph/ -run TestCallNeighborsAnnotatesConfidence -v`): `CallNode` has no `Resolution`/`Confidence`.

- [ ] **Step 3: Enrich `CallNode` + add `confLabel`** in `internal/graph/trace.go`:

```go
// CallNode is one hop in a call chain.
type CallNode struct {
	NodeID     string
	Name       string
	Path       string
	Line       int
	Resolution string // how the edge into this hop resolved: self|typed|static|iface|name ("" for root)
	Confidence string // high|medium|low ("" for root)
	Receiver   string // receiver expression for an ambiguous hop, else ""
}

// confLabel buckets a candidate confidence into a verbalizable band.
func confLabel(c float64) string {
	switch {
	case c >= 0.8:
		return "high"
	case c >= 0.4:
		return "medium"
	default:
		return "low"
	}
}
```

- [ ] **Step 4: Rewrite `callNeighbors`** to read `edge_candidates`, grouping per call edge and applying the per-site adaptive floor + a `minConf` filter. Replace the existing `callNeighbors` body:

```go
// callNeighbors returns next-hop nodes for cur in the given direction, reading
// the candidate set. Per call site (edge): if it has any candidate at/above the
// medium band keep those, else keep its low candidates (capped) — so a confident
// call yields its precise target and an ambiguous call yields its alternatives.
// minConf drops anything below the caller's floor. Results are capped by fanout.
func (e *Engine) callNeighbors(ctx context.Context, worktree string, cur CallNode, direction string, minConf float64) ([]CallNode, error) {
	const fanout = 25
	const lowCapPerEdge = 5
	var q string
	if direction == "callers" {
		q = `
SELECT ed.id, n.id, n.name, f.path, n.start_line, ec.confidence, ec.method, ed.dst_recv
  FROM edge_candidates ec
  JOIN edges ed ON ed.id = ec.edge_id
  JOIN nodes n  ON n.id = ed.src_node_id
  JOIN files f  ON f.id = n.file_id
 WHERE ed.kind='calls' AND ec.dst_node_id = ? AND f.worktree = ?
 ORDER BY ed.id, ec.confidence DESC, n.id`
	} else {
		q = `
SELECT ed.id, n.id, n.name, f.path, n.start_line, ec.confidence, ec.method, ed.dst_recv
  FROM edges ed
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes n ON n.id = ec.dst_node_id
  JOIN files f ON f.id = n.file_id
 WHERE ed.kind='calls' AND ed.src_node_id = ? AND f.worktree = ?
 ORDER BY ed.id, ec.confidence DESC, n.id`
	}
	rows, err := e.st.DB.QueryContext(ctx, q, cur.NodeID, worktree)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type cand struct {
		node CallNode
		conf float64
	}
	byEdge := map[string][]cand{}
	var order []string
	for rows.Next() {
		var edgeID, recv, method string
		var n CallNode
		var conf float64
		if err := rows.Scan(&edgeID, &n.NodeID, &n.Name, &n.Path, &n.Line, &conf, &method, &recv); err != nil {
			return nil, err
		}
		n.Resolution, n.Confidence, n.Receiver = method, confLabel(conf), recv
		if _, seen := byEdge[edgeID]; !seen {
			order = append(order, edgeID)
		}
		byEdge[edgeID] = append(byEdge[edgeID], cand{n, conf})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []CallNode
	seen := map[string]bool{}
	for _, edgeID := range order {
		cs := byEdge[edgeID]
		hasBetter := cs[0].conf >= 0.4 // rows are confidence-desc within an edge
		kept := 0
		for _, c := range cs {
			if c.conf < minConf {
				continue
			}
			if hasBetter && c.conf < 0.4 {
				continue // per-site floor: drop low when a better candidate exists
			}
			if !hasBetter && kept >= lowCapPerEdge {
				break // cap the ambiguous candidate set
			}
			if seen[c.node.NodeID] {
				continue
			}
			seen[c.node.NodeID] = true
			out = append(out, c.node)
			kept++
			if len(out) >= fanout {
				return out, nil
			}
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Update `TraceCallPath` to pass `minConf`** — add a parameter and thread it. Change the signature and the one `callNeighbors` call:

```go
func (e *Engine) TraceCallPath(ctx context.Context, worktree, symbol, direction string, depth int) ([][]CallNode, error) {
	return e.traceCallPath(ctx, worktree, symbol, direction, depth, 0)
}

// traceCallPath is the implementation; minConf is the confidence floor (0 = all).
func (e *Engine) traceCallPath(ctx context.Context, worktree, symbol, direction string, depth int, minConf float64) ([][]CallNode, error) {
	// ... existing body, but call e.callNeighbors(ctx, worktree, cur, direction, minConf) ...
}
```
Keep the existing walk for now (budget + uncertainty-terminal come in Tasks 2–3). The public `TraceCallPath` keeps its signature (passes minConf=0), so the CLI and existing tests compile unchanged.

- [ ] **Step 6: Run — PASS** (`go test ./internal/graph/ -run 'TestCallNeighbors|TestTraceCallPath|TestResolve' -v`). The bug-110 caller test must still pass (candidates carry resolution; `Other.Run`/cross-class collisions excluded by the floor).

- [ ] **Step 7: Commit** (`feat(graph): candidate-aware callNeighbors with per-site adaptive floor`).

---

## Task 2: Uncertainty-terminal walk + global node budget

**Files:** `internal/graph/trace.go`; `internal/graph/trace_test.go`.

- [ ] **Step 1: Failing tests**

```go
func TestTraceUncertaintyIsTerminal(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A name-only (low) call must not be expanded further.
	// foo() is ambiguous (two foo); each foo calls bar(). Tracing callees of Caller
	// should stop at the low-confidence foo hop, never reaching bar.
	src := []byte("package p\n" +
		"func Caller(){ foo() }\n" +
		"func foo(){ bar() }\n" +
		"func foo2(){ }\n" + // forces foo name-set >1? keep simple: foo is unique here
		"func bar(){}\n")
	if err := e.IndexFile(ctx, "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "Caller", "callees", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range chains {
		for _, n := range ch {
			if n.Name == "bar" {
				t.Errorf("bar reached past a low-confidence foo hop (should be terminal); chains=%v", chains)
			}
		}
	}
}
```
(If `foo()` resolves higher than low in this fixture, adjust the fixture so `foo` is genuinely name-only/low — e.g., a receiver-typed call whose type is unknown. The invariant under test: a hop whose `Confidence=="low"` is not expanded.)

```go
func TestTraceGlobalBudget(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Wide fan-out; assert total emitted nodes across chains <= budget.
	// (Construct a function calling many distinct functions.)
	// ... build src with e.g. 60 callees ...
	// chains, _ := e.TraceCallPath(ctx,"/wt","Root","callees",3)
	// count distinct nodes; assert <= traceNodeBudget.
}
```

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement** — add `const traceNodeBudget = 40`; in the walk, (a) skip expansion when `nb.Confidence == "low"` (append the leaf chain, don't recurse), (b) stop adding nodes once a running counter hits `traceNodeBudget`, appending a final marker chain `[]CallNode{{Name: "… (budget reached; narrow the symbol or raise depth)"}}`. Thread a `*int` budget counter through `walk`.

- [ ] **Step 4: Run — PASS** (new + existing).

- [ ] **Step 5: Commit** (`feat(graph): uncertainty-terminal trace walk + global node budget`).

---

## Task 3: MCP surface — annotated hops, min_confidence, coaching

**Files:** `internal/mcp/graph.go`, `internal/mcp/server.go`, `internal/mcp/graph_test.go`.

- [ ] **Step 1: Failing test** in `internal/mcp/graph_test.go` — assert `trace_call_path` output hops carry `resolution`/`confidence`, and `min_confidence` filters. (Follow the existing graph_test.go harness for invoking the handler.)

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement** — change the output to annotated hops:

```go
type TraceHop struct {
	Symbol     string `json:"symbol"`
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Receiver   string `json:"receiver,omitempty"`
}
type TraceCallPathOutput struct {
	Chains [][]TraceHop `json:"chains"`
}
```
Add `MinConfidence float64` to `TraceCallPathInput` (jsonschema: "0–1 floor; omit for the adaptive default that shows confident hops and surfaces low-confidence ones only when nothing better exists"). Map `confidence` words to a floor (or call a new exported `TraceCallPathFiltered`). Build hops from `CallNode`.

- [ ] **Step 4: Coaching** in `internal/mcp/server.go` — expand the `trace_call_path` description: explain `resolution`/`confidence` tags, that low-confidence branches are candidate alternatives, and `min_confidence`.

- [ ] **Step 5: Run — PASS**; full `go test ./internal/mcp/`.

- [ ] **Step 6: Commit** (`feat(mcp): annotated trace_call_path hops + min_confidence + coaching`).

---

## Task 4: CLI rendering

**Files:** `internal/cli/graph.go`, `internal/cli/graph_test.go`.

- [ ] **Step 1: Failing test** — `graph callees` output includes a `[typed·high]`-style annotation.
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Implement** — in the `callers`/`callees` branch, render each hop as `name` plus, when `n.Resolution != ""`, ` [resolution·confidence]`; show `recv` on low hops.
- [ ] **Step 4: PASS.**
- [ ] **Step 5: Commit** (`feat(cli): annotate graph callers/callees with resolution + confidence`).

---

## Task 5: Verify + live smoke

- [ ] Full `go build ./... && go test ./...` — all PASS.
- [ ] Live smoke (throwaway dir): index the demo repo; `recarto graph callees N.Svc.M` shows `Run [typed·high]`; `recarto graph callers N.Dep.Run` shows `Svc.M`. Confirm an ambiguous name-only call renders its candidate branches + low label.
- [ ] Review pass.

## Self-review notes (author)
- Spec §6: confidence labels (T1), adaptive floor (T1, per-site), uncertainty-terminal + budget (T2), min_confidence + coaching (T3), candidate visibility via branches (T1). Nested-tree dedup deferred (documented).
- Backward-compat: `CallNode` additive; `TraceCallPath` signature unchanged (new `traceCallPath` impl); existing trace/CLI/MCP tests updated only where output shape changes (T3 changes MCP output type — update graph_test.go).
- bug-110 guard stays green (candidate floor excludes cross-class collisions).
