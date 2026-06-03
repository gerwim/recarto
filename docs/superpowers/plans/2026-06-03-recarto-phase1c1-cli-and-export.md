# Recarto Phase 1C-1 — CLI & Markdown Export — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Every task is RED→GREEN→REFACTOR (TDD is non-negotiable on this project).

**Goal:** Ship the first Phase-1C surface: a complete, usable standalone **command-line tool** over the existing memory engine — `recarto remember | recall | bug add | bug search | memory show | status | doctor` — plus the shared per-process **app wiring** (resolve git identity → open the per-repo store → build the engine) and an **on-demand markdown export**.

**Architecture:** A new `internal/app` package wires the runtime in one place: it resolves the git/worktree identity via `internal/gitctx`, opens the shared per-repo store at the global cache path (`internal/cache` + `internal/store`), builds the `internal/memory` engine + `internal/ledger`, and upserts repo metadata. The CLI (`internal/cli`, stdlib `flag` — **not** cobra, see Decisions) is a thin layer of command functions over `app.App`; each command function takes an `*app.App` and an `io.Writer` so it is unit-testable with a fake git runner and a temp store. Markdown export (`internal/export`) is a pure render function over `[]*memory.Record` (golden-tested). `Engine.List` (a new read on the engine) feeds both `memory show` and `status`.

**Tech Stack:** Go 1.26.4, pure-Go `modernc.org/sqlite`, stdlib only (`flag`, `encoding/json` via existing engine, `io`, `text` formatting). The existing `internal/{gitctx,cache,store,memory,ledger,clock,ids,version}` packages. Module path `git.gerwim.nl/gerwim/recarto`. **No new third-party dependencies in 1C-1** (the MCP Go SDK arrives in 1C-2).

---

## Decisions locked for Phase 1C (made this session, before this plan)

1. **MCP server → official Go SDK** (`github.com/modelcontextprotocol/go-sdk`, version-pinned, wrapped behind a thin handler interface). This is a **Phase 1C-2** concern; **1C-1 adds no MCP code and no new dependency.**
2. **CLI → stdlib `flag`**, overriding spec §12's cobra. The command set is small and stable, `cmd/recarto/main.go` already uses `flag`, and cobra adds no correctness — only convenience. Revisit in Phase 2 if the command tree grows.
3. **Phase 1C is split** into **1C-1** (this plan: app wiring + `Engine.List` + export + CLI = a complete standalone tool) and **1C-2** (MCP server + Claude Code installer/hooks: `init`/`serve`/`hook`).

(Recorded in `.wolf/cerebrum.md` Decision Log, 2026-06-03.)

---

## Background: what already exists (Phases 1A + 1B, on `master`)

These are built, tested, and merged — **use them, do not reimplement**:

- **`internal/gitctx`** — `gitctx.Resolve(r gitctx.Runner, cwd string) (gitctx.Context, error)`. `Runner` is an interface `Run(args ...string) (string, error)`; `gitctx.Real{Dir: cwd}` is the real impl. `Context` fields: `RepoID, RepoName, Branch, Worktree, RootCommit string; NonGit bool`. Branch is `"detached@<sha>"` on detached HEAD and `"(none)"` for a non-git dir.
- **`internal/cache`** — `cache.Dir() (string, error)` (`$RECARTO_CACHE_DIR` else `<os-cache>/recarto`); `cache.StorePath(repoID string) (string, error)` → `<dir>/<repoID>/store.db`.
- **`internal/store`** — `store.Open(path) (*Store, error)` creates parent dirs, sets WAL/busy_timeout/synchronous PRAGMAs, and runs migrations (schema v2; `memories`, `memory_refs`, `memory_minhash`, `counters`, `sessions`, `meta`, `memories_fts`). `st.DB *sql.DB` (read pool); `st.WriteTx(ctx, func(c store.Conn) error) error` (serialized `BEGIN IMMEDIATE` writer); `st.Close()`. `store.Conn` is the `ExecContext/QueryContext/QueryRowContext` subset.
- **`internal/ledger`** — `ledger.New(st) *Ledger`; `l.Incr(ctx, scope, metric string, n int64) error`; `l.Get(ctx, scope, metric) (int64, error)` (returns 0 if absent); `ledger.EstimateTokens(s string) int`.
- **`internal/memory`** — `memory.New(st *store.Store, led *ledger.Ledger, clk clock.Clock) *Engine`.
  - `e.Record(ctx, nm NewMemory) (id string, reinforced bool, err error)` — write+dedup (reinforces a near-duplicate same-kind active row instead of inserting). `do_not_repeat` is auto-pinned. Invalid kind → error.
  - `e.Recall(ctx, q RecallQuery) ([]RecallResult, error)` — structural+lexical+recency+reinforcement+branch ranking, token-budgeted, pins `do_not_repeat` first; returns `nil` (and bumps `recall_misses`) when nothing is selected.
  - `e.GetByID(ctx, id) (*Record, error)` → `(nil, nil)` if absent.
  - **The engine is deliberately git-agnostic**: the caller passes `NewMemory.Branch`/`NewMemory.Worktree` and `RecallQuery.Branch`. **This plan is where they get wired** (from `app.Git`).
  - Types: `Kind` (`KindPreference|KindLearning|KindDecision|KindDoNotRepeat|KindBug|KindAction`, `Kind.Valid() bool`); `RefType` (`RefFile|RefSymbol|RefTag`); `Ref{Type RefType; Value string}`; `NewMemory{Kind,Title,Body string; Refs []Ref; Branch,Worktree,Author,Source string; Pinned bool; SupersedesID string; Extra map[string]any}`; `Record{ID string; Kind Kind; Title,Body,SearchText string; CreatedAt,UpdatedAt int64; Branch,Worktree,Author,Source string; Confidence float64; ReinforcementCount int; Pinned bool; Status,SupersededBy string; SimHash uint64; Extra map[string]any; Refs []Ref}`; `RecallQuery{TaskHint string; Files,Symbols []string; Kind Kind; Branch string; Limit,TokenBudget int}`; `RecallResult{Record *Record; Score float64; Signals map[string]float64}`.
  - **In-package internals this plan reuses for `Engine.List`** (do not duplicate): the package-private `selectMemoriesSQL` constant, `scanMemories(rows *sql.Rows) ([]*Record, error)`, and `(*Engine).loadRefs(ctx, recs []*Record) error` (all in `internal/memory/engine.go`). `Engine.List` must live in `package memory` to use them.
- **`internal/clock`** — `clock.Clock` interface (`Now() time.Time`); `clock.Real{}`; `clock.NewFake(t time.Time) *Fake` (with `.Advance(d)`).
- **`internal/version`** — `version.Version` (string).
- **`cmd/recarto/main.go`** — currently dispatches `version`/`--version` and the internal `debug-stress` command (which opens its own store via `--db`); everything else errors. This plan rewires the default branch to delegate to `internal/cli` (**keep `version` and `debug-stress` exactly as they are**).

**Running tests (use the module toolchain — ignore IDE gopls "undefined" false positives; the system Go is 1.24.4 but the module is `go 1.26.4`):**
```bash
go test ./...                              # whole suite
go test ./internal/cli/ -run TestName -v
go test -race ./...                        # race check
gofmt -l .                                 # must print nothing (CI gates on gofmt)
```

**Branch note for the executor:** Phases 1A+1B are on `master`. Do all 1C-1 work on a fresh feature branch (e.g. `phase-1c1-cli-export`) — never commit directly to `master`.

## Risk note

1C-1 has **no FTS/SDK/protocol risk** (those are 1C-2). The only external dependency is `git`, which every test fakes via a `gitctx.Runner`. No task here shells out to real git or the network.

## File Structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `internal/app/app.go` + `_test.go` | `App` runtime: resolve git → open store → build engine/ledger; write `meta` | T1 |
| `internal/memory/list.go` + `list_test.go` | `Engine.List` — filtered, ordered read of stored memories | T2 |
| `internal/export/markdown.go` + `_test.go` | pure `Render([]*memory.Record) string` (golden) | T3 |
| `internal/cli/helpers.go`, `helpers_test.go` | `splitCSV`, ref/branch helpers + the `newTestApp` test harness (`fakeGit`) | T4 |
| `internal/cli/status.go` + `_test.go` | `Status` command | T4 |
| `internal/cli/doctor.go` + `_test.go` | `Doctor` command (WAL/integrity/schema/git) | T5 |
| `internal/cli/remember.go` + `_test.go` | `Remember` command | T6 |
| `internal/cli/recall.go` + `_test.go` | `Recall` command | T7 |
| `internal/cli/bug.go` + `_test.go` | `Bug` (`add` / `search`) command | T8 |
| `internal/cli/memory.go` + `_test.go` | `Memory` (`show`) command → export | T9 |
| `internal/cli/cli.go` (dispatch) + `cli_test.go`; `cmd/recarto/main.go` (modify) | `cli.Run` switch + wire `main` | T10 |

**Build order rationale:** command *functions* (T4–T9) are written and tested directly against an `*app.App` — they do not depend on the `cli.Run` dispatcher, so every task compiles green. The `cli.Run` switch and `main.go` wiring come last (T10), once all command functions exist.

---

### Task 1: `internal/app` — runtime wiring

**Files:**
- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`

- [ ] **Step 1: Write the failing test** (`internal/app/app_test.go`)

```go
package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
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

func newTestApp(t *testing.T, branch string) *App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := Open(Config{
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

func TestOpenResolvesIdentityAndStore(t *testing.T) {
	a := newTestApp(t, "main")

	if a.Git.Branch != "main" {
		t.Errorf("branch = %q, want main", a.Git.Branch)
	}
	if a.Git.RepoID == "" || a.Git.RepoName != "repo" {
		t.Errorf("git identity not resolved: %+v", a.Git)
	}
	if a.Engine == nil || a.Ledger == nil || a.Store == nil {
		t.Fatal("app components not wired")
	}

	// Sanity: the engine writes through the opened store.
	if _, _, err := a.Engine.Record(context.Background(), newLearning()); err != nil {
		t.Fatalf("engine record failed: %v", err)
	}

	// Open writes repo metadata.
	var name string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key='repo_name'`).Scan(&name); err != nil {
		t.Fatalf("repo_name meta not written: %v", err)
	}
	if name != "repo" {
		t.Errorf("repo_name = %q, want repo", name)
	}
}
```

Also add this tiny helper to `app_test.go` (keeps the test import of `memory` local and obvious):

```go
import "git.gerwim.nl/gerwim/recarto/internal/memory"

func newLearning() memory.NewMemory {
	return memory.NewMemory{Kind: memory.KindLearning, Body: "use wal mode for cross process writers"}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestOpenResolvesIdentityAndStore -v`
Expected: FAIL — package `app` does not exist / `undefined: Open` / `undefined: Config`.

- [ ] **Step 3: Write the implementation** (`internal/app/app.go`)

```go
// Package app wires Recarto's per-process runtime in one place: it resolves the
// git/worktree identity, opens the shared per-repo store, and constructs the
// memory engine and ledger. Every surface (the CLI now; the MCP server and hooks
// in Phase 1C-2) builds an App and works through it, so the wiring lives here and
// nowhere else.
package app

import (
	"context"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// Config configures App construction. The zero value is valid: it resolves the
// real working directory, real git, and the real clock.
type Config struct {
	CWD    string        // "" → os.Getwd()
	Runner gitctx.Runner // nil → gitctx.Real{Dir: CWD}
	Clock  clock.Clock   // nil → clock.Real{}
}

// App is the resolved per-process runtime shared by every surface.
type App struct {
	Git    gitctx.Context
	Store  *store.Store
	Ledger *ledger.Ledger
	Engine *memory.Engine
	Clock  clock.Clock
}

// Open resolves identity, opens the per-repo store at the global cache path (which
// also runs migrations), builds the engine + ledger, and upserts repo metadata.
// The git identity is resolved exactly once here and cached on App, so a
// long-lived `serve` process resolves git a single time (spec §6).
func Open(cfg Config) (*App, error) {
	cwd := cfg.CWD
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = wd
	}
	runner := cfg.Runner
	if runner == nil {
		runner = gitctx.Real{Dir: cwd}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	gc, err := gitctx.Resolve(runner, cwd)
	if err != nil {
		return nil, err
	}
	path, err := cache.StorePath(gc.RepoID)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	led := ledger.New(st)
	eng := memory.New(st, led, clk)

	a := &App{Git: gc, Store: st, Ledger: led, Engine: eng, Clock: clk}
	if err := a.writeMeta(context.Background()); err != nil {
		_ = st.Close()
		return nil, err
	}
	return a, nil
}

// Close releases the store.
func (a *App) Close() error { return a.Store.Close() }

// writeMeta upserts repo identity metadata. It is idempotent and safe under
// concurrency (the store serializes writers; the UPSERT is atomic).
func (a *App) writeMeta(ctx context.Context) error {
	return a.Store.WriteTx(ctx, func(c store.Conn) error {
		for _, kv := range [][2]string{
			{"repo_name", a.Git.RepoName},
			{"repo_id", a.Git.RepoID},
			{"root_commit", a.Git.RootCommit},
			{"recarto_version", version.Version},
		} {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/app/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/app.go internal/app/app_test.go
git commit -m "feat: internal/app wires gitctx + store + engine + ledger"
```

---

### Task 2: `Engine.List` — filtered read of stored memories

**Files:**
- Create: `internal/memory/list.go`
- Create: `internal/memory/list_test.go`

- [ ] **Step 1: Write the failing test** (`internal/memory/list_test.go`)

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestListFiltersByKind(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(5_000_000)))
	ctx := context.Background()

	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "use wal mode and busy_timeout for cross process writers"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug,
		Body: "auth token refresh leaks a goroutine on error return"}); err != nil {
		t.Fatal(err)
	}

	all, err := e.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List() all active = %d, want 2", len(all))
	}

	bugs, err := e.List(ctx, ListFilter{Kind: KindBug})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Kind != KindBug {
		t.Fatalf("List(kind=bug) = %+v, want exactly 1 bug", bugs)
	}
	// Refs are loaded (here: none) and ordering is stable.
	if bugs[0].ID == "" {
		t.Error("listed record missing id")
	}
}
```

(`newTestEngine` and `newTestStore` already exist in `internal/memory/engine_test.go` / `lexical_test.go` — reuse them; do not redefine.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestListFiltersByKind -v`
Expected: FAIL — `undefined: ListFilter` / `e.List`.

- [ ] **Step 3: Write the implementation** (`internal/memory/list.go`)

```go
package memory

import "context"

// ListFilter selects stored memories for read-only listing (markdown export,
// status counts, and the MCP list_memory tool in Phase 1C-2).
type ListFilter struct {
	Kind   Kind   // "" → any kind
	Branch string // "" → all branches; else this branch OR repo-global (NULL)
	Status string // "" → "active"
	Limit  int    // 0 → no limit
}

// List returns stored records (with refs) matching f, ordered newest-updated
// first (then id) for stable, deterministic output.
func (e *Engine) List(ctx context.Context, f ListFilter) ([]*Record, error) {
	status := f.Status
	if status == "" {
		status = "active"
	}
	q := selectMemoriesSQL + ` WHERE status = ?`
	args := []any{status}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(f.Kind))
	}
	if f.Branch != "" {
		q += ` AND (branch IS NULL OR branch = ?)`
		args = append(args, f.Branch)
	}
	q += ` ORDER BY updated_at DESC, id ASC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := e.st.DB.QueryContext(ctx, q, args...)
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestListFiltersByKind -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/list.go internal/memory/list_test.go
git commit -m "feat: Engine.List filtered/ordered read for export and status"
```

---

### Task 3: `internal/export` — markdown renderer

**Files:**
- Create: `internal/export/markdown.go`
- Create: `internal/export/markdown_test.go`

- [ ] **Step 1: Write the failing test** (`internal/export/markdown_test.go`)

```go
package export

import (
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestRenderGolden(t *testing.T) {
	// 1_700_000_000_000 ms = 2023-11-14 UTC.
	recs := []*memory.Record{
		{
			Kind:               memory.KindDoNotRepeat,
			Title:              "no redundant toolchain line",
			Body:               "go.mod must not carry a toolchain directive equal to the go line",
			UpdatedAt:          1_700_000_000_000,
			Branch:             "",
			ReinforcementCount: 2,
			Refs:               []memory.Ref{{Type: memory.RefFile, Value: "go.mod"}},
		},
		{
			Kind:               memory.KindLearning,
			Title:              "WAL needs busy_timeout",
			Body:               "set busy_timeout so writers wait instead of erroring",
			UpdatedAt:          1_700_000_000_000,
			Branch:             "main",
			ReinforcementCount: 0,
		},
	}

	want := "# Recarto Memory — demo\n\n" +
		"_2 active memories_\n\n" +
		"## Do Not Repeat\n\n" +
		"- **no redundant toolchain line**\n" +
		"  go.mod must not carry a toolchain directive equal to the go line\n" +
		"  _updated 2023-11-14 · reinforced ×2 · scope global · refs: go.mod_\n\n" +
		"## Learnings\n\n" +
		"- **WAL needs busy_timeout**\n" +
		"  set busy_timeout so writers wait instead of erroring\n" +
		"  _updated 2023-11-14 · reinforced ×0 · scope main · refs: none_\n"

	got := Render("demo", recs)
	if got != want {
		t.Errorf("Render mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	got := Render("demo", nil)
	want := "# Recarto Memory — demo\n\n_0 active memories_\n"
	if got != want {
		t.Errorf("empty render = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/export/ -v`
Expected: FAIL — package `export` does not exist / `undefined: Render`.

- [ ] **Step 3: Write the implementation** (`internal/export/markdown.go`)

```go
// Package export renders stored memories to markdown on demand. SQLite is the
// single source of truth (spec D2); markdown is a render-only view with no
// persistent file, so it can never drift.
package export

import (
	"fmt"
	"strings"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// kindSections lists the kinds in display order with their headings. Pinned
// do_not_repeat comes first; the rest follow most-to-least correctness-critical.
var kindSections = []struct {
	kind    memory.Kind
	heading string
}{
	{memory.KindDoNotRepeat, "Do Not Repeat"},
	{memory.KindDecision, "Decisions"},
	{memory.KindPreference, "Preferences"},
	{memory.KindLearning, "Learnings"},
	{memory.KindBug, "Bugs"},
	{memory.KindAction, "Actions"},
}

// Render returns a markdown document for recs, grouped by kind in display order.
// It is pure (no I/O, no clock) and preserves the input order within each kind,
// so callers control sorting (see Engine.List) and the output is deterministic.
func Render(repoName string, recs []*memory.Record) string {
	byKind := map[memory.Kind][]*memory.Record{}
	for _, r := range recs {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Recarto Memory — %s\n\n", repoName)
	fmt.Fprintf(&b, "_%d active memories_\n", len(recs))

	for _, sec := range kindSections {
		group := byKind[sec.kind]
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", sec.heading)
		for _, r := range group {
			title := r.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- **%s**\n", title)
			if r.Body != "" {
				fmt.Fprintf(&b, "  %s\n", r.Body)
			}
			fmt.Fprintf(&b, "  _updated %s · reinforced ×%d · scope %s · refs: %s_\n",
				time.UnixMilli(r.UpdatedAt).UTC().Format("2006-01-02"),
				r.ReinforcementCount, scope(r.Branch), refList(r.Refs))
		}
	}
	return b.String()
}

func scope(branch string) string {
	if branch == "" {
		return "global"
	}
	return branch
}

func refList(refs []memory.Ref) string {
	if len(refs) == 0 {
		return "none"
	}
	vs := make([]string, len(refs))
	for i, r := range refs {
		vs[i] = r.Value
	}
	return strings.Join(vs, ", ")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/export/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/export/markdown.go internal/export/markdown_test.go
git commit -m "feat: on-demand markdown export renderer (golden)"
```

---

### Task 4: CLI helpers + test harness + `status`

**Files:**
- Create: `internal/cli/helpers.go`
- Create: `internal/cli/helpers_test.go`
- Create: `internal/cli/status.go`
- Create: `internal/cli/status_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/cli/helpers_test.go` (the shared harness used by T4–T9, plus a `splitCSV` unit test):

```go
package cli

import (
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// fakeGit is a gitctx.Runner returning a fixed repo identity (no real git, no fs).
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

// newTestApp builds an App against a temp store with a fixed branch and clock.
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

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a.go , , b.go ,")
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Errorf("splitCSV = %#v, want [a.go b.go]", got)
	}
	if splitCSV("   ") != nil {
		t.Error("blank input should yield nil")
	}
}
```

`internal/cli/status_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestStatusReportsIdentityCountsAndLedger(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: "use wal mode for cross process writers"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Status(ctx, a, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"branch:   main", "learning", "total", "memories_recorded"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — `undefined: splitCSV` / `undefined: Status`.

- [ ] **Step 3a: Write the helpers** (`internal/cli/helpers.go`)

```go
// Package cli implements Recarto's command-line surface over internal/app, using
// the stdlib flag package (not cobra — see the Phase 1C decision log). Each
// command is a function taking an *app.App and an io.Writer so it is unit-testable
// without spawning a process or touching real git.
package cli

import (
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// splitCSV splits a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// refsFromFlags builds structural refs from the comma-separated --files/--symbols
// /--tags flag values.
func refsFromFlags(files, symbols, tags string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range splitCSV(files) {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: f})
	}
	for _, s := range splitCSV(symbols) {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range splitCSV(tags) {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
}

// branchFor returns the branch to tag a new memory with: the current branch, or
// "" (repo-global → stored as NULL) when --global is set.
func branchFor(a *app.App, global bool) string {
	if global {
		return ""
	}
	return a.Git.Branch
}
```

- [ ] **Step 3b: Write the `status` command** (`internal/cli/status.go`)

```go
package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Status prints repo identity, per-kind active memory counts, and lifetime
// ledger counters.
func Status(ctx context.Context, a *app.App, w io.Writer) error {
	fmt.Fprintf(w, "repo:     %s (%s)\n", a.Git.RepoName, a.Git.RepoID)
	fmt.Fprintf(w, "branch:   %s\n", a.Git.Branch)
	fmt.Fprintf(w, "worktree: %s\n", a.Git.Worktree)

	rows, err := a.Store.DB.QueryContext(ctx,
		`SELECT kind, count(*) FROM memories WHERE status='active' GROUP BY kind ORDER BY kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Fprintln(w, "memories:")
	total := 0
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-14s %d\n", kind, n)
		total += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-14s %d\n", "total", total)

	fmt.Fprintln(w, "ledger (lifetime):")
	for _, m := range []string{
		"memories_recorded", "memories_recalled", "recall_tokens_injected",
		"recall_misses", "reads", "repeated_reads_blocked",
	} {
		v, err := a.Ledger.Get(ctx, "lifetime", m)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-24s %d\n", m, v)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS (`TestSplitCSV`, `TestStatusReportsIdentityCountsAndLedger`).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/helpers.go internal/cli/helpers_test.go internal/cli/status.go internal/cli/status_test.go
git commit -m "feat: cli helpers + test harness + status command"
```

---

### Task 5: `doctor` command

**Files:**
- Create: `internal/cli/doctor.go`
- Create: `internal/cli/doctor_test.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/doctor_test.go`)

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDoctorPasses(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Doctor(context.Background(), a, &buf); err != nil {
		t.Fatalf("doctor returned error: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"[ok] wal", "[ok] integrity", "[ok] schema", "[ok] git"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestDoctorPasses -v`
Expected: FAIL — `undefined: Doctor`.

- [ ] **Step 3: Write the implementation** (`internal/cli/doctor.go`)

```go
package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Doctor runs health checks: store reachable, WAL enabled, integrity ok, a schema
// version applied, and git identity resolved. It returns an error if any fail so
// the process exits non-zero.
func Doctor(ctx context.Context, a *app.App, w io.Writer) error {
	allOK := true
	check := func(name string, pass bool, detail string) {
		status := "ok"
		if !pass {
			status, allOK = "FAIL", false
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", status, name, detail)
	}

	var jm string
	if err := a.Store.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&jm); err != nil {
		return err
	}
	check("wal", jm == "wal", jm)

	var integ string
	if err := a.Store.DB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integ); err != nil {
		return err
	}
	check("integrity", integ == "ok", integ)

	var uv int
	if err := a.Store.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&uv); err != nil {
		return err
	}
	check("schema", uv >= 1, fmt.Sprintf("user_version=%d", uv))

	check("git", a.Git.RepoID != "", fmt.Sprintf("repo=%s branch=%s nongit=%v",
		a.Git.RepoName, a.Git.Branch, a.Git.NonGit))

	if !allOK {
		return fmt.Errorf("doctor: one or more checks failed")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestDoctorPasses -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go
git commit -m "feat: doctor command (wal/integrity/schema/git checks)"
```

---

### Task 6: `remember` command

**Files:**
- Create: `internal/cli/remember.go`
- Create: `internal/cli/remember_test.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/remember_test.go`)

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRememberRecordsThenReinforces(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	var buf bytes.Buffer
	if err := Remember(ctx, a, &buf, []string{"learning",
		"--body", "use wal mode for cross process writers",
		"--files", "internal/store/store.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "recorded learning ") {
		t.Errorf("unexpected output: %q", buf.String())
	}

	// A near-duplicate of the same kind reinforces instead of inserting.
	buf.Reset()
	if err := Remember(ctx, a, &buf, []string{"learning",
		"--body", "use wal mode for cross process writer"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "reinforced learning ") {
		t.Errorf("expected reinforce, got: %q", buf.String())
	}
}

func TestRememberRejectsBadKindAndMissingBody(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := Remember(ctx, a, &bytes.Buffer{}, []string{"bogus", "--body", "x"}); err == nil {
		t.Error("expected error for invalid kind")
	}
	if err := Remember(ctx, a, &bytes.Buffer{}, []string{"learning"}); err == nil {
		t.Error("expected error for missing --body")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRemember -v`
Expected: FAIL — `undefined: Remember`.

- [ ] **Step 3: Write the implementation** (`internal/cli/remember.go`)

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Remember records a memory of the given kind from CLI flags:
//
//	recarto remember <kind> --body "..." [--title ...] [--files a,b] [--symbols ...]
//	                        [--tags ...] [--pin] [--global]
func Remember(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("remember: kind required (preference|learning|decision|do_not_repeat|bug|action)")
	}
	kind := memory.Kind(args[0])
	if !kind.Valid() {
		return fmt.Errorf("remember: invalid kind %q", args[0])
	}

	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	fs.SetOutput(w)
	title := fs.String("title", "", "short title")
	body := fs.String("body", "", "full content (required)")
	files := fs.String("files", "", "comma-separated file refs")
	symbols := fs.String("symbols", "", "comma-separated symbol refs")
	tags := fs.String("tags", "", "comma-separated tag refs")
	pin := fs.Bool("pin", false, "pin (never evicted)")
	global := fs.Bool("global", false, "store repo-global (not branch-scoped)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *body == "" {
		return fmt.Errorf("remember: --body is required")
	}

	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:     kind,
		Title:    *title,
		Body:     *body,
		Refs:     refsFromFlags(*files, *symbols, *tags),
		Branch:   branchFor(a, *global),
		Worktree: a.Git.Worktree,
		Author:   "cli",
		Source:   "cli",
		Pinned:   *pin,
	})
	if err != nil {
		return err
	}
	verb := "recorded"
	if reinforced {
		verb = "reinforced"
	}
	fmt.Fprintf(w, "%s %s %s\n", verb, kind, id)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestRemember -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/remember.go internal/cli/remember_test.go
git commit -m "feat: remember command (record/reinforce via engine)"
```

---

### Task 7: `recall` command

**Files:**
- Create: `internal/cli/recall.go`
- Create: `internal/cli/recall_test.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/recall_test.go`)

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestRecallByFileRef(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindLearning,
		Title:  "WAL setup",
		Body:   "use wal mode and busy_timeout for cross process writers",
		Refs:   []memory.Ref{{Type: memory.RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Recall(ctx, a, &buf, []string{"--files", "internal/store/store.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "WAL setup") {
		t.Errorf("recall missing structural match:\n%s", buf.String())
	}
}

func TestRecallEmpty(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Recall(context.Background(), a, &buf, []string{"--files", "nope/missing.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no memories recalled") {
		t.Errorf("expected empty-recall notice, got: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRecall -v`
Expected: FAIL — `undefined: Recall`.

- [ ] **Step 3: Write the implementation** (`internal/cli/recall.go`)

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Recall runs a ranked, token-budgeted recall and prints the selected memories.
//
//	recarto recall [--files a,b] [--symbols x] [--kind learning] [--limit N]
//	               [--budget N] [free-text task hint...]
//
// NOTE: stdlib flag stops at the first non-flag argument, so flags must precede
// the free-text hint (e.g. `recall --files a.go fix the leak`).
func Recall(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(w)
	files := fs.String("files", "", "comma-separated file refs")
	symbols := fs.String("symbols", "", "comma-separated symbol refs")
	kind := fs.String("kind", "", "restrict to a kind")
	limit := fs.Int("limit", 0, "max candidates (0=default)")
	budget := fs.Int("budget", 0, "token budget (0=default)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint:    strings.Join(fs.Args(), " "),
		Files:       splitCSV(*files),
		Symbols:     splitCSV(*symbols),
		Kind:        memory.Kind(*kind),
		Branch:      a.Git.Branch,
		Limit:       *limit,
		TokenBudget: *budget,
	})
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(w, "(no memories recalled)")
		return nil
	}
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• [%s] %s  (score %.3f)\n", r.Record.Kind, title, r.Score)
		if r.Record.Body != "" {
			fmt.Fprintf(w, "    %s\n", r.Record.Body)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestRecall -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/recall.go internal/cli/recall_test.go
git commit -m "feat: recall command (ranked, budgeted, branch-aware)"
```

---

### Task 8: `bug` command (`add` / `search`)

**Files:**
- Create: `internal/cli/bug.go`
- Create: `internal/cli/bug_test.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/bug_test.go`)

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestBugAddThenSearch(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	var buf bytes.Buffer
	if err := Bug(ctx, a, &buf, []string{"add",
		"--error", "nil pointer in session refresh",
		"--cause", "missing nil guard",
		"--fix", "add a nil check before deref",
		"--file", "internal/auth/session.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "recorded bug ") {
		t.Errorf("unexpected add output: %q", buf.String())
	}

	buf.Reset()
	if err := Bug(ctx, a, &buf, []string{"search", "session", "refresh"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "nil pointer in session refresh") {
		t.Errorf("search missed the bug:\n%s", out)
	}
	if !strings.Contains(out, "add a nil check before deref") {
		t.Errorf("search should surface the fix:\n%s", out)
	}
}

func TestBugRequiresSubcommand(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Bug(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no subcommand given")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestBug -v`
Expected: FAIL — `undefined: Bug`.

- [ ] **Step 3: Write the implementation** (`internal/cli/bug.go`)

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Bug dispatches `bug add` and `bug search`.
func Bug(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("bug: subcommand required (add|search)")
	}
	switch args[0] {
	case "add":
		return bugAdd(ctx, a, w, args[1:])
	case "search":
		return bugSearch(ctx, a, w, args[1:])
	default:
		return fmt.Errorf("bug: unknown subcommand %q (add|search)", args[0])
	}
}

// bugAdd records a kind=bug memory with structured OpenWolf-style fields in Extra.
func bugAdd(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("bug add", flag.ContinueOnError)
	fs.SetOutput(w)
	msg := fs.String("error", "", "error message or user complaint (required)")
	cause := fs.String("cause", "", "root cause")
	fix := fs.String("fix", "", "what fixed it")
	file := fs.String("file", "", "file that was fixed")
	tags := fs.String("tags", "", "comma-separated tags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *msg == "" {
		return fmt.Errorf("bug add: --error is required")
	}

	extra := map[string]any{
		"error_message": *msg,
		"root_cause":    *cause,
		"fix":           *fix,
		"file":          *file,
	}
	if tg := splitCSV(*tags); len(tg) > 0 {
		extra["tags"] = tg
	}
	var refs []memory.Ref
	if *file != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: *file})
	}
	// The body carries error + fix so the lexical index can match either.
	body := *msg
	if *fix != "" {
		body = *msg + " — fix: " + *fix
	}

	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:     memory.KindBug,
		Title:    *msg,
		Body:     body,
		Refs:     refs,
		Branch:   a.Git.Branch,
		Worktree: a.Git.Worktree,
		Author:   "cli",
		Source:   "cli",
		Extra:    extra,
	})
	if err != nil {
		return err
	}
	verb := "recorded"
	if reinforced {
		verb = "reinforced"
	}
	fmt.Fprintf(w, "%s bug %s\n", verb, id)
	return nil
}

// bugSearch is OpenWolf "bug search" parity: a recall restricted to kind=bug.
func bugSearch(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	results, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: strings.Join(args, " "),
		Kind:     memory.KindBug,
		Branch:   a.Git.Branch,
	})
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(w, "(no matching bugs)")
		return nil
	}
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• %s  (score %.3f)\n", title, r.Score)
		if fix, ok := r.Record.Extra["fix"].(string); ok && fix != "" {
			fmt.Fprintf(w, "    fix: %s\n", fix)
		}
	}
	return nil
}
```

> Note: `Recall` always includes pinned `do_not_repeat` (budget-exempt), so `bug search` can occasionally surface a pinned `do_not_repeat` alongside bugs. That mirrors the engine's documented recall semantics (spec §9.2) and is acceptable.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestBug -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/bug.go internal/cli/bug_test.go
git commit -m "feat: bug add/search commands"
```

---

### Task 9: `memory show` command

**Files:**
- Create: `internal/cli/memory.go`
- Create: `internal/cli/memory_test.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/memory_test.go`)

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestMemoryShowRendersMarkdown(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindDecision,
		Title:  "sqlite is source of truth",
		Body:   "markdown is render-only so it cannot drift",
		Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Memory(ctx, a, &buf, []string{"show"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "# Recarto Memory — repo") {
		t.Errorf("missing export header:\n%s", out)
	}
	if !strings.Contains(out, "## Decisions") || !strings.Contains(out, "sqlite is source of truth") {
		t.Errorf("decision not rendered:\n%s", out)
	}
}

func TestMemoryRequiresShow(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Memory(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error without a subcommand")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestMemory -v`
Expected: FAIL — `undefined: Memory`.

- [ ] **Step 3: Write the implementation** (`internal/cli/memory.go`)

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/export"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Memory dispatches `memory show` — render all active memory as markdown to w.
func Memory(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "show" {
		return fmt.Errorf("memory: subcommand required (show)")
	}
	fs := flag.NewFlagSet("memory show", flag.ContinueOnError)
	fs.SetOutput(w)
	branch := fs.String("branch", "", "limit to a branch (default: all branches)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	recs, err := a.Engine.List(ctx, memory.ListFilter{Branch: *branch})
	if err != nil {
		return err
	}
	fmt.Fprint(w, export.Render(a.Git.RepoName, recs))
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestMemory -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/memory.go internal/cli/memory_test.go
git commit -m "feat: memory show command (markdown export to stdout)"
```

---

### Task 10: Dispatch (`cli.Run`) + wire `main.go`

**Files:**
- Create: `internal/cli/cli.go`
- Create: `internal/cli/cli_test.go`
- Modify: `cmd/recarto/main.go`

- [ ] **Step 1: Write the failing test** (`internal/cli/cli_test.go`)

```go
package cli

import "testing"

func TestRunUnknownCommandErrors(t *testing.T) {
	// Unknown commands must error without ever opening a store.
	if err := Run([]string{"frobnicate"}); err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestRunNoArgsShowsUsage(t *testing.T) {
	if err := Run(nil); err != nil {
		t.Errorf("no-args Run should print usage and return nil, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRun -v`
Expected: FAIL — `undefined: Run`.

- [ ] **Step 3: Write the dispatcher** (`internal/cli/cli.go`)

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
	case "status", "doctor", "remember", "recall", "bug", "memory":
		return true
	}
	return false
}

// Run dispatches a CLI invocation (args excludes the program name). It validates
// the command before opening the per-repo App, so unknown commands and `help`
// never touch the store.
func Run(args []string) error {
	if len(args) == 0 {
		return usage(os.Stdout)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return usage(os.Stdout)
	}
	if !known(cmd) {
		return fmt.Errorf("unknown command %q (try: status doctor remember recall bug memory)", cmd)
	}

	a, err := app.Open(app.Config{})
	if err != nil {
		return err
	}
	defer a.Close()

	ctx := context.Background()
	w := os.Stdout
	switch cmd {
	case "status":
		return Status(ctx, a, w)
	case "doctor":
		return Doctor(ctx, a, w)
	case "remember":
		return Remember(ctx, a, w, rest)
	case "recall":
		return Recall(ctx, a, w, rest)
	case "bug":
		return Bug(ctx, a, w, rest)
	case "memory":
		return Memory(ctx, a, w, rest)
	}
	return nil // unreachable: known() gates the switch
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "recarto <command>")
	fmt.Fprintln(w, "  status            repo id, branch, memory counts, ledger")
	fmt.Fprintln(w, "  doctor            health checks (db, WAL, integrity, git)")
	fmt.Fprintln(w, "  remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)")
	fmt.Fprintln(w, "  recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)")
	fmt.Fprintln(w, "  bug add|search    record or search bug memories")
	fmt.Fprintln(w, "  memory show       render all active memory as markdown (--branch)")
	return nil
}
```

- [ ] **Step 4a: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestRun -v`
Expected: PASS.

- [ ] **Step 4b: Wire `main.go`** — replace `cmd/recarto/main.go`'s `main` function so the default branch delegates to `cli.Run` (keep `version`, `--version`, and `debug-stress`, and keep the `runDebugStress` function and its imports unchanged):

```go
// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/cli"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		if err := cli.Run(nil); err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(version.Version)
	case "debug-stress":
		if err := runDebugStress(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "debug-stress:", err)
			os.Exit(1)
		}
	default:
		if err := cli.Run(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
	}
}
```

Keep the existing `runDebugStress` function below `main` exactly as it is (it still uses `flag`, `context`, `store`, and `ledger`).

- [ ] **Step 4c: Build, vet, full suite, race, and gofmt**

Run:
```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l .
```
Expected: build/vet clean; all tests PASS; `gofmt -l .` prints nothing.

- [ ] **Step 4d: Smoke-test the binary end-to-end** (proves the surfaces work against a real, throwaway store + the repo's own git):

```bash
export RECARTO_CACHE_DIR="$(mktemp -d)"
go run ./cmd/recarto remember learning --body "recarto stores memory in a per-repo sqlite cache" --files internal/store/store.go
go run ./cmd/recarto recall --files internal/store/store.go
go run ./cmd/recarto bug add --error "example failure" --fix "the example fix"
go run ./cmd/recarto bug search example
go run ./cmd/recarto memory show
go run ./cmd/recarto status
go run ./cmd/recarto doctor
```
Expected: `remember` prints `recorded learning <id>`; `recall` lists the learning; `bug search` finds the bug with its fix; `memory show` renders markdown with `## Learnings` and `## Bugs`; `status` shows counts and `memories_recorded`≥2; `doctor` prints all `[ok]` and exits 0.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go cmd/recarto/main.go
git commit -m "feat: wire cli.Run dispatch into recarto main"
```

---

## Self-Review

**1. Spec coverage (§12 CLI + §7/§8 wiring + §10 ledger + on-demand markdown):**
- `remember <kind>` → T6 (`Remember`). ✅
- `bug add` / `bug search` → T8 (`Bug`). ✅
- `recall [query] [--files …]` → T7 (`Recall`, branch-aware via `app.Git.Branch`). ✅
- `memory show` (markdown export to stdout) → T3 (`export.Render`) + T9 (`Memory`). ✅
- `status` (repo-id, branch, worktree, db path identity, counts, ledger) → T4 (`Status`). ✅ *(db path itself is implicit via the cache; identity + counts + ledger are shown. If a literal db path line is wanted, add `a.Store` exposes none today — out of 1C-1 scope; status already prints repo id/branch/worktree.)*
- `doctor` (db reachable, WAL on, integrity_check, git resolution, migration version) → T5 (`Doctor`). ✅
- `init`, `serve`, `hook` → **deferred to 1C-2** (MCP + Claude Code), per the split decision. Not a gap in 1C-1.
- Branch/worktree wiring of the git-agnostic engine (overview "gitctx wiring of branch/worktree into recall") → T1 (`app.Git`) consumed by T6/T7/T8 (`Branch`/`Worktree` on `Record`, `Branch` on `RecallQuery`). ✅
- On-demand markdown, single source of truth (D2) → export is render-only, no file written. ✅
- Token ledger surfaced (§10) → `Status` reads the lifetime counters the engine already writes. No new ledger primitives needed (estimator + counters built in 1A/1B). ✅

**2. Placeholder scan:** No TBD/TODO/"handle errors appropriately"/"similar to Task N". Every code step contains complete, compilable code; every test asserts concrete behavior. ✅

**3. Type/signature consistency (checked against the real 1A/1B source):**
- `gitctx.Resolve(runner, cwd)`, `gitctx.Real{Dir:}`, `gitctx.Runner.Run(...)`, `Context{RepoID,RepoName,Branch,Worktree,RootCommit,NonGit}` — matches `internal/gitctx/gitctx.go`. ✅
- `cache.StorePath(repoID)` — matches `internal/cache/cache.go`. ✅
- `store.Open` (creates dirs + migrates), `st.DB`, `st.WriteTx(ctx, func(c store.Conn) error)`, `st.Close()` — matches `internal/store/store.go`. ✅
- `ledger.New(st)`, `l.Get(ctx, scope, metric)` — matches `internal/ledger/ledger.go`. ✅
- `memory.New(st, led, clk)`, `Record(ctx, NewMemory) (string,bool,error)`, `Recall(ctx, RecallQuery) ([]RecallResult,error)`, `RecallResult.Record/.Score`, `NewMemory`/`Ref`/`Kind`/`RecallQuery`/`Record` fields, `Kind.Valid()` — matches `internal/memory/{engine,recall,record}.go`. ✅
- `Engine.List` reuses in-package `selectMemoriesSQL`, `scanMemories`, `loadRefs` (all in `engine.go`) — `list.go` is `package memory`, so they are accessible. ✅
- `clock.NewFake(time.Time)`, `clock.Real{}`, `version.Version` — matches 1A. ✅
- CLI command function signature `func(ctx, *app.App, io.Writer[, args]) error` is uniform across T4–T9 and matches the `cli.Run` call sites in T10. ✅

**4. TDD ordering:** every task is RED (failing test) → GREEN (minimal code) → commit; command functions (T4–T9) are tested directly so each task compiles and passes before the dispatcher (T10) exists. ✅

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-03-recarto-phase1c1-cli-and-export.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration (`superpowers:subagent-driven-development`).

**2. Inline Execution** — execute tasks in this session with checkpoints (`superpowers:executing-plans`).

After 1C-1 is green and merged, **1C-2** covers the MCP server (official Go SDK, version-pinned, behind a thin handler interface) and Claude Code integration (`init` installer + `serve` + `hook session-start|post-read|stop`), completing Phase 1.
