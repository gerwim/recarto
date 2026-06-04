# Recarto Multi-Agent Support — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add OpenCode and GitHub Copilot CLI integration to Recarto with feature parity to Claude Code, plus a cross-compile Makefile and a rewritten README.

**Architecture:** Extract the agent-neutral hook logic into a new `internal/agents` package (`Handle(ctx,a,Event)→Result`) behind an `Adapter` interface that each host implements for payload `Decode`/`Encode`. `claudecode` is refactored to one adapter (its public funcs become thin wrappers, so existing tests stay green); `opencode` and `copilot` are new adapter+installer packages. `recarto init`/`recarto hook` gain `--agent`/`--scope`. The `recarto serve` MCP server is reused unchanged.

**Tech Stack:** Go 1.26.4, stdlib `flag`/`encoding/json`, CGO tree-sitter (cross-compile via `zig cc`), modernc sqlite.

**Spec:** `docs/superpowers/specs/2026-06-04-recarto-multi-agent-support-design.md`

---

## File Structure

**New — shared core `internal/agents`:**
- `event.go` — `EventKind`, `Event`, `Result`, `Adapter` interface, `RunHook`, `KindFromString`.
- `handle.go` — `Handle` + all logic moved verbatim from `claudecode/hooks.go` & `graphhooks.go`.
- `format.go` — `formatSessionContext`/`formatBugWarnings`/`trimOneLine`/`nullable` + budget consts/env (moved).
- `jsonmerge.go` — `ReadJSONObject`/`WriteJSONObject` (moved & exported from `claudecode/settings.go`).
- `instructions.go` — canonical `Instructions` content + `WriteInstructionsBlock` (delimited, idempotent).
- `install.go` — `Scope`, `Options`, path helpers (`UserHome`, repo vs user resolution), `Detect()`.

**Refactored — `internal/agents/claudecode`:**
- `adapter.go` — `Adapter` impl (Decode/Encode CC shapes) + public `SessionStart/PostRead/PostEdit/Augment/Stop` wrappers calling `agents.RunHook`.
- `install.go` — scope-aware installer using shared helpers.
- delete logic from `hooks.go`/`graphhooks.go` (moved to `agents`); keep `settings.go` MCP/hook merge helpers (`ensureMCPServer`,`hasCommand`,`appendHook`) — these stay CC-specific.

**New — `internal/agents/opencode`:** `adapter.go`, `install.go`, `plugin.go` (JS template), tests.
**New — `internal/agents/copilot`:** `adapter.go`, `install.go`, `hooks_json.go` (hook-file template), tests.

**Modified:**
- `internal/cli/hook.go` — `--agent` selects adapter via `agents.RunHook`.
- `internal/cli/init.go` + `internal/cli/cli.go` — `--agent`/`--scope`, auto-detect, scope prompt, usage text.
- `internal/version/version.go` — `const` → `var`.

**New (root):** `Makefile`, rewritten `README.md`.

---

## Task 1: Extract agent-neutral core types

**Files:**
- Create: `internal/agents/event.go`
- Test: `internal/agents/event_test.go`

- [ ] **Step 1: Write the failing test**

```go
package agents

import "testing"

func TestKindFromString(t *testing.T) {
	cases := map[string]EventKind{
		"session-start": KindSessionStart,
		"post-read":     KindPostRead,
		"post-edit":     KindPostEdit,
		"augment":       KindAugment,
		"stop":          KindStop,
	}
	for s, want := range cases {
		got, err := KindFromString(s)
		if err != nil || got != want {
			t.Errorf("KindFromString(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := KindFromString("nope"); err == nil {
		t.Error("expected error for unknown event")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run TestKindFromString`
Expected: FAIL (package/undefined).

- [ ] **Step 3: Write minimal implementation**

```go
// Package agents holds Recarto's agent-neutral lifecycle-hook engine. Host
// adapters (claudecode, opencode, copilot) translate each tool's payloads to/from
// the neutral Event/Result types; Handle runs the shared logic in between.
package agents

import (
	"context"
	"fmt"
	"io"
)

// EventKind enumerates the lifecycle events Recarto reacts to.
type EventKind int

const (
	KindSessionStart EventKind = iota
	KindPostRead
	KindPostEdit
	KindAugment
	KindStop
)

// Event is the agent-neutral hook input. Adapters populate the fields relevant to
// the Kind; unused fields stay zero.
type Event struct {
	Kind       EventKind
	Agent      string // sessions.agent: "claude-code" | "opencode" | "copilot"
	SessionID  string
	Source     string
	FilePath   string // absolute path (PostRead/PostEdit)
	Pattern    string // grep/glob query (Augment)
	ToolResult string // raw tool result (PostRead token estimate)
	StopActive bool
}

// Result is the agent-neutral hook output. Context is injected as the host's
// "additional context"; SystemMessage is the Stop nudge. Either may be empty.
type Result struct {
	Context       string
	SystemMessage string
}

// Adapter converts a host's hook payloads to/from the neutral types.
type Adapter interface {
	Decode(kind EventKind, in io.Reader) (Event, error)
	Encode(kind EventKind, out io.Writer, res Result) error
}

// KindFromString maps a CLI event token to an EventKind.
func KindFromString(s string) (EventKind, error) {
	switch s {
	case "session-start":
		return KindSessionStart, nil
	case "post-read":
		return KindPostRead, nil
	case "post-edit":
		return KindPostEdit, nil
	case "augment":
		return KindAugment, nil
	case "stop":
		return KindStop, nil
	}
	return 0, fmt.Errorf("unknown event %q (session-start|post-read|post-edit|augment|stop)", s)
}

// RunHook decodes the event with ad, runs the shared Handle, and encodes the
// result back through ad. Wired by each adapter's public hook functions.
func RunHook(ctx context.Context, a HandleApp, ad Adapter, kind EventKind, in io.Reader, out io.Writer) error {
	ev, err := ad.Decode(kind, in)
	if err != nil {
		return err
	}
	res, err := Handle(ctx, a, ev)
	if err != nil {
		return err
	}
	return ad.Encode(kind, out, res)
}
```

> `HandleApp` is defined in Task 2 (it is `*app.App`; introduced there to avoid an import cycle note). If you prefer, use `*app.App` directly here — there is no cycle because `agents` imports `app`, and `app` does not import `agents`. Use `*app.App` directly:
> replace `a HandleApp` with `a *app.App` and add the import `"git.gerwim.nl/gerwim/recarto/internal/app"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/ -run TestKindFromString`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/event.go internal/agents/event_test.go
git commit -m "feat(agents): agent-neutral Event/Result/Adapter types + RunHook"
```

---

## Task 2: Move the hook logic into `agents.Handle`

This is a behavior-preserving move from `claudecode/hooks.go` and `claudecode/graphhooks.go`. The logic is unchanged; only its shape (Event in, Result out) changes.

**Files:**
- Create: `internal/agents/handle.go`, `internal/agents/format.go`
- Modify: `internal/agents/event.go` (use `*app.App` per the note above)
- Test: `internal/agents/handle_test.go`

- [ ] **Step 1: Write the failing test** (drives the core directly)

```go
package agents

import (
	"context"
	"strings"
	"testing"
)

func TestHandleSessionStartEmptyRepo(t *testing.T) {
	a := newTestApp(t) // harness mirrored from internal/cli/helpers_test.go (fake git, temp cache, fake clock)
	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "opencode", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Recarto memory for this repo/branch") {
		t.Errorf("missing session context header:\n%s", res.Context)
	}
	if !strings.Contains(res.Context, "record_memory") {
		t.Errorf("missing record nudge:\n%s", res.Context)
	}
}

func TestHandleStopNudge(t *testing.T) {
	a := newTestApp(t)
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "copilot", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "record any new learnings") {
		t.Errorf("missing stop nudge: %q", res.SystemMessage)
	}
}
```

Copy the `newTestApp` harness into `internal/agents/handle_test.go` from `internal/cli/helpers_test.go` (fake `gitctx.Runner`, `RECARTO_CACHE_DIR` temp dir, fake clock) — the cerebrum notes this harness is duplicated per test package (no shared testutil yet). Read `internal/cli/helpers_test.go` first and adapt the package name.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run TestHandle`
Expected: FAIL (`Handle` undefined).

- [ ] **Step 3: Write the implementation by moving logic**

Create `internal/agents/handle.go` with:

```go
package agents

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Handle runs the agent-neutral logic for one lifecycle event and returns the
// neutral result. It is exit-0 advisory: callers treat any error as non-blocking.
func Handle(ctx context.Context, a *app.App, ev Event) (Result, error) {
	switch ev.Kind {
	case KindSessionStart:
		return handleSessionStart(ctx, a, ev)
	case KindPostRead:
		return handlePostRead(ctx, a, ev)
	case KindPostEdit:
		return handlePostEdit(ctx, a, ev)
	case KindAugment:
		return handleAugment(ctx, a, ev)
	case KindStop:
		return handleStop(ctx, a, ev)
	}
	return Result{}, nil
}
```

Then port each handler from the existing code, changing ONLY the I/O boundary:
- `handleSessionStart`: body = `claudecode.SessionStart` minus the `decode(in,&hi)` (use `ev.SessionID`) and minus `writeHookContext` (return `Result{Context: formatSessionContext(...)}`). Replace the hardcoded `'claude-code'` in the `sessions` INSERT with `ev.Agent` (fall back to `"claude-code"` if empty — keep a small `agentOrDefault(ev.Agent)` helper).
- `handlePostRead`: body = `claudecode.PostRead` minus decode (use `ev.FilePath`, `ev.SessionID`, `ev.ToolResult`); accumulate `msgs` and return `Result{Context: strings.Join(msgs,"\n")}` (empty Context when none).
- `handlePostEdit`: body = `claudecode.PostEdit` minus decode (use `ev.FilePath`); collect any bug-warning text into `Result.Context` instead of calling `writeHookContext`; keep the `IndexFile`+`Resolve` side effects.
- `handleAugment`: body = `claudecode.Augment` minus decode (use `ev.Pattern`); return `Result{Context: b.String()}`.
- `handleStop`: body = `claudecode.Stop` minus decode (use `ev.SessionID`); return `Result{SystemMessage: "recarto: record any new learnings (record_memory) and bug fixes (record_bug) before ending."}`.

Move `formatSessionContext`, `formatBugWarnings`, `trimOneLine`, `nullable`, the budget consts (`sessionStartBudget`,`recentBugLimit`,`archModuleLimit`,`archDepLimit`,`cerebrumReserve`,`cerebrumBodyMax`) and `sessionBudget()`/`RECARTO_SESSION_BUDGET` into `internal/agents/format.go` verbatim.

Add helper:
```go
func agentOrDefault(s string) string {
	if s == "" {
		return "claude-code"
	}
	return s
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/ -run TestHandle`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/
git commit -m "feat(agents): move neutral hook logic into agents.Handle"
```

---

## Task 3: Move JSON-merge helpers to `agents`

**Files:**
- Create: `internal/agents/jsonmerge.go`, `internal/agents/jsonmerge_test.go`
- Modify: `internal/agents/claudecode/settings.go` (drop the moved funcs, call shared ones)

- [ ] **Step 1: Write the failing test**

```go
package agents

import "testing"

func TestReadWriteJSONObjectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sub/config.json"
	m, err := ReadJSONObject(path)
	if err != nil || len(m) != 0 {
		t.Fatalf("missing file → empty map: %v %v", m, err)
	}
	m["x"] = "y"
	if err := WriteJSONObject(path, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadJSONObject(path)
	if got["x"] != "y" {
		t.Fatalf("round trip failed: %v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run TestReadWriteJSONObjectRoundTrip`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement**

Move `readJSONObject`→`ReadJSONObject` and `writeJSONObject`→`WriteJSONObject` from `claudecode/settings.go` into `internal/agents/jsonmerge.go` (exported). In `claudecode/settings.go`, delete those two funcs and update `ensureMCPServer`/`Install` to call `agents.ReadJSONObject`/`agents.WriteJSONObject`. Keep `ensureMCPServer`/`hasCommand`/`appendHook` in `claudecode` (CC-specific shapes).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/... ./internal/agents/claudecode/...`
Expected: PASS (existing claudecode tests still green).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/jsonmerge.go internal/agents/jsonmerge_test.go internal/agents/claudecode/settings.go
git commit -m "refactor(agents): share JSON object read/write helpers"
```

---

## Task 4: Convert `claudecode` hook funcs to adapter wrappers

Keep the public signatures (`SessionStart(ctx,a,in,out)` …) so `internal/cli` and the existing `claudecode` hook tests stay green.

**Files:**
- Create: `internal/agents/claudecode/adapter.go`
- Modify: `internal/agents/claudecode/hooks.go`, `internal/agents/claudecode/graphhooks.go` (remove moved logic; keep only the input structs the adapter needs, or move them into adapter.go)

- [ ] **Step 1: Write the failing test**

```go
package claudecode

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestAdapterEncodeSessionStart(t *testing.T) {
	var sb strings.Builder
	err := Adapter{}.Encode(agents.KindSessionStart, &sb, agents.Result{Context: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"hookSpecificOutput"`) ||
		!strings.Contains(out, `"additionalContext"`) ||
		!strings.Contains(out, "hello") {
		t.Errorf("unexpected CC envelope:\n%s", out)
	}
}

func TestAdapterDecodePostEditFilePath(t *testing.T) {
	in := strings.NewReader(`{"session_id":"s","tool_input":{"file_path":"/x/y.go"}}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/x/y.go" || ev.Agent != "claude-code" {
		t.Errorf("decoded %+v", ev)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/claudecode/ -run TestAdapter`
Expected: FAIL (undefined `Adapter`).

- [ ] **Step 3: Implement `adapter.go`**

```go
package claudecode

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Adapter translates Claude Code hook payloads to/from agents.Event/Result.
type Adapter struct{}

func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string `json:"session_id"`
		Source     string `json:"source"`
		ToolInput  struct {
			FilePath string `json:"file_path"`
			Pattern  string `json:"pattern"`
			Query    string `json:"query"`
		} `json:"tool_input"`
		ToolResult     json.RawMessage `json:"tool_result"`
		StopHookActive bool            `json:"stop_hook_active"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	pattern := p.ToolInput.Pattern
	if pattern == "" {
		pattern = p.ToolInput.Query
	}
	return agents.Event{
		Kind: kind, Agent: "claude-code",
		SessionID: p.SessionID, Source: p.Source,
		FilePath: p.ToolInput.FilePath, Pattern: pattern,
		ToolResult: string(p.ToolResult), StopActive: p.StopHookActive,
	}, nil
}

func (Adapter) Encode(kind agents.EventKind, out io.Writer, res agents.Result) error {
	if kind == agents.KindStop {
		return json.NewEncoder(out).Encode(map[string]any{"systemMessage": res.SystemMessage})
	}
	if res.Context == "" {
		return nil // nothing to inject
	}
	event := "SessionStart"
	if kind != agents.KindSessionStart {
		event = "PostToolUse"
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": res.Context,
		},
	})
}

// Public hook entrypoints (stable API for cli + tests): decode → Handle → encode.
func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindSessionStart, in, out)
}
func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostRead, in, out)
}
func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostEdit, in, out)
}
func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindAugment, in, out)
}
func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindStop, in, out)
}
```

Delete the now-moved bodies and helpers from `hooks.go`/`graphhooks.go` (the input structs, `decode`, `writeHookContext`, `formatBugWarnings`, `formatSessionContext`, `trimOneLine`, `nullable`, budget consts — all now in `agents`). Those files may end up empty; if so, delete them and update `.wolf/anatomy.md`.

> **Note on existing tests:** `hooks_test.go`/`graphhooks_test.go` call `SessionStart`/`PostEdit`/etc. and assert on the CC JSON envelope — they remain valid because the public funcs and output shape are unchanged. Keep them. One exception: tests that asserted the old `'claude-code'` literal still hold (default). Run them in Step 4.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/claudecode/... ./internal/agents/...`
Expected: PASS (adapter tests + all ported hook tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/
git commit -m "refactor(claudecode): hook funcs become thin adapter wrappers over agents.Handle"
```

---

## Task 5: Scope + instructions + detection (shared install scaffolding)

**Files:**
- Create: `internal/agents/install.go`, `internal/agents/instructions.go`, plus tests `install_test.go`, `instructions_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInstructionsBlockIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("# My rules\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := WriteInstructionsBlock(p); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "keep me") {
		t.Error("clobbered existing content")
	}
	if strings.Count(s, recartoBlockBegin) != 1 {
		t.Errorf("block not idempotent:\n%s", s)
	}
	if !strings.Contains(s, "record_memory") {
		t.Error("missing recarto instructions body")
	}
}

func TestScopePathsRepoVsUser(t *testing.T) {
	r := Paths{Scope: ScopeRepo, Dir: "/repo", Home: "/home/u"}
	if r.RepoFile("AGENTS.md") != "/repo/AGENTS.md" {
		t.Errorf("repo file = %s", r.RepoFile("AGENTS.md"))
	}
	u := Paths{Scope: ScopeUser, Dir: "/repo", Home: "/home/u"}
	if u.UserFile(".copilot", "mcp-config.json") != "/home/u/.copilot/mcp-config.json" {
		t.Errorf("user file = %s", u.UserFile(".copilot", "mcp-config.json"))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run 'TestWriteInstructionsBlock|TestScopePaths'`
Expected: FAIL.

- [ ] **Step 3: Implement**

`instructions.go`:
```go
package agents

import (
	"os"
	"strings"
)

const (
	recartoBlockBegin = "<!-- recarto:begin -->"
	recartoBlockEnd   = "<!-- recarto:end -->"
)

// Instructions is the canonical guidance written to each host's instructions file.
const Instructions = `# Recarto

This repo uses **Recarto** for cross-session memory (preferences, learnings,
decisions, do-not-repeats, bugs) and a tree-sitter code graph, stored in a shared
per-repo SQLite cache outside the working tree.

- At session start Recarto injects the most relevant memories for this branch.
- **Record** knowledge with ` + "`record_memory`" + ` and bug fixes with ` + "`record_bug`" + `.
- **Recall** with ` + "`recall_memory`" + ` and ` + "`search_bugs`" + ` before changing code.
- Query the code graph with ` + "`search_graph`/`get_code_snippet`/`trace_call_path`" + `
  instead of reading whole files.

Recarto reinforces near-duplicate memories instead of duplicating, so record freely.`

// WriteInstructionsBlock inserts or replaces the recarto-delimited block in the
// file at path (creating it if absent), preserving any other content.
func WriteInstructionsBlock(path string) error {
	block := recartoBlockBegin + "\n" + Instructions + "\n" + recartoBlockEnd + "\n"
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(existing)
	if i := strings.Index(s, recartoBlockBegin); i >= 0 {
		if j := strings.Index(s, recartoBlockEnd); j > i {
			s = s[:i] + strings.TrimRight(block, "\n") + s[j+len(recartoBlockEnd):]
			return os.WriteFile(path, []byte(s), 0o644)
		}
	}
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if len(s) > 0 {
		s += "\n"
	}
	return os.WriteFile(path, []byte(s+block), 0o644)
}
```

`install.go`:
```go
package agents

import (
	"os"
	"os/exec"
	"path/filepath"
)

type Scope int

const (
	ScopeRepo Scope = iota
	ScopeUser
)

func (s Scope) String() string {
	if s == ScopeUser {
		return "user"
	}
	return "repo"
}

// Paths resolves config locations for a given scope.
type Paths struct {
	Scope Scope
	Dir   string // repo root
	Home  string // user home
}

func (p Paths) RepoFile(parts ...string) string {
	return filepath.Join(append([]string{p.Dir}, parts...)...)
}
func (p Paths) UserFile(parts ...string) string {
	return filepath.Join(append([]string{p.Home}, parts...)...)
}

// Result summarizes one agent's install for human-readable output.
type Result struct {
	Agent string
	MCP   string
	Hooks string
	Docs  string
}

// Detect returns the installed agent names (binary on PATH) among the supported set.
func Detect() []string {
	var found []string
	for _, bin := range []struct{ exe, name string }{
		{"claude", "claude"}, {"opencode", "opencode"}, {"copilot", "copilot"},
	} {
		if _, err := exec.LookPath(bin.exe); err == nil {
			found = append(found, bin.name)
		}
	}
	return found
}

// HomeDir returns the user home, honoring COPILOT_HOME callers separately.
func HomeDir() (string, error) { return os.UserHomeDir() }
```

> Note: there is already an `agents.Result` need vs the per-agent `claudecode.Result`. Keep the shared one named `InstallResult` to avoid confusion: rename `Result`→`InstallResult` in `install.go` and its test. (The neutral hook `Result` in `event.go` is the hook result — distinct name, distinct file. Using `InstallResult` for installs removes ambiguity.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/ -run 'TestWriteInstructionsBlock|TestScopePaths'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/install.go internal/agents/instructions.go internal/agents/install_test.go internal/agents/instructions_test.go
git commit -m "feat(agents): scope paths, agent detection, idempotent instructions block"
```

---

## Task 6: Refactor claudecode installer to scope-aware + AGENTS.md

**Files:**
- Modify: `internal/agents/claudecode/install.go`
- Test: update `internal/agents/claudecode/install_test.go`

- [ ] **Step 1: Update the failing test** — add scope + AGENTS.md expectations

```go
func TestInstallRepoScopeWritesAgentsAndMCP(t *testing.T) {
	dir := t.TempDir()
	res, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: t.TempDir()}, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q", res.MCP)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) {
		t.Error(".mcp.json missing recarto")
	}
	settings, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if !strings.Contains(string(settings), "recarto hook session-start --agent claude") {
		t.Errorf("settings missing scoped hook command:\n%s", settings)
	}
	agentsmd, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(agentsmd), "record_memory") {
		t.Error("AGENTS.md missing instructions")
	}
	// idempotent
	res2, _ := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: t.TempDir()}, "recarto")
	if res2.MCP != "already present" {
		t.Errorf("not idempotent: %+v", res2)
	}
}
```

Update the existing `TestInstallCreatesConfigAndIsIdempotent` to the new `Install(Paths, binary)` signature (or replace it with the above). Remove the `RECARTO.md` assertion (retired). Keep the "preserve unrelated hook" assertion.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/claudecode/ -run TestInstall`
Expected: FAIL.

- [ ] **Step 3: Implement**

Rewrite `Install` to take `agents.Paths` + binary, resolve targets by scope:
- MCP: repo→`.mcp.json`; user→`<home>/.claude.json`. Merge `mcpServers.recarto` (existing `ensureMCPServer`).
- Hooks: repo→`.claude/settings.json`; user→`<home>/.claude/settings.json`. Commands gain ` --agent claude` (update `hookSpecs` to append `--agent claude`).
- Docs: repo→`AGENTS.md` (via `agents.WriteInstructionsBlock`); user→`<home>/.claude/CLAUDE.md`.
- Return `agents.InstallResult{Agent:"claude", ...}`.

Update `hookSpecs(binary)` commands to e.g. `binary + " hook session-start --agent claude"` for all five. Delete the old `instructions`/`RECARTO.md` constant and write step.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/claudecode/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/install.go internal/agents/claudecode/install_test.go
git commit -m "refactor(claudecode): scope-aware installer, AGENTS.md, retire RECARTO.md"
```

---

## Task 7: Copilot adapter

**Files:**
- Create: `internal/agents/copilot/adapter.go`, `internal/agents/copilot/adapter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package copilot

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestDecodePostToolUseRoutesAndExtractsPath(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"edit_file","toolArgs":{"path":"/a/b.go"}}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a/b.go" || ev.Agent != "copilot" {
		t.Errorf("ev = %+v", ev)
	}
}

func TestDecodeAugmentPattern(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"grep","toolArgs":{"pattern":"foo"}}`)
	ev, _ := Adapter{}.Decode(agents.KindAugment, in)
	if ev.Pattern != "foo" {
		t.Errorf("pattern = %q", ev.Pattern)
	}
}

func TestEncodeAdditionalContext(t *testing.T) {
	var sb strings.Builder
	_ = Adapter{}.Encode(agents.KindSessionStart, &sb, agents.Result{Context: "hi"})
	if !strings.Contains(sb.String(), `"additionalContext"`) || !strings.Contains(sb.String(), "hi") {
		t.Errorf("out = %s", sb.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/copilot/ -run 'TestDecode|TestEncode'`
Expected: FAIL.

- [ ] **Step 3: Implement `adapter.go`**

```go
// Package copilot integrates Recarto with GitHub Copilot CLI: MCP registration,
// .github/hooks hook file, AGENTS.md, and the camelCase hook payload adapter.
package copilot

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

type Adapter struct{}

func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string          `json:"sessionId"`
		Source     string          `json:"source"`
		ToolName   string          `json:"toolName"`
		ToolArgs   json.RawMessage `json:"toolArgs"`
		ToolResult json.RawMessage `json:"toolResult"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	ev := agents.Event{Kind: kind, Agent: "copilot", SessionID: p.SessionID, Source: p.Source, ToolResult: string(p.ToolResult)}
	ev.FilePath = pickString(p.ToolArgs, "path", "filePath", "file_path", "file")
	ev.Pattern = pickString(p.ToolArgs, "pattern", "query", "regex")
	return ev, nil
}

func (Adapter) Encode(kind agents.EventKind, out io.Writer, res agents.Result) error {
	if kind == agents.KindStop {
		// agentStop: informational only (advisory parity; never block).
		return json.NewEncoder(out).Encode(map[string]any{})
	}
	if res.Context == "" {
		return json.NewEncoder(out).Encode(map[string]any{})
	}
	return json.NewEncoder(out).Encode(map[string]any{"additionalContext": res.Context})
}

// pickString returns the first non-empty string value among keys in a JSON object.
func pickString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindSessionStart, in, out)
}
func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostRead, in, out)
}
func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostEdit, in, out)
}
func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindAugment, in, out)
}
func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindStop, in, out)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/copilot/ -run 'TestDecode|TestEncode'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/copilot/adapter.go internal/agents/copilot/adapter_test.go
git commit -m "feat(copilot): camelCase hook adapter (decode/encode + arg extraction)"
```

---

## Task 8: Copilot installer (.mcp.json / mcp-config.json + .github/hooks + AGENTS.md)

**Files:**
- Create: `internal/agents/copilot/install.go`, `internal/agents/copilot/install_test.go`

- [ ] **Step 1: Write the failing test**

```go
package copilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestInstallRepoScope(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	res, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q", res.MCP)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json: %s", mcp)
	}
	hook, _ := os.ReadFile(filepath.Join(dir, ".github", "hooks", "recarto.json"))
	hs := string(hook)
	for _, want := range []string{`"sessionStart"`, `"postToolUse"`, `"agentStop"`, "hook session-start --agent copilot", "hook post-edit --agent copilot"} {
		if !strings.Contains(hs, want) {
			t.Errorf("hook file missing %q:\n%s", want, hs)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing: %v", err)
	}
	res2, _ := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if res2.MCP != "already present" {
		t.Errorf("not idempotent: %+v", res2)
	}
}

func TestInstallUserScopeTargets(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeUser, Dir: dir, Home: home}, "/bin/recarto"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "mcp-config.json")); err != nil {
		t.Errorf("user mcp-config.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "hooks", "recarto.json")); err != nil {
		t.Errorf("user hook file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "copilot-instructions.md")); err != nil {
		t.Errorf("user instructions missing: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/copilot/ -run TestInstall`
Expected: FAIL.

- [ ] **Step 3: Implement `install.go`**

```go
package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// hookFile is the recarto-owned Copilot CLI hook config. Copilot merges hook
// files, so a dedicated file is idempotent by construction (overwrite).
func hookFile(binary string) []byte {
	cmd := func(event string) map[string]any {
		return map[string]any{"type": "command", "bash": binary + " hook " + event + " --agent copilot"}
	}
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"sessionStart": []any{cmd("session-start")},
			"postToolUse":  []any{cmd("post-edit"), cmd("post-read"), cmd("augment")},
			"agentStop":    []any{cmd("stop")},
		},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n')
}

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
		"type": "local", "command": binary, "args": []any{"serve"}, "tools": []any{"*"},
	}
	return true
}

func Install(p agents.Paths, binary string) (agents.InstallResult, error) {
	res := agents.InstallResult{Agent: "copilot"}

	var mcpPath, hookPath, docPath string
	switch p.Scope {
	case agents.ScopeUser:
		mcpPath = p.UserFile(".copilot", "mcp-config.json")
		hookPath = p.UserFile(".copilot", "hooks", "recarto.json")
		docPath = p.UserFile(".copilot", "copilot-instructions.md")
	default:
		mcpPath = p.RepoFile(".mcp.json")
		hookPath = p.RepoFile(".github", "hooks", "recarto.json")
		docPath = p.RepoFile("AGENTS.md")
	}

	doc, err := agents.ReadJSONObject(mcpPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(doc, binary) {
		res.MCP = "configured"
	} else {
		res.MCP = "already present"
	}
	if err := agents.WriteJSONObject(mcpPath, doc); err != nil {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(hookPath, hookFile(binary), 0o644); err != nil {
		return res, err
	}
	res.Hooks = "written"

	if err := agents.WriteInstructionsBlock(docPath); err != nil {
		return res, err
	}
	res.Docs = "written"
	return res, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/copilot/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/copilot/install.go internal/agents/copilot/install_test.go
git commit -m "feat(copilot): scope-aware installer (.mcp.json/.github/hooks/AGENTS.md)"
```

---

## Task 9: OpenCode adapter (neutral JSON)

**Files:**
- Create: `internal/agents/opencode/adapter.go`, `internal/agents/opencode/adapter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package opencode

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestDecodeNeutral(t *testing.T) {
	in := strings.NewReader(`{"session_id":"s","file_path":"/a.go","pattern":"x"}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a.go" || ev.Agent != "opencode" {
		t.Errorf("ev = %+v", ev)
	}
}

func TestEncodeNeutral(t *testing.T) {
	var sb strings.Builder
	_ = Adapter{}.Encode(agents.KindSessionStart, &sb, agents.Result{Context: "ctx"})
	if !strings.Contains(sb.String(), `"additionalContext"`) || !strings.Contains(sb.String(), "ctx") {
		t.Errorf("out = %s", sb.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/opencode/ -run 'TestDecode|TestEncode'`
Expected: FAIL.

- [ ] **Step 3: Implement `adapter.go`** (neutral shapes — we own both ends via the JS plugin)

```go
// Package opencode integrates Recarto with OpenCode: opencode.json MCP, a generated
// JS plugin bridging lifecycle hooks, AGENTS.md, and a neutral hook payload adapter.
package opencode

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

type Adapter struct{}

func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string `json:"session_id"`
		Source     string `json:"source"`
		FilePath   string `json:"file_path"`
		Pattern    string `json:"pattern"`
		ToolResult string `json:"tool_result"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	return agents.Event{
		Kind: kind, Agent: "opencode",
		SessionID: p.SessionID, Source: p.Source,
		FilePath: p.FilePath, Pattern: p.Pattern, ToolResult: p.ToolResult,
	}, nil
}

func (Adapter) Encode(_ agents.EventKind, out io.Writer, res agents.Result) error {
	return json.NewEncoder(out).Encode(map[string]any{
		"additionalContext": res.Context,
		"systemMessage":     res.SystemMessage,
	})
}

func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindSessionStart, in, out)
}
func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostRead, in, out)
}
func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostEdit, in, out)
}
func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindAugment, in, out)
}
func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindStop, in, out)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/opencode/ -run 'TestDecode|TestEncode'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/opencode/adapter.go internal/agents/opencode/adapter_test.go
git commit -m "feat(opencode): neutral hook adapter"
```

---

## Task 10: OpenCode installer (opencode.json + plugin + AGENTS.md)

**Files:**
- Create: `internal/agents/opencode/install.go`, `internal/agents/opencode/plugin.go`, `internal/agents/opencode/install_test.go`

- [ ] **Step 1: Write the failing test**

```go
package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestInstallRepoScope(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	res, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q", res.MCP)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "opencode.json"))
	cs := string(cfg)
	if !strings.Contains(cs, `"mcp"`) || !strings.Contains(cs, `"recarto"`) || !strings.Contains(cs, `"local"`) {
		t.Errorf("opencode.json: %s", cs)
	}
	plug, _ := os.ReadFile(filepath.Join(dir, ".opencode", "plugins", "recarto.js"))
	ps := string(plug)
	for _, want := range []string{"experimental.chat.system.transform", "file.edited", "/bin/recarto", "hook session-start --agent opencode"} {
		if !strings.Contains(ps, want) {
			t.Errorf("plugin missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing: %v", err)
	}
	res2, _ := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if res2.MCP != "already present" {
		t.Errorf("not idempotent: %+v", res2)
	}
}

func TestInstallUserScopeTargets(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeUser, Dir: dir, Home: home}, "/bin/recarto"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range [][]string{
		{".config", "opencode", "opencode.json"},
		{".config", "opencode", "plugins", "recarto.js"},
		{".config", "opencode", "AGENTS.md"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Errorf("user target %v missing: %v", rel, err)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/opencode/ -run TestInstall`
Expected: FAIL.

- [ ] **Step 3: Implement `plugin.go` + `install.go`**

`plugin.go`:
```go
package opencode

import "strings"

// pluginJS returns the generated OpenCode plugin source bound to the recarto
// binary. It bridges OpenCode lifecycle hooks to `recarto hook … --agent opencode`.
func pluginJS(binary string) string {
	const tmpl = `// recarto OpenCode plugin (generated by ` + "`recarto init`" + `). Do not edit by hand.
const BIN = ${BIN};
export const recarto = async ({ $, directory }) => {
  const run = async (event, payload) => {
    try {
      const r = await $` + "`${BIN} hook ${event} --agent opencode`" + `
        .cwd(directory).stdin(JSON.stringify(payload)).quiet().nothrow();
      return JSON.parse(r.stdout.toString() || "{}");
    } catch { return {}; }
  };
  let injected = false;
  return {
    "experimental.chat.system.transform": async (_input, output) => {
      if (injected) return; injected = true;
      const r = await run("session-start", {});
      if (r.additionalContext) output.system.push(r.additionalContext);
    },
    "file.edited": async (input) => { await run("post-edit", { file_path: input.file }); },
    "tool.execute.after": async (input, output) => {
      const name = (input && input.tool) || "";
      if (/grep|glob|search|find/i.test(name)) {
        const r = await run("augment", { pattern: (output && output.args && (output.args.pattern || output.args.query)) || "" });
        if (r.additionalContext && output) output.output = (output.output || "") + "\n\n" + r.additionalContext;
      } else if (/read|view|cat/i.test(name)) {
        const r = await run("post-read", { file_path: (output && output.args && output.args.filePath) || "", tool_result: (output && output.output) || "" });
        if (r.additionalContext && output) output.output = (output.output || "") + "\n\n" + r.additionalContext;
      }
    },
  };
};
`
	return strings.ReplaceAll(tmpl, "${BIN}", jsString(binary))
}

// jsString returns a JSON-quoted JS string literal (safe for paths with backslashes).
func jsString(s string) string {
	q := strings.ReplaceAll(s, `\`, `\\`)
	q = strings.ReplaceAll(q, `"`, `\"`)
	return `"` + q + `"`
}
```

`install.go`:
```go
package opencode

import (
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func ensureMCPServer(doc map[string]any, binary string) bool {
	mcp, ok := doc["mcp"].(map[string]any)
	if !ok {
		mcp = map[string]any{}
		doc["mcp"] = mcp
	}
	if _, exists := mcp["recarto"]; exists {
		return false
	}
	mcp["recarto"] = map[string]any{
		"type": "local", "command": []any{binary, "serve"}, "enabled": true,
	}
	return true
}

func Install(p agents.Paths, binary string) (agents.InstallResult, error) {
	res := agents.InstallResult{Agent: "opencode"}

	var cfgPath, plugPath, docPath string
	switch p.Scope {
	case agents.ScopeUser:
		cfgPath = p.UserFile(".config", "opencode", "opencode.json")
		plugPath = p.UserFile(".config", "opencode", "plugins", "recarto.js")
		docPath = p.UserFile(".config", "opencode", "AGENTS.md")
	default:
		cfgPath = p.RepoFile("opencode.json")
		plugPath = p.RepoFile(".opencode", "plugins", "recarto.js")
		docPath = p.RepoFile("AGENTS.md")
	}

	doc, err := agents.ReadJSONObject(cfgPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(doc, binary) {
		res.MCP = "configured"
	} else {
		res.MCP = "already present"
	}
	if err := agents.WriteJSONObject(cfgPath, doc); err != nil {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(plugPath), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(plugPath, []byte(pluginJS(binary)), 0o644); err != nil {
		return res, err
	}
	res.Hooks = "plugin written"

	if err := agents.WriteInstructionsBlock(docPath); err != nil {
		return res, err
	}
	res.Docs = "written"
	return res, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/opencode/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/opencode/
git commit -m "feat(opencode): scope-aware installer + generated plugin"
```

---

## Task 11: CLI `hook --agent` dispatch

**Files:**
- Modify: `internal/cli/hook.go`
- Test: `internal/cli/hook_test.go` (extend)

- [ ] **Step 1: Write the failing test**

```go
func TestHookAgentSelectsCopilotPayload(t *testing.T) {
	a := newTestApp(t)
	// Copilot camelCase payload; bad CC parsing would yield empty file path → no-op.
	in := strings.NewReader(`{"sessionId":"s1","source":"new"}`)
	var out strings.Builder
	if err := Hook(context.Background(), a, in, &out, []string{"session-start", "--agent", "copilot"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("copilot session-start should emit additionalContext:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestHookAgent`
Expected: FAIL.

- [ ] **Step 3: Implement**

Rewrite `Hook` to parse `--agent` (default `claude`), map event→`agents.EventKind`, select the adapter, and call `agents.RunHook`:

```go
func Hook(ctx context.Context, a *app.App, in io.Reader, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: event required (session-start|post-read|post-edit|augment|stop)")
	}
	event := args[0]
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent host: claude|opencode|copilot")
	if helped, err := parseFlags(fs, args[1:]); helped || err != nil {
		return err
	}
	kind, err := agents.KindFromString(event)
	if err != nil {
		return err
	}
	var ad agents.Adapter
	switch *agent {
	case "claude", "claude-code":
		ad = claudecode.Adapter{}
	case "opencode":
		ad = opencode.Adapter{}
	case "copilot":
		ad = copilot.Adapter{}
	default:
		return fmt.Errorf("hook: unknown --agent %q (claude|opencode|copilot)", *agent)
	}
	return agents.RunHook(ctx, a, ad, kind, in, out)
}
```

Add imports for `flag`, `agents`, `claudecode`, `opencode`, `copilot`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/cli/ -run TestHook`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/hook.go internal/cli/hook_test.go
git commit -m "feat(cli): hook --agent selects host adapter"
```

---

## Task 12: CLI `init --agent --scope` + auto-detect + prompt

**Files:**
- Modify: `internal/cli/init.go`, `internal/cli/cli.go` (usage text; `Init` signature/call)
- Test: `internal/cli/init_test.go` (extend)

- [ ] **Step 1: Write the failing test**

```go
func TestInitAllAgentsRepoScope(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := Init(&out, dir, "/bin/recarto", []string{"--agent", "claude,opencode,copilot", "--scope", "repo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".mcp.json", "opencode.json", filepath.Join(".github", "hooks", "recarto.json"), "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if !strings.Contains(out.String(), "claude") || !strings.Contains(out.String(), "opencode") || !strings.Contains(out.String(), "copilot") {
		t.Errorf("output should summarize all agents:\n%s", out.String())
	}
}

func TestInitUnknownAgentErrors(t *testing.T) {
	if err := Init(&strings.Builder{}, t.TempDir(), "/bin/recarto", []string{"--agent", "vim", "--scope", "repo"}); err == nil {
		t.Error("expected error for unknown agent")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestInit`
Expected: FAIL (signature mismatch).

- [ ] **Step 3: Implement**

Rewrite `Init` to the new signature and logic:

```go
func Init(out io.Writer, dir, binary string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agentList := fs.String("agent", "", "comma list: claude,opencode,copilot (default: auto-detect)")
	scopeStr := fs.String("scope", "", "user|repo (default: repo; prompts when a TTY)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	selected := splitCSV(*agentList)
	if len(selected) == 0 {
		selected = agents.Detect()
		if len(selected) == 0 {
			selected = []string{"claude"} // sensible fallback
		}
	}

	scope := agents.ScopeRepo
	switch *scopeStr {
	case "user":
		scope = agents.ScopeUser
	case "repo", "":
		scope = agents.ScopeRepo
	default:
		return fmt.Errorf("init: invalid --scope %q (user|repo)", *scopeStr)
	}

	home, err := agents.HomeDir()
	if err != nil {
		return err
	}
	p := agents.Paths{Scope: scope, Dir: dir, Home: home}

	fmt.Fprintf(out, "recarto init (scope=%s, agents=%s)\n", scope, strings.Join(selected, ","))
	for _, name := range selected {
		var res agents.InstallResult
		switch name {
		case "claude", "claude-code":
			res, err = claudecode.Install(p, binary)
		case "opencode":
			res, err = opencode.Install(p, binary)
		case "copilot":
			res, err = copilot.Install(p, binary)
		default:
			return fmt.Errorf("init: unknown agent %q (claude|opencode|copilot)", name)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %-9s mcp:%s hooks:%s docs:%s\n", res.Agent, res.MCP, res.Hooks, res.Docs)
	}
	return nil
}
```

Update `cli.go`'s `init` branch to call `Init(out, dir, binaryPath(), rest)` and refresh the usage line:
`"  init [--agent ..] [--scope user|repo]  configure agents (Claude/OpenCode/Copilot)"`.

> **Scope prompt:** interactive prompting (TTY → ask user/repo) lives at the `cmd/recarto` edge to keep `Init` pure/testable. Add to `cmd/recarto/main.go`: when the subcommand is `init`, no `--scope` is present, and `isatty(stdin)`, read a line ("Install scope — [r]epo or [u]ser? ") and append `--scope repo|user` to the args before calling `cli.Run`. Keep it minimal; default to repo on empty/non-TTY. (Covered by manual verification, not a unit test.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/cli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/init.go internal/cli/cli.go internal/cli/init_test.go
git commit -m "feat(cli): init --agent/--scope with auto-detect"
```

---

## Task 13: Scope prompt at the binary edge

**Files:**
- Modify: `cmd/recarto/main.go`

- [ ] **Step 1: Implement** (no unit test — TTY edge; verified manually)

In `main()`, before the default `cli.Run(os.Args[1:], ...)` branch, special-case `init`:

```go
case "init":
	args := maybePromptScope(os.Args[2:])
	if err := cli.Run(append([]string{"init"}, args...), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "recarto:", err)
		os.Exit(1)
	}
```

Add:
```go
import "github.com/mattn/go-isatty" // already an indirect dep (go.sum); promote to direct via go mod tidy

func maybePromptScope(args []string) []string {
	for _, a := range args {
		if a == "--scope" || strings.HasPrefix(a, "--scope=") {
			return args
		}
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return args
	}
	fmt.Fprint(os.Stderr, "Install scope — [r]epo (this project) or [u]ser (global)? [r/u]: ")
	var line string
	fmt.Fscanln(os.Stdin, &line)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "u") {
		return append(args, "--scope", "user")
	}
	return append(args, "--scope", "repo")
}
```

- [ ] **Step 2: Build & manual check**

Run: `go build ./cmd/recarto && printf '' | ./recarto init --agent claude --scope repo` (non-interactive path).
Expected: prints the init summary; no prompt when `--scope` present.

- [ ] **Step 3: Commit**

```bash
go mod tidy
git add cmd/recarto/main.go go.mod go.sum
git commit -m "feat(cli): prompt repo/user scope on interactive init"
```

---

## Task 14: Version var + Makefile

**Files:**
- Modify: `internal/version/version.go`
- Create: `Makefile`

- [ ] **Step 1: Change `const`→`var`**

```go
// Version is the current Recarto version (overridable via -ldflags -X).
var Version = "0.1.0-dev"
```

Run: `go test ./internal/version/...` → PASS (existing `TestVersionNotEmpty`).

- [ ] **Step 2: Create `Makefile`**

```make
BINARY_NAME := recarto
PKG         := ./cmd/recarto
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -ldflags "-s -w -X git.gerwim.nl/gerwim/recarto/internal/version.Version=$(VERSION)"

.PHONY: all linux darwin windows test vet fmt clean

all: linux darwin windows

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-linux-musl" \
		go build $(LDFLAGS) -o releases/linux-amd64/$(BINARY_NAME) $(PKG)

darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 CC="zig cc -target aarch64-macos" \
		go build $(LDFLAGS) -o releases/darwin-arm64/$(BINARY_NAME) $(PKG)

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="zig cc -target x86_64-windows-gnu" \
		go build $(LDFLAGS) -o releases/windows-amd64/$(BINARY_NAME).exe $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf releases
```

- [ ] **Step 3: Verify the build**

Run: `make all` (requires `zig` on PATH). Expected: three binaries under `releases/`.
If `zig` is absent, run `make linux` natively to confirm at least the host target builds; note zig requirement in README.

- [ ] **Step 4: Commit**

```bash
git add Makefile internal/version/version.go
git commit -m "build: cross-compile Makefile (zig cc) + version ldflags var"
```

---

## Task 15: README rewrite

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Replace** the GitLab boilerplate with sections: title + one-line description; "What it does" (cross-session memory + code graph as MCP tools/hooks for Claude Code, OpenCode, Copilot CLI); **Install** (download prebuilt for Windows x64 / Linux x64 / macOS ARM64, or build: `make all` — needs Go 1.26.4 + `zig`; native `go build ./cmd/recarto`); **Setup** (`recarto init` → auto-detects installed agents, prompts repo/user scope; what each agent gets: MCP server, lifecycle hooks, AGENTS.md; flags `--agent`, `--scope`); **Usage** (MCP tools list + key CLI commands from `cli.go` usage); **How it works** (per-repo SQLite cache, branch-tagged memories, zig cross-compile note); **Development** (`make test`, CI matrix).

- [ ] **Step 2: Verify** links/commands match the actual CLI (`recarto help` output) and Makefile targets.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: rewrite README with per-platform install + multi-agent setup"
```

---

## Task 16: Full suite + gofmt + manual integration

- [ ] **Step 1:** `gofmt -l .` → empty. `go vet ./...` → clean. `go test ./... -race` → all PASS.
- [ ] **Step 2:** Build: `go build ./cmd/recarto -o /tmp/recarto`.
- [ ] **Step 3:** Manual against installed tools (temp dir repo):
  - `/tmp/recarto init --agent opencode,copilot --scope repo` → inspect generated files.
  - `cd` into it; `copilot mcp list` shows `recarto`; `opencode mcp list` shows `recarto`.
  - `echo '{"sessionId":"x"}' | /tmp/recarto hook session-start --agent copilot` → valid JSON with `additionalContext`.
  - `echo '{"session_id":"x"}' | /tmp/recarto hook session-start --agent opencode` → valid neutral JSON.
- [ ] **Step 4:** Update `.wolf/anatomy.md` (new files), `.wolf/memory.md` (session line), `.wolf/cerebrum.md` (Key Learnings: multi-agent host facts), `.wolf/buglog.json` (any bug found).
- [ ] **Step 5:** Final commit:

```bash
git add -A
git commit -m "test: full suite green; openwolf bookkeeping for multi-agent slice"
```

---

## Self-Review

**Spec coverage:** §3.1 core → T1–T2; §3.2 adapters → T4 (claude), T7 (copilot), T9 (opencode); §3.3 CLI → T11–T13; §3.4 installers → T6/T8/T10 + shared T3/T5; §4 Makefile → T14; §5 README → T15; §6 testing → every task + T16. Scope user/repo → T5/T6/T8/T10/T12/T13. Auto-detect → T5/T12. Instructions unify (AGENTS.md repo, native user files) → T5/T6/T8/T10. ✓

**Placeholder scan:** none — all code blocks are concrete. Move tasks (T2) name exact source funcs to relocate verbatim. ✓

**Type consistency:** `agents.Event`/`agents.Result`/`agents.Adapter`/`agents.RunHook`/`agents.EventKind`/`agents.Paths`/`agents.InstallResult`/`agents.ScopeRepo|ScopeUser`/`agents.ReadJSONObject`/`agents.WriteJSONObject`/`agents.WriteInstructionsBlock`/`agents.Detect`/`agents.HomeDir` used consistently across tasks. Each adapter exposes `Adapter{}` + `SessionStart/PostRead/PostEdit/Augment/Stop`. `Install(agents.Paths, binary) (agents.InstallResult, error)` uniform across the three installers. `Init(out, dir, binary, args)` matches its caller in `cli.go`. ✓

**Risks tracked:** Copilot tool names (substring routing, verify in T16); OpenCode `experimental.chat.system.transform` (degrade to AGENTS.md, verify T16); shared `.mcp.json` (verify both readers T16).
