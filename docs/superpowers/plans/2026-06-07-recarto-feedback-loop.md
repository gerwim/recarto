# Recarto Self-Improvement Feedback Loop — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an LLM report Recarto's own faults (errors, bad ranking, friction, capability gaps) from any repo into one global queue the maintainer triages with `recarto feedback` from anywhere.

**Architecture:** A reports is a `memory.Record` of a new internal kind `feedback`, stored in a dedicated global store keyed by the reserved repo-id `_recarto_feedback` (path `<userCacheDir>/recarto/_recarto_feedback/store.db`, independent of cwd/git). It reuses the existing `memory.Engine` so near-duplicate reports reinforce instead of duplicating; source repos accumulate as `tag` refs (unioned on reinforce) so one entry shows every repo it was seen in. Agents write via a new `report_recarto_issue` MCP tool plus a reflection clause on the existing Stop hook; the maintainer reads via a location-independent `recarto feedback` CLI.

**Tech Stack:** Go, modernc SQLite (pure-Go), MCP Go SDK (confined to `internal/mcp/server.go`), FTS5.

---

## Spec deviations (flagged for reviewer)

Two pragmatic departures from `docs/superpowers/specs/2026-06-07-recarto-feedback-loop-design.md`, discovered while reading the code:

1. **Stop-hook gating dropped.** The spec wanted the reflection nudge gated to "only when a Recarto MCP tool was used this session." The MCP server (`recarto serve`) holds no per-session id — handlers are `(ctx, *app.App, In)` with no session/agent — so per-session MCP-usage tracking isn't cleanly available. Since `handleStop` *already* emits an unconditional `SystemMessage` every session, the reflection clause is appended to that existing message (Task 6). Net effect: one extra sentence on a nudge that already fires; no new gating machinery.

2. **Agent host dropped.** The spec listed "agent host (claude/opencode/copilot)" among the auto-captured fields, but the host identity isn't available at MCP-tool time (only at hook time, via `Event.Agent`). Rather than ship a dead field, it is omitted entirely. If wanted later, an installer task would inject the host into each MCP config's `env` block. The source repo and symptom are what matter for triage.

`--note` on `resolve` is kept (via `json_set`). If the reviewer prefers strict spec fidelity on gating, that's a follow-up.

---

## File Structure

**New files:**
- `internal/feedback/feedback.go` — reserved-id constant + `Open`/`Bundle`/`Close` for the global feedback store. One responsibility: hand callers an open feedback store + engine, repo-independently.
- `internal/feedback/feedback_test.go`
- `internal/memory/feedback.go` — `Engine.ResolveFeedback` (active→resolved for kind=feedback), mirroring `todo.go`'s `CompleteTodo`.
- `internal/memory/feedback_test.go`
- `internal/mcp/handlers_feedback.go` — `report_recarto_issue` input/output + handler.
- `internal/mcp/handlers_feedback_test.go`
- `internal/cli/feedback.go` — `recarto feedback list|show|resolve`.
- `internal/cli/feedback_test.go`

**Modified files:**
- `internal/memory/record.go` — add `KindFeedback`; make `Valid()` accept it WITHOUT adding it to `AllKinds` (keeps it out of the `record_memory` enum).
- `internal/store/migrate.go` — migration v17 widening the kind + status CHECKs.
- `internal/store/migrate_test.go` — bump the version assertion; add a v17 acceptance test.
- `internal/mcp/schema.go` — `reportRecartoIssueSchema()`.
- `internal/mcp/server.go` — register the tool.
- `internal/mcp/handlers.go` — guard `record_memory` against `kind=feedback`.
- `internal/cli/cli.go` — dispatch `feedback` BEFORE `app.Open` (location independence); add usage line.
- `internal/agents/handle.go` — extend the Stop `SystemMessage`.
- `internal/agents/handle_test.go` — assert the new clause.
- `internal/agents/instructions.go` — add a `report_recarto_issue` bullet.
- `internal/agents/instructions_test.go` — assert the bullet.

---

## Task 1: Add the `feedback` kind + store migration v17

**Files:**
- Modify: `internal/memory/record.go:9-34`
- Modify: `internal/store/migrate.go:11` and `:14-433` (append v17)
- Modify: `internal/store/migrate_test.go:331-350` (version assertion) and add a new test
- Test: `internal/store/migrate_test.go`

The kind/status CHECKs are enforced in SQLite. v12 is the precedent: it widened both via `writable_schema` + `replace()` with idempotency guards. We mirror it exactly to add `kind='feedback'` and `status='resolved'`.

- [ ] **Step 1: Write the failing migration test**

Replace `TestSchemaVersionIs16` (at `internal/store/migrate_test.go:331`) with this, and add the acceptance test after it:

```go
func TestSchemaVersionIs17(t *testing.T) {
	if schemaVersion != 17 {
		t.Fatalf("schemaVersion = %d, want 17", schemaVersion)
	}
	if len(migrations) != 17 {
		t.Fatalf("len(migrations) = %d, want 17", len(migrations))
	}
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 17 {
		t.Fatalf("user_version = %d, want 17", v)
	}
}

// TestMigrateV17AllowsFeedbackAndResolved mirrors TestMigrateV12AllowsTodoAndDone:
// the widened CHECKs admit the new kind/status while still rejecting bogus values.
func TestMigrateV17AllowsFeedbackAndResolved(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mustExec(t, st, `INSERT INTO memories(id,kind,title,body,created_at,updated_at)
		VALUES('f1','feedback','search_content rejected query','body',1,1)`)
	mustExec(t, st, `UPDATE memories SET status='resolved' WHERE id='f1'`)

	if _, err := st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid kind")
	}
	if _, err := st.DB.Exec(`UPDATE memories SET status='bogus' WHERE id='f1'`); err == nil {
		t.Fatal("expected CHECK to still reject an invalid status")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestSchemaVersionIs17|TestMigrateV17' -v`
Expected: FAIL — `schemaVersion = 16, want 17` (and the feedback INSERT would be rejected by the un-widened CHECK).

- [ ] **Step 3: Bump the version and append migration v17**

In `internal/store/migrate.go`, change line 11:

```go
const schemaVersion = 17
```

Then append this entry to the `migrations` slice, immediately after the v16 string (after `internal/store/migrate.go:432`, before the closing `}`):

```go
	// v17 (feedback kind): widen the memories CHECK constraints to admit
	// kind='feedback' (recorded only via report_recarto_issue, into the dedicated
	// global feedback store) and status='resolved' (the feedback triage terminal).
	// Same mechanism and rationale as v12's todo widening: SQLite cannot ALTER a
	// CHECK and a table rebuild is unsafe inside the migration transaction, so edit
	// the stored schema text via writable_schema, swapping ONLY the two CHECK
	// value-lists with replace() to preserve ALTER-appended columns, then RESET so
	// pooled connections reparse. The `sql NOT LIKE` guards keep it idempotent. This
	// is NOT a graph migration; the derived graph is left untouched.
	`
PRAGMA writable_schema=ON;
UPDATE sqlite_master
   SET sql = replace(sql,
     '''preference'',''learning'',''decision'',''do_not_repeat'',''bug'',''action'',''todo''',
     '''preference'',''learning'',''decision'',''do_not_repeat'',''bug'',''action'',''todo'',''feedback''')
 WHERE type='table' AND name='memories' AND sql NOT LIKE '%''feedback''%';
UPDATE sqlite_master
   SET sql = replace(sql,
     '''active'',''superseded'',''archived'',''done''',
     '''active'',''superseded'',''archived'',''done'',''resolved''')
 WHERE type='table' AND name='memories' AND sql NOT LIKE '%''resolved''%';
PRAGMA writable_schema=RESET;
`,
```

- [ ] **Step 4: Add the `KindFeedback` kind (valid, but not in the record_memory enum)**

In `internal/memory/record.go`, add the constant in the `const` block (after `KindTodo` at line 15):

```go
	KindTodo        Kind = "todo"
	// KindFeedback is an INTERNAL kind: reports about Recarto itself, recorded only
	// via report_recarto_issue into the dedicated feedback store. It is deliberately
	// absent from AllKinds so it never appears in the record_memory enum, but Valid()
	// accepts it so Engine.Record will store it.
	KindFeedback Kind = "feedback"
```

Then change `validKinds` (at `internal/memory/record.go:25-31`) to add it explicitly, leaving `AllKinds` unchanged:

```go
var validKinds = func() map[Kind]bool {
	m := make(map[Kind]bool, len(AllKinds)+1)
	for _, k := range AllKinds {
		m[k] = true
	}
	m[KindFeedback] = true // internal kind, not user-recordable via record_memory
	return m
}()
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ ./internal/memory/ -run 'TestSchemaVersionIs17|TestMigrateV17|TestMigrate' -v`
Expected: PASS. Also run the full store suite to confirm no migration invariant regressed (the v10-seeded graph-wipe tests still pass because v11–v16 still wipe):
Run: `go test ./internal/store/`
Expected: ok.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go internal/memory/record.go
git commit -m "feat(store): add feedback memory kind + status via migration v17"
```

---

## Task 2: The global feedback store opener (`internal/feedback`)

**Files:**
- Create: `internal/feedback/feedback.go`
- Test: `internal/feedback/feedback_test.go`

Repo-independent open of the reserved store. This is what makes the CLI and MCP tool agree on one global queue.

- [ ] **Step 1: Write the failing test**

Create `internal/feedback/feedback_test.go`:

```go
package feedback

import (
	"context"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestOpenIsRepoIndependentAndPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", dir)
	ctx := context.Background()

	b, err := Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	// The store lives at the reserved id under the cache dir, regardless of cwd.
	want := filepath.Join(dir, StoreID, "store.db")
	if b.Store.Path() != want {
		t.Fatalf("store path = %q, want %q", b.Store.Path(), want)
	}
	if _, _, err := b.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindFeedback, Title: "t", Body: "b", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	b.Close()

	// Reopen: the report persists in the same global store.
	b2, err := Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	recs, err := b2.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("feedback records = %d, want 1", len(recs))
	}
}
```

> NOTE: this test uses `b.Store.Path()`, which does not exist yet. `store.Store` has an unexported `path` field (confirmed at `internal/store/store.go:33`, set at `:62`) but no accessor. Add one in this task (include `internal/store/store.go` in the Step 5 commit):
> ```go
> // Path returns the database file path this store was opened from.
> func (s *Store) Path() string { return s.path }
> ```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/feedback/ -run TestOpenIsRepoIndependentAndPersists -v`
Expected: FAIL — package `feedback` does not exist / undefined `Open`.

- [ ] **Step 3: Write the implementation**

Create `internal/feedback/feedback.go`:

```go
// Package feedback opens Recarto's global, cross-repo "feedback about Recarto
// itself" store. It is keyed by a reserved repo-id and is independent of the
// current git repo or working directory, so reports written from any repo collect
// in one queue and `recarto feedback` reads them from anywhere.
package feedback

import (
	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// StoreID is the reserved repo-id for the global feedback store. Real repo-ids are
// git-hash derived, so a leading-underscore sentinel cannot collide with one.
const StoreID = "_recarto_feedback"

// Bundle is an open feedback store and its memory engine. Close it when done.
type Bundle struct {
	Store  *store.Store
	Engine *memory.Engine
}

// Open opens — creating and migrating on first use — the global feedback store.
// It ignores the working directory and git: the path is fixed under the cache dir.
func Open(clk clock.Clock) (*Bundle, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	path, err := cache.StorePath(StoreID)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	led := ledger.New(st)
	eng := memory.New(st, led, clk)
	return &Bundle{Store: st, Engine: eng}, nil
}

// Close releases the underlying store.
func (b *Bundle) Close() error { return b.Store.Close() }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/feedback/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/feedback/ internal/store/store.go
git commit -m "feat(feedback): repo-independent global feedback store opener"
```

---

## Task 3: `Engine.ResolveFeedback` (active→resolved)

**Files:**
- Create: `internal/memory/feedback.go`
- Test: `internal/memory/feedback_test.go`

Mirror of `CompleteTodo`, for the feedback kind, with an optional resolution note written via `json_set`.

- [ ] **Step 1: Write the failing test**

Create `internal/memory/feedback_test.go`:

```go
package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, ledger.New(st), clock.Real{})
}

func TestResolveFeedback(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	id, _, err := e.Record(ctx, NewMemory{Kind: KindFeedback, Title: "t", Body: "b", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.ResolveFeedback(ctx, id, "fixed in v2")
	if err != nil || !ok {
		t.Fatalf("ResolveFeedback = (%v, %v), want (true, nil)", ok, err)
	}
	rec, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("status = %q, want resolved", rec.Status)
	}
	if note, _ := rec.Extra["resolution_note"].(string); note != "fixed in v2" {
		t.Fatalf("resolution_note = %q, want %q", note, "fixed in v2")
	}

	// Idempotent: a second resolve finds no active feedback with that id.
	ok, err = e.ResolveFeedback(ctx, id, "")
	if err != nil || ok {
		t.Fatalf("second ResolveFeedback = (%v, %v), want (false, nil)", ok, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/memory/ -run TestResolveFeedback -v`
Expected: FAIL — `e.ResolveFeedback undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/memory/feedback.go`:

```go
package memory

import (
	"context"
	"database/sql"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// ResolveFeedback marks an active feedback report resolved (active→resolved),
// optionally recording a resolution note in Extra. It mirrors CompleteTodo for the
// feedback kind: safe to call repeatedly, returning (false, nil) when no active
// feedback with that id exists. The row is dropped from the lexical index so a
// resolved report stops surfacing as a recall candidate.
func (e *Engine) ResolveFeedback(ctx context.Context, id, note string) (bool, error) {
	var done bool
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		now := e.clk.Now().UnixMilli()
		var res sql.Result
		var err error
		if note != "" {
			res, err = c.ExecContext(ctx,
				`UPDATE memories
				    SET status='resolved', updated_at=?,
				        extra=json_set(extra, '$.resolution_note', ?)
				  WHERE id=? AND kind='feedback' AND status='active'`,
				now, note, id)
		} else {
			res, err = c.ExecContext(ctx,
				`UPDATE memories SET status='resolved', updated_at=?
				  WHERE id=? AND kind='feedback' AND status='active'`,
				now, id)
		}
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

> (`store.Conn.ExecContext` returns `(sql.Result, error)` — confirmed at `internal/store/store.go:20` — so `var res sql.Result` is correct. This mirrors `todo.go`'s `CompleteTodo`, which uses the same pattern with `:=`.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/memory/ -run TestResolveFeedback -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/feedback.go internal/memory/feedback_test.go
git commit -m "feat(memory): ResolveFeedback transitions feedback active->resolved"
```

---

## Task 4: The `report_recarto_issue` MCP tool

**Files:**
- Create: `internal/mcp/handlers_feedback.go`
- Modify: `internal/mcp/schema.go:48` (append `reportRecartoIssueSchema`)
- Modify: `internal/mcp/server.go:69` (register the tool, before `return s`)
- Modify: `internal/mcp/handlers.go:51-58` (guard record_memory against feedback)
- Test: `internal/mcp/handlers_feedback_test.go`

The handler writes to the feedback store (never the per-repo store), auto-captures source repo/version/host, and carries the source repo as a `tag` ref so reinforcement accumulates the repo set.

- [ ] **Step 1: Write the failing test**

The `internal/mcp` test package already has `newTestApp(t, branch)` (at `internal/mcp/handlers_test.go:40`) which sets `RECARTO_CACHE_DIR` to a temp dir and opens an `*app.App` with a `fakeGit` identity (toplevel `/repo`). Reuse it — the per-repo store and the feedback store both live under that one cache dir, keyed by different repo-ids.

Create `internal/mcp/handlers_feedback_test.go`:

```go
package mcp

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestReportRecartoIssueWritesToFeedbackStore(t *testing.T) {
	a := newTestApp(t, "main") // sets RECARTO_CACHE_DIR; resolves a.Git via fakeGit
	ctx := context.Background()

	out, err := reportRecartoIssue(ctx, a, ReportRecartoIssueInput{
		SignalType: "error",
		Title:      "search_content rejected query param",
		Detail:     "Called search_content with `query`; it errored with unexpected property.",
		Tool:       "search_content",
		Severity:   "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("expected an id")
	}

	// It must NOT land in the per-repo store.
	perRepo, err := a.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(perRepo) != 0 {
		t.Fatalf("feedback leaked into the per-repo store: %d rows", len(perRepo))
	}

	// It MUST land in the global feedback store.
	b, err := feedback.Open(a.Clock)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	recs, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("feedback store rows = %d, want 1", len(recs))
	}
	if st, _ := recs[0].Extra["signal_type"].(string); st != "error" {
		t.Fatalf("signal_type = %q, want error", st)
	}

	// A near-identical second report reinforces rather than duplicating.
	out2, err := reportRecartoIssue(ctx, a, ReportRecartoIssueInput{
		SignalType: "error",
		Title:      "search_content rejected query param",
		Detail:     "Called search_content with `query`; it errored with unexpected property.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Reinforced || out2.ID != out.ID {
		t.Fatalf("second report = (id=%s reinforced=%v), want reinforce of %s", out2.ID, out2.Reinforced, out.ID)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcp/ -run TestReportRecartoIssueWritesToFeedbackStore -v`
Expected: FAIL — `reportRecartoIssue` / `ReportRecartoIssueInput` undefined.

- [ ] **Step 3: Write the handler**

Create `internal/mcp/handlers_feedback.go`:

```go
package mcp

import (
	"context"
	"fmt"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// ReportRecartoIssueInput are the arguments to report_recarto_issue. signal_type,
// title and detail are required; the rest are optional. Context (source repo,
// version) is auto-captured by the handler — the agent does not supply it.
type ReportRecartoIssueInput struct {
	SignalType   string `json:"signal_type" jsonschema:"the kind of problem: error (a tool errored/crashed/returned a clearly-wrong empty result), quality (it worked but ranked/returned the wrong thing), friction (confusing or awkward API), or gap (a capability you needed was missing)"`
	Title        string `json:"title" jsonschema:"one-line summary of the problem"`
	Detail       string `json:"detail" jsonschema:"what you were doing, what you expected, and what actually happened. Describe the Recarto interaction and a minimal repro — do NOT paste proprietary source from this repo"`
	Tool         string `json:"tool,omitempty" jsonschema:"which Recarto tool or area was involved (e.g. search_content, trace_call_path)"`
	Severity     string `json:"severity,omitempty" jsonschema:"low, medium, or high"`
	Repro        string `json:"repro,omitempty" jsonschema:"the call/args that triggered it"`
	SuggestedFix string `json:"suggested_fix,omitempty" jsonschema:"your proposed fix, if any"`
}

// ReportRecartoIssueOutput is the result of report_recarto_issue.
type ReportRecartoIssueOutput struct {
	ID         string `json:"id" jsonschema:"id of the recorded or reinforced report"`
	Reinforced bool   `json:"reinforced" jsonschema:"true if a near-duplicate report (e.g. from another repo) was reinforced instead of duplicated"`
}

// reportRecartoIssue records a report about Recarto itself into the dedicated
// global feedback store (NOT the current repo's store). The source repo is carried
// as a tag ref so that a report seen in several repos reinforces one entry while
// accumulating the set of repos it was seen in.
func reportRecartoIssue(ctx context.Context, a *app.App, in ReportRecartoIssueInput) (ReportRecartoIssueOutput, error) {
	if in.SignalType == "" || in.Title == "" || in.Detail == "" {
		return ReportRecartoIssueOutput{}, fmt.Errorf("signal_type, title and detail are required")
	}
	b, err := feedback.Open(a.Clock)
	if err != nil {
		return ReportRecartoIssueOutput{}, err
	}
	defer b.Close()

	source := a.Git.RepoName
	if source == "" {
		source = a.Git.RepoID
	}
	extra := map[string]any{
		"signal_type":      in.SignalType,
		"severity":         in.Severity,
		"tool":             in.Tool,
		"repro":            in.Repro,
		"suggested_fix":    in.SuggestedFix,
		"source_repo_id":   a.Git.RepoID,
		"source_repo_name": a.Git.RepoName,
		"recarto_version":  version.Version,
	}
	// Source repo as a tag ref: reinforce unions refs, so one entry accumulates every
	// repo it was reported from (occurrence count = reinforcement_count + 1).
	var refs []memory.Ref
	if source != "" {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: "repo:" + source})
	}
	id, reinforced, err := b.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindFeedback,
		Title:  in.Title,
		Body:   in.Title + "\n\n" + in.Detail, // body drives dedup + lexical match
		Refs:   refs,
		Branch: "", // global — feedback is not branch-scoped
		Author: "mcp",
		Source: "mcp",
		Extra:  extra,
	})
	if err != nil {
		return ReportRecartoIssueOutput{}, err
	}
	return ReportRecartoIssueOutput{ID: id, Reinforced: reinforced}, nil
}
```

- [ ] **Step 4: Add the enum schema**

Append to `internal/mcp/schema.go` (after `queryGraphSchema` at line 48):

```go
func reportRecartoIssueSchema() *jsonschema.Schema {
	return withEnum(schemaFor[ReportRecartoIssueInput](), "signal_type",
		"error", "quality", "friction", "gap")
}
```

- [ ] **Step 5: Register the tool**

In `internal/mcp/server.go`, add before `return s` (line 70):

```go
	addToolSchema(s, a, "report_recarto_issue",
		"Report a problem with Recarto ITSELF (not the user's code) so the maintainer can fix it: a tool that errored, ranked/returned the wrong thing, was confusing to use, or lacked a capability you needed. Routes to a global cross-repo queue. Describe the Recarto interaction and a minimal repro — not proprietary source from this repo.",
		reportRecartoIssueSchema(), reportRecartoIssue)
```

- [ ] **Step 6: Guard record_memory against the internal feedback kind**

In `internal/mcp/handlers.go`, in `recordMemory`, after the existing kind validation (after line 55):

```go
	if !kind.Valid() {
		return RecordMemoryOutput{}, fmt.Errorf("invalid kind %q", in.Kind)
	}
	if kind == memory.KindFeedback {
		return RecordMemoryOutput{}, fmt.Errorf("kind %q is recorded via report_recarto_issue, not record_memory", in.Kind)
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/mcp/ -run 'TestReportRecartoIssue|TestRecordMemory' -v`
Expected: PASS. Then build the whole module to confirm registration compiles:
Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/handlers_feedback.go internal/mcp/handlers_feedback_test.go internal/mcp/schema.go internal/mcp/server.go internal/mcp/handlers.go
git commit -m "feat(mcp): report_recarto_issue tool routing to the global feedback store"
```

---

## Task 5: The `recarto feedback` CLI (location-independent)

**Files:**
- Create: `internal/cli/feedback.go`
- Modify: `internal/cli/cli.go:36-42` (dispatch before `app.Open`) and `:107-122` (usage)
- Test: `internal/cli/feedback_test.go`

The command opens the feedback store directly — it must NOT go through `app.Open` (which resolves git and would fail outside a repo). It lists/show/resolves, sorted by reinforcement then recency.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/feedback_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func seedFeedback(t *testing.T) string {
	t.Helper()
	b, err := feedback.Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id, _, err := b.Engine.Record(context.Background(), memory.NewMemory{
		Kind:  memory.KindFeedback,
		Title: "search_content rejected query param",
		Body:  "search_content rejected query param\n\ndetail",
		Refs:  []memory.Ref{{Type: memory.RefTag, Value: "repo:ordia"}},
		Extra: map[string]any{"signal_type": "error", "severity": "medium"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFeedbackListShowResolve(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	ctx := context.Background()
	id := seedFeedback(t)

	// list (default = open)
	var out bytes.Buffer
	if err := Feedback(ctx, &out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search_content rejected") ||
		!strings.Contains(out.String(), "error") ||
		!strings.Contains(out.String(), "ordia") {
		t.Fatalf("list output missing fields:\n%s", out.String())
	}

	// show
	out.Reset()
	if err := Feedback(ctx, &out, []string{"show", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "severity") {
		t.Fatalf("show output missing fields:\n%s", out.String())
	}

	// resolve
	out.Reset()
	if err := Feedback(ctx, &out, []string{"resolve", id, "--note", "fixed"}); err != nil {
		t.Fatal(err)
	}

	// list is now empty; list --all shows the resolved one.
	out.Reset()
	if err := Feedback(ctx, &out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "search_content rejected") {
		t.Fatalf("resolved report still in open list:\n%s", out.String())
	}
	out.Reset()
	if err := Feedback(ctx, &out, []string{"list", "--all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search_content rejected") {
		t.Fatalf("--all did not show the resolved report:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestFeedbackListShowResolve -v`
Expected: FAIL — `undefined: Feedback`.

- [ ] **Step 3: Write the CLI**

Create `internal/cli/feedback.go`:

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Feedback dispatches `recarto feedback [list|show|resolve]`. It opens the global
// feedback store directly (no git/app resolution) so it works from anywhere,
// including outside a git repo.
func Feedback(ctx context.Context, w io.Writer, args []string) error {
	b, err := feedback.Open(clock.Real{})
	if err != nil {
		return err
	}
	defer b.Close()

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return feedbackList(ctx, b, w, args[1:])
	case "show":
		return feedbackShow(ctx, b, w, args[1:])
	case "resolve":
		return feedbackResolve(ctx, b, w, args[1:])
	default:
		return fmt.Errorf("feedback: unknown subcommand %q (list|show|resolve)", sub)
	}
}

func feedbackList(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("feedback list", flag.ContinueOnError)
	fs.SetOutput(w)
	all := fs.Bool("all", false, "include resolved reports")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}

	recs, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		return err
	}
	if *all {
		resolved, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback, Status: "resolved"})
		if err != nil {
			return err
		}
		recs = append(recs, resolved...)
	}
	// Rank by reinforcement (occurrence) desc, then most-recent.
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].ReinforcementCount != recs[j].ReinforcementCount {
			return recs[i].ReinforcementCount > recs[j].ReinforcementCount
		}
		return recs[i].UpdatedAt > recs[j].UpdatedAt
	})
	if len(recs) == 0 {
		fmt.Fprintln(w, "(no feedback)")
		return nil
	}
	for _, r := range recs {
		sig, _ := r.Extra["signal_type"].(string)
		sev, _ := r.Extra["severity"].(string)
		line := fmt.Sprintf("• %s  [%s", r.ID, sig)
		if sev != "" {
			line += "/" + sev
		}
		line += fmt.Sprintf("]  ×%d  %s", r.ReinforcementCount+1, r.Title)
		if r.Status == "resolved" {
			line += "  (resolved)"
		}
		fmt.Fprintln(w, line)
		if repos := sourceRepos(r); repos != "" {
			fmt.Fprintf(w, "    repos: %s\n", repos)
		}
	}
	return nil
}

func feedbackShow(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("feedback show: id required")
	}
	r, err := b.Engine.GetByID(ctx, args[0])
	if err != nil {
		return err
	}
	if r == nil || r.Kind != memory.KindFeedback {
		fmt.Fprintln(w, "(no such feedback)")
		return nil
	}
	fmt.Fprintf(w, "id:        %s\n", r.ID)
	fmt.Fprintf(w, "status:    %s\n", r.Status)
	fmt.Fprintf(w, "occurrences: %d\n", r.ReinforcementCount+1)
	for _, k := range []string{"signal_type", "severity", "tool", "recarto_version", "source_repo_name", "repro", "suggested_fix", "resolution_note"} {
		if v, _ := r.Extra[k].(string); v != "" {
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
	if repos := sourceRepos(r); repos != "" {
		fmt.Fprintf(w, "repos:     %s\n", repos)
	}
	fmt.Fprintf(w, "\n%s\n", r.Body)
	return nil
}

func feedbackResolve(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("feedback resolve", flag.ContinueOnError)
	fs.SetOutput(w)
	note := fs.String("note", "", "resolution note")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("feedback resolve: id required")
	}
	ok, err := b.Engine.ResolveFeedback(ctx, fs.Arg(0), *note)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(w, "(no active feedback with that id)")
		return nil
	}
	fmt.Fprintf(w, "resolved %s\n", fs.Arg(0))
	return nil
}

// sourceRepos extracts the comma-joined repo names from the record's repo: tag refs.
func sourceRepos(r *memory.Record) string {
	var repos []string
	for _, ref := range r.Refs {
		if ref.Type == memory.RefTag && strings.HasPrefix(ref.Value, "repo:") {
			repos = append(repos, strings.TrimPrefix(ref.Value, "repo:"))
		}
	}
	return strings.Join(repos, ", ")
}
```

> NOTE: `parseFlags` and the `flag` usage mirror `internal/cli/bug.go`. Confirm `parseFlags(fs, args) (bool, error)` exists: `grep -n 'func parseFlags' internal/cli/helpers.go`. It is used throughout bug.go, so it does.

- [ ] **Step 4: Wire dispatch + usage in `cli.go`**

In `internal/cli/cli.go`, add a branch right after the `init` block (after line 42, before `if !known(cmd)`):

```go
	// feedback reads the global feedback store directly — no git/app resolution —
	// so it works from anywhere, including outside a git repo.
	if cmd == "feedback" {
		return Feedback(context.Background(), out, rest)
	}
```

And add a usage line in `usage` (after the `bug` line at line 115):

```go
	fmt.Fprintln(w, "  feedback          triage cross-repo Recarto bug reports (list|show|resolve) — works anywhere")
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run TestFeedbackListShowResolve -v`
Expected: PASS.

- [ ] **Step 6: Verify location independence manually**

Run from a non-repo directory to confirm no git is required:
```bash
go build -o /tmp/recarto ./cmd/recarto && cd /tmp && RECARTO_CACHE_DIR=/tmp/rc-fb /tmp/recarto feedback list
```
Expected: prints `(no feedback)` with exit 0 — no git error.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/feedback.go internal/cli/feedback_test.go internal/cli/cli.go
git commit -m "feat(cli): recarto feedback list/show/resolve, location-independent"
```

---

## Task 6: Stop-hook reflection clause

**Files:**
- Modify: `internal/agents/handle.go:349` (extend the Stop SystemMessage)
- Test: `internal/agents/handle_test.go`

See deviation #1: appended to the existing unconditional Stop nudge.

- [ ] **Step 1: Write/extend the failing test**

Find the existing Stop test:
Run: `grep -n 'handleStop\|KindStop\|SystemMessage' internal/agents/handle_test.go`

Add this test to `internal/agents/handle_test.go`. It uses the package's existing `newTestApp(t, branch)` helper (`internal/agents/helpers_test.go:37`); `strings` is already imported in `handle_test.go`. The two existing Stop tests (`TestHandleStopNudge`, `TestHandleStopNudgePromptsPreferences`) assert substrings the extended message keeps ("record any new learnings", the preferences clause), so they stay green — no changes needed there.

```go
func TestHandleStopNudgesFeedback(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "report_recarto_issue") {
		t.Fatalf("Stop message missing feedback nudge:\n%s", res.SystemMessage)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agents/ -run TestHandleStopNudgesFeedback -v`
Expected: FAIL — message does not contain `report_recarto_issue`.

- [ ] **Step 3: Extend the Stop message**

In `internal/agents/handle.go`, replace the return at line 349:

```go
	return Result{SystemMessage: "recarto: before ending, record any new learnings (record_memory), bug fixes (record_bug), and preferences — e.g. if the user corrected your approach or stated a style/workflow choice. And if Recarto itself errored, ranked the wrong result first, confused you, or lacked a capability you needed this session, call report_recarto_issue so it can be fixed."}, nil
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/ -run TestHandleStop -v`
Expected: PASS. If another existing test asserts the exact old Stop string, update that expected string to the new one.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/handle.go internal/agents/handle_test.go
git commit -m "feat(hooks): nudge report_recarto_issue in the session-end reflection"
```

---

## Task 7: Encourage the tool in the agent instructions

**Files:**
- Modify: `internal/agents/instructions.go:106-110` (add a bullet near the curate bullet)
- Test: `internal/agents/instructions_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/agents/instructions_test.go`:

```go
func TestInstructionsMentionReportTool(t *testing.T) {
	if !strings.Contains(Instructions, "report_recarto_issue") {
		t.Fatal("Instructions should encourage report_recarto_issue")
	}
}
```

> Ensure `strings` is imported in that test file.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agents/ -run TestInstructionsMentionReportTool -v`
Expected: FAIL.

- [ ] **Step 3: Add the bullet**

In `internal/agents/instructions.go`, insert before the final "Inspect & curate" bullet (before line 106). Mind the Go string concatenation style used in the file (backtick segments with `+ "`...`" +` for inline code):

```go
- **Report Recarto's own faults**: if a Recarto tool errors, ranks the wrong
  result first, confuses you, or lacks a capability you needed, call ` + "`report_recarto_issue`" + `
  (signal_type: error|quality|friction|gap). It routes to a global cross-repo queue
  the maintainer triages — describe the Recarto interaction and a minimal repro, not
  this repo's source.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agents/ -run 'TestInstructions' -v`
Expected: PASS. (If an existing test pins the full Instructions text or its length, update that fixture.)

- [ ] **Step 5: Commit**

```bash
git add internal/agents/instructions.go internal/agents/instructions_test.go
git commit -m "docs(agents): instruct agents to report Recarto faults via report_recarto_issue"
```

---

## Final verification

- [ ] **Step 1: Full build + test**

Run: `go build ./... && go test ./...`
Expected: all packages `ok`.

- [ ] **Step 2: End-to-end smoke test**

```bash
go build -o /tmp/recarto ./cmd/recarto
export RECARTO_CACHE_DIR=/tmp/rc-e2e
# Simulate a report (the MCP path is covered by tests; here we exercise the store+CLI):
cd /tmp && /tmp/recarto feedback list      # → (no feedback)
```
Expected: `(no feedback)` from outside any git repo, exit 0 — confirming location independence end to end.

- [ ] **Step 3: Propagate instructions (manual, user action — note only)**

Re-running `recarto init` in each repo (or at user scope) rewrites the managed instructions block to include the new `report_recarto_issue` bullet. This is a user action, not part of the automated plan.
