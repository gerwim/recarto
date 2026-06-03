# Recarto Phase 1C-2 — MCP Server & Claude Code Integration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Every task is RED→GREEN→REFACTOR (TDD is non-negotiable on this project).

**Goal:** Complete Phase 1 by shipping the two remaining surfaces over the existing memory engine: a **stdio MCP server** (`recarto serve`) exposing the Phase-1 tools, and **Claude Code integration** — an idempotent `recarto init` installer plus the `recarto hook session-start|post-read|stop` lifecycle entrypoints.

**Architecture:** A new `internal/mcp` package exposes the engine as MCP tools. The official Go SDK (`github.com/modelcontextprotocol/go-sdk`, pinned) is used in **exactly one file** (`server.go`), wrapped behind a generic `addTool` adapter; the actual tool logic lives in SDK-free handlers (`handlers.go`) that take an `*app.App` + a plain input struct and return a plain output struct, so they unit-test without any transport and SDK churn touches one file (mirrors the swappable `Lexical` boundary). A new `internal/agents/claudecode` package holds the installer (JSON-merging `.mcp.json` + `.claude/settings.json`, writing `RECARTO.md`) and the three hook handlers (each reads the hook JSON from an `io.Reader`, works through `*app.App`, and writes hook output to an `io.Writer`, so they test against buffers). The CLI gains three thin commands (`serve`, `init`, `hook`) wired into the existing `cli.Run` dispatcher; hooks are structurally non-blocking (the dispatcher exits 0 on every hook path).

**Tech Stack:** Go 1.26.4, pure-Go `modernc.org/sqlite`, **new dependency** `github.com/modelcontextprotocol/go-sdk v1.6.1` (pure-Go → static binary preserved), stdlib `encoding/json`/`flag`/`io`/`os`/`path/filepath`. The existing `internal/{app,gitctx,cache,store,memory,ledger,clock,export,version}` packages. Module path `git.gerwim.nl/gerwim/recarto`.

---

## Decisions locked for Phase 1C-2 (made in prior brainstorming; see `.wolf/cerebrum.md` Decision Log 2026-06-03)

1. **MCP server uses the official Go SDK** `github.com/modelcontextprotocol/go-sdk`, **version-pinned** and **wrapped behind our own thin handler interface** so SDK breaking changes touch one file. Rationale: multi-client interop (Claude Code + OpenCode + Copilot CLI) and the versioned `initialize`/capabilities handshake are error-prone to hand-roll across clients — take the dependency for protocol correctness. The SDK is pure-Go, so the static-binary goal (D1/D3) is preserved.
2. **Version pin = `v1.6.1`** (latest stable as of 2026-05-22; the cerebrum's earlier note said `v1.5.0` from before the version was checked — pin `v1.6.1` and correct the cerebrum). The current API is the typed `mcp.AddTool[In, Out]` form with `func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)` handlers and `jsonschema:"…"` struct tags. *(Verified 2026-06-03 against pkg.go.dev + the SDK quick-start/server docs.)*
3. **Hooks are pure CLI, not MCP** (spec §13) — they cold-start as short-lived processes and never load the MCP server. They are **Advisory + Augment + (opt-in) Stop gate** (D10); this plan ships the **Advisory** core: `session-start` (recall injection + session row + record nudge), `post-read` (ledger + repeated-read warning), `stop` (session finalize + advisory reminder). **Deferred (spec marks them opt-in/stretch):** the bounded opt-in **Stop gate** and **PreToolUse(pre-write)** injection — out of 1C-2's required scope, not a coverage gap.
4. **CLI stays on stdlib `flag`** (the Phase-1C decision overriding spec §12's cobra). `serve`/`init`/`hook` are thin commands wired into the existing `cli.Run` switch.

---

## Background: what already exists (Phases 1A + 1B + 1C-1, on `master`)

These are built, tested, and merged — **use them, do not reimplement**:

- **`internal/app`** — `app.Open(app.Config{CWD, Runner, Clock}) (*App, error)`; `App{Git gitctx.Context; Store *store.Store; Ledger *ledger.Ledger; Engine *memory.Engine; Clock clock.Clock}`; `(*App).Close() error`. The **single runtime wiring point**: resolves git identity once, opens the per-repo store (runs migrations), builds engine+ledger, upserts `meta`. The zero `Config` uses real cwd/git/clock. **Every surface in this plan builds an `*app.App` and works through it.**
- **`internal/memory`** — `Engine` with:
  - `Record(ctx, NewMemory) (id string, reinforced bool, err error)` — write+dedup; auto-pins `do_not_repeat`; invalid kind → error.
  - `Recall(ctx, RecallQuery) ([]RecallResult, error)` — structural+lexical+recency+reinforcement+branch ranking, token-budgeted, pins `do_not_repeat` first; returns `nil` (and bumps `recall_misses`) when nothing is selected.
  - `GetByID(ctx, id) (*Record, error)` → `(nil, nil)` if absent.
  - `List(ctx, ListFilter) ([]*Record, error)` (1C-1) — `ListFilter{Kind Kind; Branch, Status string; Limit int}`; default status `active`; ordered newest-updated first.
  - **The engine is git-agnostic**: callers pass `NewMemory.Branch`/`.Worktree` and `RecallQuery.Branch`. This plan injects them from `app.Git` at the surface edge (same as the 1C-1 CLI).
  - Types: `Kind` (`KindPreference|KindLearning|KindDecision|KindDoNotRepeat|KindBug|KindAction`; `Kind.Valid() bool`); `RefType` (`RefFile|RefSymbol|RefTag`); `Ref{Type RefType; Value string}`; `NewMemory{Kind,Title,Body string; Refs []Ref; Branch,Worktree,Author,Source string; Pinned bool; SupersedesID string; Extra map[string]any}`; `Record{ID string; Kind Kind; Title,Body,SearchText string; CreatedAt,UpdatedAt int64; Branch,Worktree,Author,Source string; Confidence float64; ReinforcementCount int; Pinned bool; Status,SupersededBy string; SimHash uint64; Extra map[string]any; Refs []Ref}`; `RecallQuery{TaskHint string; Files,Symbols []string; Kind Kind; Branch string; Limit,TokenBudget int}`; `RecallResult{Record *Record; Score float64; Signals map[string]float64}`.
- **`internal/ledger`** — `ledger.New(st) *Ledger`; `l.Incr(ctx, scope, metric string, n int64) error` (atomic UPSERT, cross-process safe); `l.Get(ctx, scope, metric) (int64, error)` (0 if absent); package func `ledger.EstimateTokens(s string) int`.
- **`internal/store`** — `st.DB *sql.DB` (read pool); `st.WriteTx(ctx, func(c store.Conn) error) error` (serialized `BEGIN IMMEDIATE` writer). `store.Conn` is the `ExecContext/QueryContext/QueryRowContext` subset. Schema (v2) includes `sessions(id TEXT PK, agent TEXT, branch TEXT, worktree TEXT, started_at INTEGER, ended_at INTEGER)` and `counters(scope, metric, value, PK(scope,metric))` — **no Go DAO exists for `sessions`; this plan writes/updates it directly via `WriteTx`.**
- **`internal/export`** — `export.Render(repoName string, recs []*memory.Record) string` (1C-1, on-demand markdown; not used by 1C-2 but available).
- **`internal/cli`** (1C-1) — each command is `func Name(ctx, *app.App, io.Writer[, args []string]) error`. `cli.Run(args []string, out io.Writer) error` validates the command via `known()` **before** `app.Open`. Helpers: `splitCSV`, `refsFromFlags`, `branchFor`, `parseFlags`. Test harness `newTestApp(t, branch)` + `fakeGit` live in `internal/cli/helpers_test.go`.
- **`internal/version`** — `version.Version string`.
- **`internal/clock`** — `clock.NewFake(t time.Time) *Fake`; `clock.Real{}`; `Clock.Now() time.Time`.
- **`cmd/recarto/main.go`** — `version`/`--version` and `debug-stress` are handled in `main`; **everything else already delegates to `cli.Run(os.Args[1:], os.Stdout)`**, so `serve`/`init`/`hook` flow through the default branch with **no change to `main.go`**.

**Running tests (use the module toolchain — ignore IDE gopls "undefined" false positives; the system Go is 1.24.4 but the module is `go 1.26.4`):**
```bash
go test ./...                              # whole suite
go test ./internal/mcp/ -run TestName -v
go test -race ./...                        # race check
gofmt -l .                                 # must print nothing (CI gates on gofmt)
```

**Branch note for the executor:** Phases 1A+1B+1C-1 are on `master`. Do all 1C-2 work on a fresh feature branch (e.g. `phase-1c2-mcp-hooks`) — never commit directly to `master`.

## Risk note

1C-2 introduces the project's **first new third-party dependency and first network step** (`go get` the MCP SDK in Task 3). Mitigations baked into the plan:
- The SDK is touched in **one file** (`internal/mcp/server.go`); all tool logic is SDK-free (Tasks 1–2 add **no** dependency and compile/pass before the SDK is fetched).
- After `go get`, **verify go.mod still has `go 1.26.4` alone with no redundant `toolchain` line** (cerebrum Do-Not-Repeat / bug-002: a `toolchain go1.26.4` line is rejected by Go).
- The hook handlers shell out to nothing and fake git via `gitctx.Runner` in tests; only Task 8's optional binary smoke uses real git (the repo's own).

## File Structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `internal/mcp/handlers.go` + `handlers_test.go` | SDK-free tool input/output types + pure handlers over `*app.App`; the `newTestApp` harness | T1, T2 |
| `internal/mcp/server.go` + `server_test.go` | the **only** SDK file: `NewServer` (adapter + `AddTool` registration) + `Serve`; in-memory round-trip test | T3 |
| `internal/agents/claudecode/settings.go` + `settings_test.go` | JSON object read/write + idempotent `.mcp.json` / hook merge helpers | T4 |
| `internal/agents/claudecode/install.go` + `install_test.go` | idempotent `Install(dir, binary)` → `.mcp.json` + `.claude/settings.json` + `RECARTO.md` | T5 |
| `internal/agents/claudecode/hooks.go` + `hooks_test.go` | `SessionStart` / `PostRead` / `Stop` hook handlers + input types | T6 |
| `internal/cli/serve.go`, `init.go`, `hook.go` (+ `init_test.go`, `hook_test.go`) | thin CLI commands over the two new packages | T7 |
| `internal/cli/cli.go` (modify) + `cli_test.go` (extend) | dispatch `serve`/`init`/`hook`; `binaryPath`; never-block hooks | T8 |

**Build order rationale:** the SDK-free handlers (T1–T2) compile and test with no new dependency. The single SDK file (T3) is added next. The Claude Code package (T4–T6) is independent of MCP. The CLI wrappers (T7) depend on T3+T6. The dispatch + full verification (T8) is last.

---

### Task 1: `internal/mcp` — tool types + `record_memory` / `recall_memory` handlers

**Files:**
- Create: `internal/mcp/handlers.go`
- Create: `internal/mcp/handlers_test.go`

- [ ] **Step 1: Write the failing test** (`internal/mcp/handlers_test.go`)

```go
package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
// Duplicated per test package (no shared testutil package yet — see cerebrum).
type fakeGit struct{ branch string }

func (f fakeGit) Run(args ...string) (string, error) {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir"):
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return "/repo", nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: branch},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestRecordMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind:  "learning",
		Body:  "use wal mode and busy_timeout for cross process writers",
		Files: []string{"internal/store/store.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.Reinforced {
		t.Errorf("first record = %+v, want a fresh id and Reinforced=false", out)
	}

	// A near-duplicate of the same kind reinforces instead of inserting.
	out2, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning",
		Body: "use wal mode and busy_timeout for cross process writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Reinforced {
		t.Errorf("near-duplicate = %+v, want Reinforced=true", out2)
	}
}

func TestRecordMemoryRejectsBadKind(t *testing.T) {
	a := newTestApp(t, "main")
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "bogus", Body: "x"}); err == nil {
		t.Error("expected error for invalid kind")
	}
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning"}); err == nil {
		t.Error("expected error for missing body")
	}
}

func TestRecallMemoryHandler(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind:  "learning",
		Title: "WAL setup",
		Body:  "use wal mode and busy_timeout for cross process writers",
		Files: []string{"internal/store/store.go"},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := recallMemory(ctx, a, RecallMemoryInput{Files: []string{"internal/store/store.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Memories) == 0 {
		t.Fatalf("recall returned nothing: %+v", out)
	}
	if out.Memories[0].Title != "WAL setup" {
		t.Errorf("top recall = %q, want WAL setup", out.Memories[0].Title)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/mcp/ -v`
Expected: FAIL — package `mcp` does not exist / `undefined: recordMemory` / `undefined: RecordMemoryInput`.

- [ ] **Step 3: Write the implementation** (`internal/mcp/handlers.go`)

```go
// Package mcp exposes the Recarto memory engine as Model Context Protocol tools.
// The tool logic in this file is deliberately SDK-free: each handler takes an
// *app.App and a plain input struct and returns a plain output struct, so it is
// unit-testable without any transport. The official MCP Go SDK is used only in
// server.go, behind the addTool adapter — so SDK churn touches one file (mirrors
// the swappable Lexical boundary in internal/memory).
package mcp

import (
	"context"
	"fmt"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// MemoryView is the wire shape for a memory returned by recall/list/get/search.
type MemoryView struct {
	ID                 string  `json:"id"`
	Kind               string  `json:"kind"`
	Title              string  `json:"title"`
	Body               string  `json:"body"`
	Branch             string  `json:"branch,omitempty"`
	Pinned             bool    `json:"pinned,omitempty"`
	ReinforcementCount int     `json:"reinforcement_count,omitempty"`
	Score              float64 `json:"score,omitempty"`
}

// RecordMemoryInput are the arguments to the record_memory tool. Fields without
// `omitempty` (kind, body) are required in the generated JSON schema.
type RecordMemoryInput struct {
	Kind         string   `json:"kind" jsonschema:"memory kind: preference, learning, decision, do_not_repeat, bug, or action"`
	Title        string   `json:"title,omitempty" jsonschema:"short one-line summary"`
	Body         string   `json:"body" jsonschema:"full memory content"`
	Files        []string `json:"files,omitempty" jsonschema:"related file paths"`
	Symbols      []string `json:"symbols,omitempty" jsonschema:"related symbol names"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
	Pinned       bool     `json:"pinned,omitempty" jsonschema:"pin so it is never evicted"`
	Global       bool     `json:"global,omitempty" jsonschema:"store repo-global instead of branch-scoped"`
	SupersedesID string   `json:"supersedes_id,omitempty" jsonschema:"id of a prior memory this replaces"`
}

// RecordMemoryOutput is the result of the record_memory tool.
type RecordMemoryOutput struct {
	ID         string `json:"id" jsonschema:"id of the recorded or reinforced memory"`
	Reinforced bool   `json:"reinforced" jsonschema:"true if an existing near-duplicate was reinforced"`
}

func recordMemory(ctx context.Context, a *app.App, in RecordMemoryInput) (RecordMemoryOutput, error) {
	kind := memory.Kind(in.Kind)
	if !kind.Valid() {
		return RecordMemoryOutput{}, fmt.Errorf("invalid kind %q", in.Kind)
	}
	if in.Body == "" {
		return RecordMemoryOutput{}, fmt.Errorf("body is required")
	}
	branch := a.Git.Branch
	if in.Global {
		branch = ""
	}
	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:         kind,
		Title:        in.Title,
		Body:         in.Body,
		Refs:         refsFrom(in.Files, in.Symbols, in.Tags),
		Branch:       branch,
		Worktree:     a.Git.Worktree,
		Author:       "mcp",
		Source:       "mcp",
		Pinned:       in.Pinned,
		SupersedesID: in.SupersedesID,
	})
	if err != nil {
		return RecordMemoryOutput{}, err
	}
	return RecordMemoryOutput{ID: id, Reinforced: reinforced}, nil
}

// RecallMemoryInput are the arguments to the recall_memory tool (all optional).
type RecallMemoryInput struct {
	TaskHint    string   `json:"task_hint,omitempty" jsonschema:"free-text description of the current task"`
	Files       []string `json:"files,omitempty" jsonschema:"files relevant to the current task"`
	Symbols     []string `json:"symbols,omitempty" jsonschema:"symbols relevant to the current task"`
	Kind        string   `json:"kind,omitempty" jsonschema:"restrict to a single kind"`
	Limit       int      `json:"limit,omitempty" jsonschema:"max candidates to consider"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"token budget for the returned set"`
}

// RecallMemoryOutput is the result of the recall_memory tool.
type RecallMemoryOutput struct {
	Count    int          `json:"count"`
	Memories []MemoryView `json:"memories"`
}

func recallMemory(ctx context.Context, a *app.App, in RecallMemoryInput) (RecallMemoryOutput, error) {
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint:    in.TaskHint,
		Files:       in.Files,
		Symbols:     in.Symbols,
		Kind:        memory.Kind(in.Kind),
		Branch:      a.Git.Branch,
		Limit:       in.Limit,
		TokenBudget: in.TokenBudget,
	})
	if err != nil {
		return RecallMemoryOutput{}, err
	}
	out := RecallMemoryOutput{Count: len(results), Memories: make([]MemoryView, 0, len(results))}
	for _, r := range results {
		v := viewOf(r.Record)
		v.Score = r.Score
		out.Memories = append(out.Memories, v)
	}
	return out, nil
}

// refsFrom builds structural refs from file/symbol/tag value lists.
func refsFrom(files, symbols, tags []string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range files {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: f})
	}
	for _, s := range symbols {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range tags {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
}

// viewOf projects a stored Record onto the wire shape (Score is set by callers).
func viewOf(r *memory.Record) MemoryView {
	return MemoryView{
		ID:                 r.ID,
		Kind:               string(r.Kind),
		Title:              r.Title,
		Body:               r.Body,
		Branch:             r.Branch,
		Pinned:             r.Pinned,
		ReinforcementCount: r.ReinforcementCount,
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/mcp/ -v`
Expected: PASS (`TestRecordMemoryHandler`, `TestRecordMemoryRejectsBadKind`, `TestRecallMemoryHandler`).

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/handlers_test.go
git commit -m "feat: mcp record_memory/recall_memory handlers (SDK-free)"
```

---

### Task 2: remaining handlers — `record_bug` / `search_bugs` / `get_memory` / `list_memory`

**Files:**
- Modify: `internal/mcp/handlers.go`
- Modify: `internal/mcp/handlers_test.go`

- [ ] **Step 1: Write the failing tests** (append to `internal/mcp/handlers_test.go`)

```go
func TestRecordBugThenSearch(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	add, err := recordBug(ctx, a, RecordBugInput{
		ErrorMessage: "nil pointer in session refresh",
		RootCause:    "missing nil guard",
		Fix:          "add a nil check before deref",
		File:         "internal/auth/session.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if add.ID == "" {
		t.Fatal("record_bug returned no id")
	}

	out, err := searchBugs(ctx, a, SearchBugsInput{Query: "session refresh"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 {
		t.Fatalf("search_bugs found nothing: %+v", out)
	}
	if out.Bugs[0].Title != "nil pointer in session refresh" {
		t.Errorf("top bug = %q", out.Bugs[0].Title)
	}
}

func TestRecordBugRequiresError(t *testing.T) {
	a := newTestApp(t, "main")
	if _, err := recordBug(context.Background(), a, RecordBugInput{}); err == nil {
		t.Error("expected error when error_message is empty")
	}
}

func TestGetAndListMemory(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "decision", Title: "sqlite is source of truth", Body: "markdown is render-only"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := getMemory(ctx, a, GetMemoryInput{ID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Memory == nil || got.Memory.Title != "sqlite is source of truth" {
		t.Errorf("get_memory = %+v", got)
	}

	missing, err := getMemory(ctx, a, GetMemoryInput{ID: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Found {
		t.Error("get_memory for a missing id should report Found=false")
	}

	list, err := listMemory(ctx, a, ListMemoryInput{Kind: "decision"})
	if err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Memories[0].Kind != "decision" {
		t.Errorf("list_memory(kind=decision) = %+v", list)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/mcp/ -run 'TestRecordBug|TestGetAndList' -v`
Expected: FAIL — `undefined: recordBug` / `undefined: RecordBugInput` / `undefined: getMemory`.

- [ ] **Step 3: Write the implementation** (append to `internal/mcp/handlers.go`)

```go
// RecordBugInput are the arguments to the record_bug convenience tool.
type RecordBugInput struct {
	ErrorMessage string   `json:"error_message" jsonschema:"the error text or user complaint"`
	RootCause    string   `json:"root_cause,omitempty" jsonschema:"why it broke"`
	Fix          string   `json:"fix,omitempty" jsonschema:"what fixed it"`
	File         string   `json:"file,omitempty" jsonschema:"the file that was fixed"`
	Tags         []string `json:"tags,omitempty" jsonschema:"freeform tags"`
}

// RecordBugOutput is the result of the record_bug tool.
type RecordBugOutput struct {
	ID         string `json:"id"`
	Reinforced bool   `json:"reinforced"`
}

func recordBug(ctx context.Context, a *app.App, in RecordBugInput) (RecordBugOutput, error) {
	if in.ErrorMessage == "" {
		return RecordBugOutput{}, fmt.Errorf("error_message is required")
	}
	extra := map[string]any{
		"error_message": in.ErrorMessage,
		"root_cause":    in.RootCause,
		"fix":           in.Fix,
		"file":          in.File,
	}
	if len(in.Tags) > 0 {
		extra["tags"] = in.Tags
	}
	var refs []memory.Ref
	if in.File != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: in.File})
	}
	// The body carries error + fix so the lexical index can match either.
	body := in.ErrorMessage
	if in.Fix != "" {
		body = in.ErrorMessage + " — fix: " + in.Fix
	}
	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:     memory.KindBug,
		Title:    in.ErrorMessage,
		Body:     body,
		Refs:     refs,
		Branch:   a.Git.Branch,
		Worktree: a.Git.Worktree,
		Author:   "mcp",
		Source:   "mcp",
		Extra:    extra,
	})
	if err != nil {
		return RecordBugOutput{}, err
	}
	return RecordBugOutput{ID: id, Reinforced: reinforced}, nil
}

// SearchBugsInput is the argument to the search_bugs tool.
type SearchBugsInput struct {
	Query string `json:"query" jsonschema:"keywords to search bug memories before fixing"`
}

// SearchBugsOutput is the result of the search_bugs tool.
type SearchBugsOutput struct {
	Count int          `json:"count"`
	Bugs  []MemoryView `json:"bugs"`
}

func searchBugs(ctx context.Context, a *app.App, in SearchBugsInput) (SearchBugsOutput, error) {
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: in.Query,
		Kind:     memory.KindBug,
		Branch:   a.Git.Branch,
	})
	if err != nil {
		return SearchBugsOutput{}, err
	}
	out := SearchBugsOutput{Count: len(results), Bugs: make([]MemoryView, 0, len(results))}
	for _, r := range results {
		v := viewOf(r.Record)
		v.Score = r.Score
		out.Bugs = append(out.Bugs, v)
	}
	return out, nil
}

// GetMemoryInput is the argument to the get_memory tool.
type GetMemoryInput struct {
	ID string `json:"id" jsonschema:"the memory id"`
}

// GetMemoryOutput is the result of the get_memory tool.
type GetMemoryOutput struct {
	Found  bool        `json:"found"`
	Memory *MemoryView `json:"memory,omitempty"`
}

func getMemory(ctx context.Context, a *app.App, in GetMemoryInput) (GetMemoryOutput, error) {
	rec, err := a.Engine.GetByID(ctx, in.ID)
	if err != nil {
		return GetMemoryOutput{}, err
	}
	if rec == nil {
		return GetMemoryOutput{Found: false}, nil
	}
	v := viewOf(rec)
	return GetMemoryOutput{Found: true, Memory: &v}, nil
}

// ListMemoryInput are the arguments to the list_memory tool (all optional).
type ListMemoryInput struct {
	Kind   string `json:"kind,omitempty" jsonschema:"restrict to a single kind"`
	Branch string `json:"branch,omitempty" jsonschema:"restrict to a branch (default: all branches)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows to return"`
}

// ListMemoryOutput is the result of the list_memory tool.
type ListMemoryOutput struct {
	Count    int          `json:"count"`
	Memories []MemoryView `json:"memories"`
}

func listMemory(ctx context.Context, a *app.App, in ListMemoryInput) (ListMemoryOutput, error) {
	recs, err := a.Engine.List(ctx, memory.ListFilter{
		Kind:   memory.Kind(in.Kind),
		Branch: in.Branch,
		Limit:  in.Limit,
	})
	if err != nil {
		return ListMemoryOutput{}, err
	}
	out := ListMemoryOutput{Count: len(recs), Memories: make([]MemoryView, 0, len(recs))}
	for _, r := range recs {
		out.Memories = append(out.Memories, viewOf(r))
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/mcp/ -v`
Expected: PASS (all six handler tests).

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/handlers.go internal/mcp/handlers_test.go
git commit -m "feat: mcp record_bug/search_bugs/get_memory/list_memory handlers"
```

---

### Task 3: `internal/mcp/server.go` — the SDK boundary + `Serve` + round-trip

**Files:**
- Modify: `go.mod` / `go.sum` (add the SDK dependency)
- Create: `internal/mcp/server.go`
- Create: `internal/mcp/server_test.go`

- [ ] **Step 1: Add the dependency** (this is the GREEN-enabling step for the SDK import)

```bash
go get github.com/modelcontextprotocol/go-sdk@v1.6.1
go mod tidy
```

Then **verify go.mod was not corrupted** (cerebrum Do-Not-Repeat / bug-002):
```bash
grep -n '^go ' go.mod        # expect exactly: go 1.26.4
grep -n '^toolchain' go.mod   # expect NO output (a redundant toolchain line breaks the build)
go build ./...                # must still build
```
Expected: `go 1.26.4` present, no `toolchain` line, build clean. If `go mod tidy` added a `toolchain` line, delete that line and re-run `go build ./...`.

- [ ] **Step 2: Write the failing test** (`internal/mcp/server_test.go`)

```go
package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// resultText concatenates text content and any structured content as JSON, so the
// assertion is robust to how the SDK serializes a typed tool result.
func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.StructuredContent != nil {
		j, _ := json.Marshal(res.StructuredContent)
		b.Write(j)
	}
	return b.String()
}

// connect wires a client to a fresh Recarto server over in-memory transports.
func connect(t *testing.T, a *appHolder) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := NewServer(a.app)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil) // connect server first so it is ready
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

type appHolder struct{ app interface{ /* placeholder */ } }

func TestServerRecordRecallRoundTrip(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	srv := NewServer(a)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Wait() }()

	rec, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name: "record_memory",
		Arguments: map[string]any{
			"kind":  "learning",
			"body":  "use wal mode and busy_timeout for cross process writers",
			"files": []any{"internal/store/store.go"},
		},
	})
	if err != nil {
		t.Fatalf("record_memory call: %v", err)
	}
	if rec.IsError {
		t.Fatalf("record_memory tool error: %s", resultText(rec))
	}

	rc, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "recall_memory",
		Arguments: map[string]any{"files": []any{"internal/store/store.go"}},
	})
	if err != nil {
		t.Fatalf("recall_memory call: %v", err)
	}
	if rc.IsError {
		t.Fatalf("recall_memory tool error: %s", resultText(rc))
	}
	if !strings.Contains(resultText(rc), "wal mode") {
		t.Errorf("recall did not surface the recorded memory:\n%s", resultText(rc))
	}
}

func TestServerInvalidKindIsRejected(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	srv := NewServer(a)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Wait() }()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "record_memory",
		Arguments: map[string]any{"kind": "bogus", "body": "x"},
	})
	// The SDK surfaces a handler error either as a transport error or as an
	// IsError result; accept either as "the bad kind was rejected".
	if err == nil && (res == nil || !res.IsError) {
		t.Errorf("expected invalid kind to be rejected, got res=%+v err=%v", res, err)
	}
}
```

> **Note for the executor:** delete the `connect`/`appHolder` scaffolding above before finishing — they were an early sketch. The two `Test...` functions are self-contained and are what must pass. (Kept here only to make the RED step obviously fail to compile first; remove them in Step 4 so `gofmt`/`go vet` stay clean.) If you prefer, omit `connect`/`appHolder` entirely from the start — they are not used by either test.

- [ ] **Step 3: Write the implementation** (`internal/mcp/server.go`)

```go
package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// NewServer builds the Recarto MCP server with every Phase-1 tool registered
// against a. This is the ONLY file that imports the MCP SDK: tool logic lives in
// SDK-free handlers (handlers.go), so an SDK upgrade or breaking change touches
// this file alone (the same isolation principle as the swappable Lexical backend).
func NewServer(a *app.App) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "recarto", Version: version.Version}, nil)
	addTool(s, a, "record_memory",
		"Record (or reinforce a near-duplicate of) a memory: a preference, learning, decision, do_not_repeat, bug, or action.",
		recordMemory)
	addTool(s, a, "record_bug",
		"Record a bug with its error message, root cause, and fix (a convenience wrapper over record_memory).",
		recordBug)
	addTool(s, a, "recall_memory",
		"Recall the most relevant memories for the current task — branch-aware, structurally ranked, and token-budgeted.",
		recallMemory)
	addTool(s, a, "search_bugs",
		"Search recorded bugs by keywords before attempting a fix.",
		searchBugs)
	addTool(s, a, "get_memory",
		"Fetch a single memory by id.",
		getMemory)
	addTool(s, a, "list_memory",
		"List stored memories, optionally filtered by kind and branch.",
		listMemory)
	return s
}

// addTool adapts an SDK-free handler func(ctx, *app.App, In) (Out, error) into the
// SDK's typed tool handler. Confining the SDK signature to this one adapter keeps
// every handler transport-agnostic and unit-testable.
func addTool[In, Out any](s *sdk.Server, a *app.App, name, desc string,
	fn func(context.Context, *app.App, In) (Out, error)) {
	sdk.AddTool(s, &sdk.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
			out, err := fn(ctx, a, in)
			// A non-nil err is surfaced by the SDK as a tool error result.
			return nil, out, err
		})
}

// Serve runs the MCP server over stdio until the client disconnects. It is the
// body of `recarto serve`.
func Serve(ctx context.Context, a *app.App) error {
	return NewServer(a).Run(ctx, &sdk.StdioTransport{})
}
```

- [ ] **Step 4: Remove the test scaffolding, then run the tests to verify they pass**

Delete the `connect` function and the `appHolder` type from `server_test.go` (Step 2 note), then:

Run: `go test ./internal/mcp/ -v`
Expected: PASS — all handler tests plus `TestServerRecordRecallRoundTrip` and `TestServerInvalidKindIsRejected`.

Also confirm formatting/vet are clean for the new file:
```bash
gofmt -l internal/mcp/
go vet ./internal/mcp/
```
Expected: `gofmt -l` prints nothing; vet clean.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat: mcp stdio server over official Go SDK (v1.6.1) + round-trip test"
```

---

### Task 4: `internal/agents/claudecode` — JSON merge helpers

**Files:**
- Create: `internal/agents/claudecode/settings.go`
- Create: `internal/agents/claudecode/settings_test.go`

- [ ] **Step 1: Write the failing tests** (`internal/agents/claudecode/settings_test.go`)

```go
package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureMCPServerIdempotent(t *testing.T) {
	doc := map[string]any{}
	if !ensureMCPServer(doc, "recarto") {
		t.Fatal("first ensure should report a change")
	}
	if ensureMCPServer(doc, "recarto") {
		t.Error("second ensure should be a no-op")
	}
	servers := doc["mcpServers"].(map[string]any)
	rc := servers["recarto"].(map[string]any)
	if rc["command"] != "recarto" {
		t.Errorf("command = %v, want recarto", rc["command"])
	}
}

func TestAppendHookAndHasCommandIdempotent(t *testing.T) {
	s := map[string]any{}
	cmd := "recarto hook session-start"
	if hasCommand(s, "SessionStart", cmd) {
		t.Fatal("empty settings should not contain the command")
	}
	appendHook(s, "SessionStart", "", cmd)
	if !hasCommand(s, "SessionStart", cmd) {
		t.Fatal("command should be present after append")
	}
	// A second, unrelated hook is added as a separate group (nothing clobbered).
	appendHook(s, "SessionStart", "", "other-tool --flag")
	groups := s["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(groups) != 2 {
		t.Errorf("expected 2 hook groups, got %d", len(groups))
	}
}

func TestReadWriteJSONObjectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "settings.json")

	m, err := readJSONObject(path) // missing file → empty object, no error
	if err != nil || len(m) != 0 {
		t.Fatalf("missing file should yield empty map: %v, %v", m, err)
	}
	m["x"] = "y"
	if err := writeJSONObject(path, m); err != nil { // creates parent dirs
		t.Fatal(err)
	}
	got, err := readJSONObject(path)
	if err != nil || got["x"] != "y" {
		t.Fatalf("round trip failed: %v, %v", got, err)
	}
	b, _ := os.ReadFile(path)
	var probe map[string]any
	if json.Unmarshal(b, &probe) != nil {
		t.Error("written file is not valid JSON")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agents/claudecode/ -v`
Expected: FAIL — package does not exist / `undefined: ensureMCPServer` / `undefined: readJSONObject`.

- [ ] **Step 3: Write the implementation** (`internal/agents/claudecode/settings.go`)

```go
// Package claudecode installs Recarto's Claude Code integration (MCP server
// registration + lifecycle hooks) and implements the hook entrypoints. The
// installer merges into existing .mcp.json / .claude/settings.json without
// clobbering unrelated content and is idempotent.
package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// readJSONObject reads a JSON object file into a map. A missing or empty file
// yields an empty (non-nil) map and no error, so callers can merge unconditionally.
func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeJSONObject writes m as indented JSON, creating parent directories.
func writeJSONObject(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// ensureMCPServer adds an mcpServers.recarto entry to doc if absent. It reports
// whether it changed doc.
func ensureMCPServer(doc map[string]any, binary string) bool {
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	if _, exists := servers["recarto"]; exists {
		return false
	}
	servers["recarto"] = map[string]any{
		"command": binary,
		"args":    []any{"serve"},
	}
	return true
}

// hasCommand reports whether settings already contains a command hook with the
// exact command string under the given event (used for idempotency).
func hasCommand(settings map[string]any, event, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == command {
				return true
			}
		}
	}
	return false
}

// appendHook appends a command hook group for event. matcher is omitted when
// empty (events like SessionStart/Stop run unconditionally).
func appendHook(settings map[string]any, event, matcher, command string) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	groups, _ := hooks[event].([]any)
	entry := map[string]any{"type": "command", "command": command}
	group := map[string]any{"hooks": []any{entry}}
	if matcher != "" {
		group["matcher"] = matcher
	}
	hooks[event] = append(groups, group)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/claudecode/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/settings.go internal/agents/claudecode/settings_test.go
git commit -m "feat: claudecode JSON merge helpers (.mcp.json + settings hooks)"
```

---

### Task 5: `Install` — the idempotent `recarto init` installer

**Files:**
- Create: `internal/agents/claudecode/install.go`
- Create: `internal/agents/claudecode/install_test.go`

- [ ] **Step 1: Write the failing test** (`internal/agents/claudecode/install_test.go`)

```go
package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCreatesConfigAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	// A pre-existing, unrelated hook must be preserved across install.
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"keep-me"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(dir, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.HooksStatus, "installed") {
		t.Errorf("HooksStatus = %q, want installed N", res.HooksStatus)
	}
	if res.MCPStatus != "configured" {
		t.Errorf("MCPStatus = %q, want configured", res.MCPStatus)
	}

	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json missing recarto serve:\n%s", mcp)
	}

	settings, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	for _, want := range []string{
		"keep-me",
		"recarto hook session-start",
		"recarto hook post-read",
		"recarto hook stop",
	} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("settings.json missing %q:\n%s", want, settings)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "RECARTO.md")); err != nil {
		t.Errorf("RECARTO.md not written: %v", err)
	}

	// Re-running installs nothing new.
	res2, err := Install(dir, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res2.HooksStatus != "already present" || res2.MCPStatus != "already present" {
		t.Errorf("second install not idempotent: %+v", res2)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agents/claudecode/ -run TestInstall -v`
Expected: FAIL — `undefined: Install` / `undefined: Result`.

- [ ] **Step 3: Write the implementation** (`internal/agents/claudecode/install.go`)

```go
package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
)

// Result summarizes what Install changed, for human-readable CLI output.
type Result struct {
	MCPStatus          string // "configured" | "already present"
	HooksStatus        string // "installed N" | "already present"
	InstructionsStatus string // "written" | "already present"
}

// hookSpec is one (event, matcher, command) hook the installer registers.
type hookSpec struct{ event, matcher, command string }

// hookSpecs are the Phase-1 Advisory hooks. SessionStart and Stop run
// unconditionally (no matcher); post-read is scoped to the Read tool.
func hookSpecs(binary string) []hookSpec {
	return []hookSpec{
		{"SessionStart", "", binary + " hook session-start"},
		{"PostToolUse", "Read", binary + " hook post-read"},
		{"Stop", "", binary + " hook stop"},
	}
}

// instructions is the RECARTO.md snippet describing the record/recall discipline.
const instructions = `# Recarto

This repo uses **Recarto** for cross-session memory (preferences, learnings,
decisions, do-not-repeats, bugs) stored in a shared per-repo SQLite cache outside
the working tree.

- At session start, Recarto injects the most relevant memories for this branch.
- **Record** new knowledge with the ` + "`record_memory`" + ` tool (or ` + "`recarto remember`" + `):
  preferences, learnings, decisions, do-not-repeats, and actions.
- **Record bugs** with ` + "`record_bug`" + ` (error, root cause, fix) and **search**
  them with ` + "`search_bugs`" + ` before fixing something.
- **Recall** relevant memory for the current task with ` + "`recall_memory`" + `.

Recarto reinforces near-duplicate memories instead of duplicating them, so record
freely — repeated learnings converge to one entry.
`

// Install sets up Claude Code integration for the repo rooted at dir, invoking
// the recarto binary as `binary`. It is idempotent: re-running adds nothing that
// is already present and preserves unrelated config.
func Install(dir, binary string) (Result, error) {
	var res Result

	// 1. Register the MCP server in .mcp.json.
	mcpPath := filepath.Join(dir, ".mcp.json")
	mcpDoc, err := readJSONObject(mcpPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(mcpDoc, binary) {
		res.MCPStatus = "configured"
	} else {
		res.MCPStatus = "already present"
	}
	if err := writeJSONObject(mcpPath, mcpDoc); err != nil {
		return res, err
	}

	// 2. Install lifecycle hooks into .claude/settings.json.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	settings, err := readJSONObject(settingsPath)
	if err != nil {
		return res, err
	}
	added := 0
	for _, hs := range hookSpecs(binary) {
		if !hasCommand(settings, hs.event, hs.command) {
			appendHook(settings, hs.event, hs.matcher, hs.command)
			added++
		}
	}
	if added > 0 {
		res.HooksStatus = fmt.Sprintf("installed %d", added)
	} else {
		res.HooksStatus = "already present"
	}
	if err := writeJSONObject(settingsPath, settings); err != nil {
		return res, err
	}

	// 3. Write RECARTO.md if absent.
	mdPath := filepath.Join(dir, "RECARTO.md")
	switch _, err := os.Stat(mdPath); {
	case err == nil:
		res.InstructionsStatus = "already present"
	case os.IsNotExist(err):
		if err := os.WriteFile(mdPath, []byte(instructions), 0o644); err != nil {
			return res, err
		}
		res.InstructionsStatus = "written"
	default:
		return res, err
	}

	return res, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/agents/claudecode/ -run TestInstall -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/install.go internal/agents/claudecode/install_test.go
git commit -m "feat: idempotent recarto init installer (.mcp.json, hooks, RECARTO.md)"
```

---

### Task 6: hook handlers — `session-start` / `post-read` / `stop`

**Files:**
- Create: `internal/agents/claudecode/hooks.go`
- Create: `internal/agents/claudecode/hooks_test.go`

- [ ] **Step 1: Write the failing tests** (`internal/agents/claudecode/hooks_test.go`)

```go
package claudecode

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// fakeGit + newTestApp duplicated per test package (no shared testutil yet).
type fakeGit struct{ branch string }

func (f fakeGit) Run(args ...string) (string, error) {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir"):
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return "/repo", nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: branch},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestSessionStartInjectsRecallAndOpensSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:  memory.KindDoNotRepeat,
		Title: "no redundant toolchain line",
		Body:  "go.mod must not carry a toolchain directive equal to the go line",
	}); err != nil {
		t.Fatal(err)
	}

	in := strings.NewReader(`{"session_id":"sess-1","source":"startup","hook_event_name":"SessionStart"}`)
	var out bytes.Buffer
	if err := SessionStart(ctx, a, in, &out); err != nil {
		t.Fatal(err)
	}

	s := out.String()
	if !strings.Contains(s, "additionalContext") {
		t.Errorf("session-start output is not a hook payload:\n%s", s)
	}
	if !strings.Contains(s, "no redundant toolchain line") {
		t.Errorf("session-start did not inject the pinned memory:\n%s", s)
	}
	if !strings.Contains(s, "record_memory") {
		t.Errorf("session-start missing the record nudge:\n%s", s)
	}

	var started int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE id='sess-1'`).Scan(&started); err != nil {
		t.Fatalf("session row not created: %v", err)
	}
}

func TestPostReadWarnsOnRepeat(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	payload := `{"session_id":"sess-1","tool_name":"Read","tool_input":{"file_path":"internal/store/store.go"},"tool_result":"package store // ..."}`

	var first bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.String(), "already read") {
		t.Errorf("first read should not warn:\n%s", first.String())
	}

	var second bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.String(), "already read") {
		t.Errorf("second read should warn:\n%s", second.String())
	}

	n, _ := a.Ledger.Get(ctx, "lifetime", "repeated_reads_blocked")
	if n != 1 {
		t.Errorf("repeated_reads_blocked = %d, want 1", n)
	}
}

func TestStopFinalizesSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"sess-1","source":"startup"}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"sess-1","stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "record_memory") {
		t.Errorf("stop reminder missing:\n%s", out.String())
	}

	var ended *int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT ended_at FROM sessions WHERE id='sess-1'`).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if ended == nil {
		t.Error("ended_at not set on stop")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agents/claudecode/ -run 'TestSessionStart|TestPostRead|TestStop' -v`
Expected: FAIL — `undefined: SessionStart` / `undefined: PostRead` / `undefined: Stop`.

- [ ] **Step 3: Write the implementation** (`internal/agents/claudecode/hooks.go`)

```go
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// sessionStartBudget is the token budget for the session-start recall slice (spec §9.2).
const sessionStartBudget = 1500

// sessionStartInput is the subset of the Claude Code SessionStart payload we use.
type sessionStartInput struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

// SessionStart opens/creates the session row, recalls a token-budgeted slice for
// this branch, and emits it as additionalContext plus a record nudge. It always
// exits 0 (the caller treats any returned error as non-blocking).
func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi sessionStartInput
	if err := decode(in, &hi); err != nil {
		return err
	}

	now := a.Clock.Now().UnixMilli()
	if hi.SessionID != "" {
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO sessions(id, agent, branch, worktree, started_at)
				 VALUES (?, 'claude-code', ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`,
				hi.SessionID, nullable(a.Git.Branch), a.Git.Worktree, now)
			return err
		}); err != nil {
			return err
		}
	}
	_ = a.Ledger.Incr(ctx, "lifetime", "sessions", 1)

	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		Branch:      a.Git.Branch,
		TokenBudget: sessionStartBudget,
	})
	if err != nil {
		return err
	}
	return writeHookContext(out, "SessionStart", formatSessionContext(results))
}

// postReadInput is the subset of the Claude Code PostToolUse(Read) payload we use.
type postReadInput struct {
	SessionID string `json:"session_id"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
	ToolResult json.RawMessage `json:"tool_result"`
}

// PostRead logs the read to the ledger and warns (non-blocking) when the same file
// has already been read this session, surfacing the tokens that re-read would cost.
func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi postReadInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	file := hi.ToolInput.FilePath
	if file == "" {
		return nil
	}
	scope := "session:" + hi.SessionID
	tokens := int64(ledger.EstimateTokens(string(hi.ToolResult)))

	_ = a.Ledger.Incr(ctx, "lifetime", "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "reads", 1)
	_ = a.Ledger.Incr(ctx, scope, "read:"+file, 1)

	n, err := a.Ledger.Get(ctx, scope, "read:"+file)
	if err != nil {
		return err
	}
	if n <= 1 {
		return nil // first read this session: nothing to warn about
	}
	_ = a.Ledger.Incr(ctx, "lifetime", "repeated_reads_blocked", 1)
	msg := fmt.Sprintf("You have already read %s in this session (%d times, ~%d tokens). "+
		"Rely on the earlier read instead of reading it again.", file, n, tokens)
	return writeHookContext(out, "PostToolUse", msg)
}

// stopInput is the subset of the Claude Code Stop payload we use.
type stopInput struct {
	SessionID      string `json:"session_id"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// Stop finalizes the session row and emits an advisory reminder. It never blocks
// (spec §13: Stop is exit-0 advisory in Phase 1).
func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	var hi stopInput
	if err := decode(in, &hi); err != nil {
		return err
	}
	if hi.SessionID != "" {
		now := a.Clock.Now().UnixMilli()
		if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`,
				now, hi.SessionID)
			return err
		}); err != nil {
			return err
		}
	}
	payload := map[string]any{
		"systemMessage": "recarto: record any new learnings (record_memory) and bug fixes (record_bug) before ending.",
	}
	return json.NewEncoder(out).Encode(payload)
}

// decode reads a single JSON object from r, tolerating an empty body.
func decode(r io.Reader, v any) error {
	if err := json.NewDecoder(r).Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// writeHookContext emits the Claude Code hookSpecificOutput.additionalContext shape.
func writeHookContext(out io.Writer, event, additionalContext string) error {
	payload := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": additionalContext,
		},
	}
	return json.NewEncoder(out).Encode(payload)
}

// formatSessionContext renders recalled memories plus a record nudge.
func formatSessionContext(results []memory.RecallResult) string {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")
	if len(results) == 0 {
		b.WriteString("(no prior memories yet)\n")
	} else {
		for _, r := range results {
			title := r.Record.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", r.Record.Kind, title)
			if r.Record.Body != "" {
				fmt.Fprintf(&b, "  %s\n", r.Record.Body)
			}
		}
	}
	b.WriteString("\nRecord new learnings with the record_memory tool and bug fixes with record_bug.")
	return b.String()
}

// nullable maps "" to a SQL NULL so repo-global rows store NULL, not "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/claudecode/ -v`
Expected: PASS (settings, install, and all three hook tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/hooks.go internal/agents/claudecode/hooks_test.go
git commit -m "feat: claudecode session-start/post-read/stop hook handlers"
```

---

### Task 7: CLI wrappers — `serve` / `init` / `hook`

**Files:**
- Create: `internal/cli/serve.go`
- Create: `internal/cli/init.go`
- Create: `internal/cli/hook.go`
- Create: `internal/cli/init_test.go`
- Create: `internal/cli/hook_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/cli/init_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Init(&out, dir, "recarto"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf(".mcp.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json not created: %v", err)
	}
	if !strings.Contains(out.String(), "mcp server") {
		t.Errorf("init output missing summary:\n%s", out.String())
	}
}
```

`internal/cli/hook_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHookSessionStartDispatch(t *testing.T) {
	a := newTestApp(t, "main")
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"s1","source":"startup"}`)
	if err := Hook(context.Background(), a, in, &out, []string{"session-start"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("hook session-start produced no context:\n%s", out.String())
	}
}

func TestHookUnknownEventErrors(t *testing.T) {
	a := newTestApp(t, "main")
	err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, []string{"bogus"})
	if err == nil {
		t.Error("expected error for unknown hook event")
	}
}

func TestHookRequiresEvent(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no event is given")
	}
}
```

(`newTestApp` already exists in `internal/cli/helpers_test.go` from 1C-1 — reuse it.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestInit|TestHook' -v`
Expected: FAIL — `undefined: Init` / `undefined: Hook`.

- [ ] **Step 3a: Write `serve`** (`internal/cli/serve.go`)

```go
package cli

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/mcp"
)

// Serve runs the MCP server over stdio until the client disconnects. It is a thin
// delegation to internal/mcp; the server itself is covered by the mcp round-trip
// test (it blocks on stdio, so it is not unit-tested here directly).
func Serve(ctx context.Context, a *app.App) error {
	return mcp.Serve(ctx, a)
}
```

- [ ] **Step 3b: Write `init`** (`internal/cli/init.go`)

```go
package cli

import (
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
)

// Init sets up Claude Code integration for the repo rooted at dir, registering the
// recarto binary as `binary` (its own absolute path at runtime). It is idempotent.
func Init(out io.Writer, dir, binary string) error {
	res, err := claudecode.Install(dir, binary)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "recarto init (%s)\n", dir)
	fmt.Fprintf(out, "  mcp server:   %s\n", res.MCPStatus)
	fmt.Fprintf(out, "  hooks:        %s\n", res.HooksStatus)
	fmt.Fprintf(out, "  instructions: %s\n", res.InstructionsStatus)
	return nil
}
```

- [ ] **Step 3c: Write `hook`** (`internal/cli/hook.go`)

```go
package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Hook dispatches a Claude Code lifecycle hook, reading the hook payload from in
// and writing hook output to out. Hooks are advisory: the caller (Run) exits 0
// even when this returns an error, so a hook failure never blocks the agent.
func Hook(ctx context.Context, a *app.App, in io.Reader, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: event required (session-start|post-read|stop)")
	}
	switch args[0] {
	case "session-start":
		return claudecode.SessionStart(ctx, a, in, out)
	case "post-read":
		return claudecode.PostRead(ctx, a, in, out)
	case "stop":
		return claudecode.Stop(ctx, a, in, out)
	default:
		return fmt.Errorf("hook: unknown event %q (session-start|post-read|stop)", args[0])
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestInit|TestHook' -v`
Expected: PASS (`TestInitWritesConfig`, `TestHookSessionStartDispatch`, `TestHookUnknownEventErrors`, `TestHookRequiresEvent`).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/serve.go internal/cli/init.go internal/cli/hook.go internal/cli/init_test.go internal/cli/hook_test.go
git commit -m "feat: cli serve/init/hook command wrappers"
```

---

### Task 8: dispatch + full verification

**Files:**
- Modify: `internal/cli/cli.go`
- Modify: `internal/cli/cli_test.go`

- [ ] **Step 1: Write the failing test** (append to `internal/cli/cli_test.go`)

```go
func TestRunHelpListsNewCommands(t *testing.T) {
	var buf bytes.Buffer
	if err := Run([]string{"help"}, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"init", "serve", "hook"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRunInitNeedsNoStore(t *testing.T) {
	// init must work in a non-git temp dir and must not require a resolvable repo.
	dir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Chdir(dir) // Go 1.24+: scopes os.Getwd to dir for this test
	if err := Run([]string{"init"}, io.Discard); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf("init did not write .mcp.json: %v", err)
	}
}
```

Add the imports this test needs to `internal/cli/cli_test.go` — the file currently imports `bytes`, `io`, `strings`, `testing`; add `os` and `path/filepath`:

```go
import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestRun -v`
Expected: FAIL — `TestRunHelpListsNewCommands` (usage lacks init/serve/hook) and/or `TestRunInitNeedsNoStore` (unknown command "init").

- [ ] **Step 3: Update the dispatcher** (`internal/cli/cli.go` — replace the whole file)

```go
package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// known reports whether cmd is a CLI command that needs an open App.
func known(cmd string) bool {
	switch cmd {
	case "status", "doctor", "remember", "recall", "bug", "memory", "serve", "hook":
		return true
	}
	return false
}

// Run dispatches a CLI invocation (args excludes the program name) and writes
// command output to out. It validates the command before opening the per-repo
// App, so unknown commands and `help` never touch the store. `init` configures
// the repo's files and needs no store. Hooks are non-blocking: a hook error is
// reported to stderr but Run still returns nil so the agent is never blocked.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage(out)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return usage(out)
	}

	// init operates on the working directory's files; no store needed.
	if cmd == "init" {
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return Init(out, dir, binaryPath())
	}

	if !known(cmd) {
		return fmt.Errorf("unknown command %q (try: init serve hook status doctor remember recall bug memory)", cmd)
	}

	a, err := app.Open(app.Config{})
	if err != nil {
		if cmd == "hook" {
			// Hooks must never block the agent, even if the store cannot open.
			fmt.Fprintln(os.Stderr, "recarto hook:", err)
			return nil
		}
		return err
	}
	defer a.Close()

	ctx := context.Background()
	switch cmd {
	case "status":
		return Status(ctx, a, out)
	case "doctor":
		return Doctor(ctx, a, out)
	case "remember":
		return Remember(ctx, a, out, rest)
	case "recall":
		return Recall(ctx, a, out, rest)
	case "bug":
		return Bug(ctx, a, out, rest)
	case "memory":
		return Memory(ctx, a, out, rest)
	case "serve":
		return Serve(ctx, a)
	case "hook":
		if err := Hook(ctx, a, os.Stdin, out, rest); err != nil {
			fmt.Fprintln(os.Stderr, "recarto hook:", err)
		}
		return nil // hooks never block
	}
	return nil // unreachable: known() gates the switch
}

// binaryPath returns the absolute path of the running recarto binary (so hooks
// and .mcp.json invoke the exact binary), falling back to the bare name.
func binaryPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "recarto"
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "recarto <command>")
	fmt.Fprintln(w, "  init              set up Claude Code integration (.mcp.json, hooks, RECARTO.md)")
	fmt.Fprintln(w, "  serve             run the MCP server over stdio")
	fmt.Fprintln(w, "  hook <event>      run a Claude Code hook (session-start|post-read|stop)")
	fmt.Fprintln(w, "  status            repo id, branch, memory counts, ledger")
	fmt.Fprintln(w, "  doctor            health checks (db, WAL, integrity, git)")
	fmt.Fprintln(w, "  remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)")
	fmt.Fprintln(w, "  recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)")
	fmt.Fprintln(w, "  bug add|search    record or search bug memories")
	fmt.Fprintln(w, "  memory show       render all active memory as markdown (--branch)")
	return nil
}
```

- [ ] **Step 4a: Run the cli suite to verify it passes**

Run: `go test ./internal/cli/ -v`
Expected: PASS (existing 1C-1 tests + the new `TestRunHelpListsNewCommands` and `TestRunInitNeedsNoStore`).

- [ ] **Step 4b: Build, vet, full suite, race, gofmt**

Run:
```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l .
```
Expected: build/vet clean; all tests PASS; `gofmt -l .` prints nothing. (`cmd/recarto/main.go` needs **no change** — `serve`/`init`/`hook` already flow through its default `cli.Run` branch.)

- [ ] **Step 4c: Smoke-test the binary end-to-end** (real, throwaway store + the repo's own git)

> **Do NOT run `recarto init` in the repo root** — it would write `.mcp.json`/`RECARTO.md` and edit `.claude/settings.json` in this working tree. Build the binary and run `init` inside a temp dir.

```bash
export RECARTO_CACHE_DIR="$(mktemp -d)"
BIN="$RECARTO_CACHE_DIR/recarto"
go build -o "$BIN" ./cmd/recarto

# init into a throwaway repo (subshell so the cwd change is scoped)
TMPREPO="$(mktemp -d)"
( cd "$TMPREPO" && git init -q && "$BIN" init )
ls "$TMPREPO/.mcp.json" "$TMPREPO/.claude/settings.json" "$TMPREPO/RECARTO.md"

# hooks (run from the recarto repo so real git resolves; same cache → shared store)
echo '{"session_id":"s1","source":"startup"}' | "$BIN" hook session-start
echo '{"session_id":"s1","tool_input":{"file_path":"go.mod"},"tool_result":"module x"}' | "$BIN" hook post-read
echo '{"session_id":"s1","tool_input":{"file_path":"go.mod"},"tool_result":"module x"}' | "$BIN" hook post-read
echo '{"session_id":"s1","stop_hook_active":false}' | "$BIN" hook stop
"$BIN" status
```
Expected:
- `init` prints `mcp server: configured`, `hooks: installed 3`, `instructions: written`; the three files exist in `$TMPREPO`.
- `hook session-start` prints a JSON object containing `"additionalContext"`.
- the **second** `hook post-read` prints a JSON object containing `already read go.mod` (cross-process counter sharing); the first prints nothing.
- `hook stop` prints a JSON object with the `systemMessage` reminder.
- `status` shows the lifetime ledger with `reads` ≥ 2 and `repeated_reads_blocked` ≥ 1.

(The MCP `serve` path is covered by the in-memory round-trip test in Task 3; it blocks on stdio and is not shell-smoked.)

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat: dispatch serve/init/hook; hooks never block"
```

- [ ] **Step 6: Update OpenWolf tracking + correct the SDK version note**

Per the OpenWolf protocol:
- `.wolf/anatomy.md`: add entries for the new files (`internal/mcp/*`, `internal/agents/claudecode/*`, `internal/cli/{serve,init,hook}.go`, this plan doc) — or run `openwolf scan` if available.
- `.wolf/memory.md`: append a session summary line.
- `.wolf/cerebrum.md`: **correct the Decision Log note that says SDK `v1.5.0` → `v1.6.1`** (the actual pinned version), and add a Key Learning capturing the SDK handler shape + the Claude Code hook JSON contract (so the next session does not re-research them).

Then commit:
```bash
git add .wolf/
git commit -m "docs: openwolf tracking + SDK v1.6.1 correction for Phase 1C-2"
```

---

## Self-Review

**1. Spec coverage:**

- **§5 / §11 MCP server + tools** — `record_memory` (T1), `recall_memory` (T1), `record_bug`/`search_bugs`/`get_memory`/`list_memory` (T2), stdio `serve` (T3 + T7/T8). ✅ Each tool is a thin handler over `internal/memory` + the engine; the SDK lives in one file. ✅
- **§14 MCP integration round-trip** — `TestServerRecordRecallRoundTrip` records then recalls over the in-memory JSON-RPC transport. ✅
- **§13 `recarto init` (idempotent)** — registers MCP server in `.mcp.json`, installs SessionStart/PostToolUse(Read)/Stop hooks into `.claude/settings.json` (preserving unrelated config), writes `RECARTO.md` (T4/T5 + T7/T8). ✅
- **§13 hooks** — SessionStart: opens/creates session row, emits token-budgeted recall as `additionalContext` + record nudge, always exit 0 (T6). PostToolUse(Read): logs to ledger, warns on repeated reads with token cost, exit 0 (T6). Stop: finalizes `ended_at`, advisory reminder, exit 0 (T6). ✅
- **§10 ledger** — hooks bump `sessions`, `reads`, `repeated_reads_blocked` atomically via the existing `Ledger.Incr`; `status` (1C-1) already surfaces them. ✅
- **§16 exit criteria** — fresh repo `recarto init`; session-start injects a budgeted recall slice; record/recall/bug via **MCP** (this plan) and **CLI** (1C-1); dedup reinforces (engine); ledger atomic; `memory show` markdown (1C-1); concurrency-safe shared store (1A). The binary smoke (T8) exercises init + hooks end-to-end. ✅
- **Deferred (spec §13 marks opt-in/stretch):** the bounded opt-in **Stop gate** and **PreToolUse(pre-write)** injection. Explicitly out of 1C-2's required scope (cerebrum split decision) — **not a coverage gap.** Recorded as such here so the next session does not treat them as missing.
- **D1/D3 static binary** — the only new dependency (MCP Go SDK) is pure-Go; T3 verifies the build and the `go 1.26.4`-with-no-`toolchain` invariant. ✅

**2. Placeholder scan:** No `TBD`/`TODO`/"handle errors appropriately"/"similar to Task N". Every code step is complete and compilable; every test asserts concrete behavior. The one piece of *scaffolding* (the `connect`/`appHolder` sketch in T3 Step 2) is explicitly flagged for deletion in T3 Step 4 and is not referenced by either passing test. ✅

**3. Type/signature consistency (checked against real 1A/1B/1C-1 source):**
- `app.Open(app.Config{CWD,Runner,Clock})`, `App{Git,Store,Ledger,Engine,Clock}`, `App.Close` — matches `internal/app/app.go`. ✅
- `Engine.Record(ctx,NewMemory)(string,bool,error)`, `Recall(ctx,RecallQuery)([]RecallResult,error)`, `GetByID(ctx,id)(*Record,error)`, `List(ctx,ListFilter)([]*Record,error)`; `NewMemory`/`Ref`/`Kind`/`RecallQuery`/`RecallResult`/`Record`/`ListFilter` fields; `Kind.Valid()`; `RefFile/RefSymbol/RefTag`; `KindBug/KindDoNotRepeat` — matches `internal/memory`. ✅
- `Ledger.Incr(ctx,scope,metric,n)`, `Ledger.Get(ctx,scope,metric)`, `ledger.EstimateTokens(string)int` — matches `internal/ledger/ledger.go`. ✅
- `Store.WriteTx(ctx,func(store.Conn)error)`, `store.Conn.ExecContext`, `Store.DB.QueryRowContext`; `sessions(id,agent,branch,worktree,started_at,ended_at)` and `counters(scope,metric,value)` columns — matches `internal/store/{store,migrate}.go`. ✅
- `clock.NewFake`, `version.Version` — matches 1A. ✅
- MCP SDK v1.6.1: `mcp.NewServer(*Implementation,*ServerOptions)`, `mcp.AddTool[In,Out](*Server,*Tool,handler)`, handler `func(ctx,*CallToolRequest,In)(*CallToolResult,Out,error)`, `Server.Run(ctx,Transport)`, `StdioTransport`, `NewInMemoryTransports`, `Server.Connect`, `NewClient`, `Client.Connect`, `ClientSession.CallTool(ctx,*CallToolParams)`, `CallToolResult.{Content,IsError,StructuredContent}`, `TextContent.Text`, `,omitempty`→optional field — verified against pkg.go.dev + the SDK quick-start/server docs (2026-06-03). ✅
- Claude Code hook I/O: SessionStart `{session_id,source}` → `{hookSpecificOutput:{hookEventName,additionalContext}}`; PostToolUse `{tool_input:{file_path},tool_result}`; Stop `{session_id,stop_hook_active}`; settings `hooks.<Event>[].{matcher?,hooks:[{type:"command",command}]}`; `.mcp.json` `{mcpServers:{recarto:{command,args}}}` — verified against the official hooks reference (2026-06-03). ✅
- CLI command shape `func Name(ctx,*app.App,io.Writer[,args])error`, `cli.Run(args,out)`, `known()`-before-`app.Open` — matches 1C-1 `internal/cli`. ✅ The two app-free/non-uniform signatures (`Init(out,dir,binary)`, `Serve(ctx,a)`, `Hook(ctx,a,in,out,args)`) are deliberate and consistent between their definition (T7) and call sites (T8).

**4. TDD ordering:** every task is RED (failing test) → GREEN (minimal code) → commit. The SDK-free handlers (T1–T2) and the claudecode package (T4–T6) compile and pass before the SDK file (T3) and the dispatcher (T8). ✅

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-03-recarto-phase1c2-mcp-and-hooks.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration (`superpowers:subagent-driven-development`).

**2. Inline Execution** — execute tasks in this session with checkpoints (`superpowers:executing-plans`).

Completing this plan **finishes Phase 1 (Memory Spine)**: the binary runs as CLI (1C-1), MCP server, and Claude Code hook entrypoint (1C-2) over one concurrency-safe per-repo store. Phase 2 (tree-sitter Code Graph) and Phase 3 (multi-agent + cross-worktree merge + background compaction) are specced separately.

Which approach?
