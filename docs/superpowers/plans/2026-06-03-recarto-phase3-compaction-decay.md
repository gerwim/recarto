# Recarto Phase 3 — Compaction & Decay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a hook-driven + CLI compaction pass that keeps the per-repo memory store high-signal and bounded — superseded-GC, budget-triggered decay+eviction (archive→GC), and a bounded global dedup sweep — with decay on a logical activity clock so sporadic repo usage never rots memory.

**Architecture:** One new file `internal/memory/compact.go` adds `Engine.Compact(ctx, CompactOptions) (CompactReport, error)` plus the activity-epoch machinery, reusing the existing store/ledger/lexical/fingerprint internals. Schema bumps to **v4** (three new `memories` columns). Recall stamps `last_used_epoch` on surfaced rows. A `recarto compact` CLI command and a `SessionStart` hook trigger expose it. Eviction is budget-triggered (never age-triggered) so quiet repos never lose memory; a single-flight lease in `meta` serializes concurrent worktrees.

**Tech Stack:** Go 1.26.4, `modernc.org/sqlite` (pure-Go), stdlib `flag`, existing `internal/{store,memory,ledger,app,cli,agents/claudecode}` packages.

**Spec:** `docs/superpowers/specs/2026-06-03-recarto-phase3-compaction-decay-design.md`

**Conventions for the executor:**
- TDD strictly: write the failing test, run it red, implement minimally, run it green, commit.
- Run Go with the module toolchain: `go test ./...` (in-module `go version` = go1.26.4). Ignore IDE/gopls "undefined" false positives (system Go 1.24.4 mismatch).
- All commits end with the trailer: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Work on branch `phase-3-compaction-decay` (already created; the spec is already committed there).

---

## File Map

| File | Change | Responsibility |
|---|---|---|
| `internal/store/migrate.go` | Modify | Add v4 migration (3 columns + index); bump `schemaVersion` to 4. |
| `internal/store/migrate_test.go` | Modify | Add a test asserting the new columns exist. |
| `internal/memory/compact.go` | **Create** | Activity epoch, decay scoring, the four pass steps, `Engine.Compact`, `Engine.Epoch`, `Engine.LiveTokens`. |
| `internal/memory/compact_test.go` | **Create** | Unit + concurrency tests for everything in `compact.go`. |
| `internal/memory/compact_bench_test.go` | **Create** | `BenchmarkCompact` over a 50k synthetic store (resolves the sync-vs-detached question). |
| `internal/memory/recall.go` | Modify | Stamp `last_used_epoch`/`recall_count` on surfaced rows. |
| `internal/memory/recall_test.go` | Modify | Assert recall stamps the epoch. |
| `internal/cli/compact.go` | **Create** | `recarto compact [--force] [--dry-run] [--budget-tokens N]`. |
| `internal/cli/compact_test.go` | **Create** | CLI command test. |
| `internal/cli/cli.go` | Modify | Register `compact` in `known()`, the dispatch switch, and `usage`. |
| `internal/cli/status.go` | Modify | Show epoch, archived count, live-tokens-vs-budget, last run, compaction counters. |
| `internal/cli/status_test.go` | Modify | Assert the new status lines. |
| `internal/agents/claudecode/hooks.go` | Modify | `SessionStart` calls `Engine.Compact` best-effort before recall. |
| `internal/agents/claudecode/hooks_test.go` | Modify | Assert the hook advances the epoch. |
| `internal/graph/` → `internal/memory/multiproc_compact_test.go` | **Create** | Multi-process `recarto compact` concurrency proof. |

## Deliberate deviations from the spec

Two intentional refinements to the spec, both low-risk and following established codebase convention:

1. **Gating lives in `Engine.Compact`, not in `app.MaybeCompact`/`CompactNow`.** Spec §6 sketched thin `app` wrappers, but every existing surface calls engine methods directly (e.g. `SessionStart` calls `a.Graph.ReconcileIfStale` and CLI commands call `a.Engine.Record`). So the interval-gate + single-flight lease live inside `Engine.Compact(ctx, CompactOptions{Force})`; the hook and CLI call it directly. No `app` wrapper is added.
2. **Only `--budget-tokens` is exposed as a knob this slice.** The half-life, archive-grace, dedup-bucket, and min-interval values ship as compile-time constants in `compact.go` (spec §10 lists them as tunable). Exposing them via `RECARTO_*` env is a trivial follow-up if real usage demands it; budget is the only knob worth surfacing now.

---

### Task 1: Schema v4 migration (3 new `memories` columns)

**Files:**
- Modify: `internal/store/migrate.go`
- Test: `internal/store/migrate_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/migrate_test.go`:

```go
func TestMigrateAddsCompactionColumns(t *testing.T) {
	st := newTestStore(t) // existing helper in this package's tests
	defer st.Close()
	// The v4 columns must exist and be selectable.
	_, err := st.DB.Query(`SELECT last_used_epoch, recall_count, archived_epoch FROM memories LIMIT 0`)
	if err != nil {
		t.Fatalf("compaction columns missing: %v", err)
	}
}
```

> If `newTestStore` is not already defined in `internal/store/*_test.go`, use `store.Open(filepath.Join(t.TempDir(), "store.db"))` inline instead.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestMigrateAddsCompactionColumns -v`
Expected: FAIL — `no such column: last_used_epoch`.

- [ ] **Step 3: Add the v4 migration and bump the version**

In `internal/store/migrate.go`, change the version constant:

```go
// schemaVersion is the number of applied migrations.
const schemaVersion = 4
```

Append a fourth entry to the `migrations` slice (after the v3 string, before the closing `}`):

```go
	// v4 (Phase 3): compaction/decay signals. last_used_epoch is the activity
	// epoch at which a memory was last recalled (decay = current_epoch - it);
	// recall_count is cumulative recall hits; archived_epoch is when eviction
	// archived the row (drives archive-GC grace). Existing rows backfill to 0 —
	// harmless because eviction is budget-triggered, never age-triggered.
	`
ALTER TABLE memories ADD COLUMN last_used_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memories ADD COLUMN recall_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memories ADD COLUMN archived_epoch  INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_memories_last_used_epoch ON memories(last_used_epoch);
`,
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS — `TestMigrateAddsCompactionColumns` plus the existing version-assertion tests (they compare `user_version` against `schemaVersion`, now both 4).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat(store): schema v4 — compaction/decay columns on memories"
```

---

### Task 2: Activity-epoch machinery + meta helpers

**Files:**
- Create: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/memory/compact_test.go`:

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestEpochDefaultsToOne(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	got, err := e.Epoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("fresh-store epoch = %d, want 1", got)
	}
}
```

(Later tasks append more tests to this same file; Task 6 adds the shared `insertActive`/`statusOf` helpers and grows the import block.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestEpochDefaultsToOne -v`
Expected: FAIL — `e.Epoch undefined`.

- [ ] **Step 3: Create `compact.go` with the epoch + meta machinery**

Create `internal/memory/compact.go`:

```go
package memory

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Compaction tuning. These are policy defaults (not empirically optimized) and
// are overridable per call (BudgetTokens) or, for the rest, by editing here.
const (
	// DefaultBudgetTokens is the live-set token budget. Deliberately generous:
	// normal/quiet repos stay well under it and never evict — eviction is a
	// safety valve against runaway growth, not routine behavior.
	DefaultBudgetTokens = 250_000

	halfLifeActionEpochs = 5.0  // disposable activity log: fastest decay
	halfLifeStdEpochs    = 30.0 // preference / learning
	archiveGraceEpochs   = 30   // archived -> hard-deleted after this many epochs

	defaultDedupBuckets = 1000 // dedup sweep work budget per pass (cursor resumes)

	minIntervalMs = int64(60 * 60 * 1000) // 1h: gate so epoch ~= active sessions
	leaseTTLMs    = int64(5 * 60 * 1000)  // 5m: single-flight lease lifetime
)

// meta keys owned by compaction.
const (
	metaActivityEpoch = "compact_activity_epoch"
	metaLastRunAt     = "compact_last_run_at"
	metaDedupCursor   = "dedup_sweep_cursor" // "band,hash" of last processed bucket
	metaLease         = "compact_lease"      // unix-ms until which a pass holds the lease
)

// getMetaInt reads an integer meta value via q (a *sql.DB or a tx Conn), or def
// if the key is absent or unparseable.
func getMetaInt(ctx context.Context, q store.Conn, key string, def int64) (int64, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if perr != nil {
		return def, nil
	}
	return n, nil
}

// setMetaInt upserts an integer meta value on the given connection.
func setMetaInt(ctx context.Context, c store.Conn, key string, v int64) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, strconv.FormatInt(v, 10))
	return err
}

// getMetaStr reads a string meta value, or def if absent.
func getMetaStr(ctx context.Context, q store.Conn, key, def string) (string, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	return s, nil
}

func setMetaStr(ctx context.Context, c store.Conn, key, v string) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, v)
	return err
}

// Epoch returns the current activity epoch (>= 1). The epoch is a logical clock
// that advances once per gated compaction pass (~once per active session), so
// "staleness" is measured in engagement rather than wall-clock time.
func (e *Engine) Epoch(ctx context.Context) (int64, error) {
	return getMetaInt(ctx, e.st.DB, metaActivityEpoch, 1)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestEpochDefaultsToOne -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): activity-epoch + meta helpers for compaction"
```

---

### Task 3: Recall stamps `last_used_epoch` on surfaced rows

**Files:**
- Modify: `internal/memory/recall.go`
- Modify: `internal/memory/compact.go` (add `stampRecalled`)
- Test: `internal/memory/recall_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/recall_test.go`:

```go
func TestRecallStampsLastUsedEpoch(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(12_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id, _, err := e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "store wal pragmas",
		Body:   "open applies wal and busy timeout pragmas on every connection",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pretend we are at epoch 7.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaInt(ctx, c, metaActivityEpoch, 7)
	}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		Files:       []string{"internal/store/store.go"},
		Branch:      "main",
		TokenBudget: 100000,
	})
	if err != nil || len(res) == 0 {
		t.Fatalf("recall returned %d results, err=%v", len(res), err)
	}

	var epoch, count int64
	if err := e.st.DB.QueryRowContext(ctx,
		`SELECT last_used_epoch, recall_count FROM memories WHERE id=?`, id).
		Scan(&epoch, &count); err != nil {
		t.Fatal(err)
	}
	if epoch != 7 {
		t.Errorf("last_used_epoch = %d, want 7", epoch)
	}
	if count != 1 {
		t.Errorf("recall_count = %d, want 1", count)
	}
}
```

> Ensure `recall_test.go` imports `"git.gerwim.nl/gerwim/recarto/internal/store"` (add to the import block if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestRecallStampsLastUsedEpoch -v`
Expected: FAIL — `last_used_epoch = 0, want 7` (recall does not stamp yet).

- [ ] **Step 3: Add `stampRecalled` and call it from `Recall`**

Add to `internal/memory/compact.go`:

```go
// stampRecalled bumps last_used_epoch to the current activity epoch and
// increments recall_count for the surfaced rows, in ONE batched UPDATE. It is
// the decay subsystem's "time since last used" signal. Best-effort: callers
// ignore the error so a stamping failure never fails an otherwise-good recall.
// It runs in its own WriteTx, OUTSIDE any enclosing transaction (the store's
// write mutex is non-reentrant), alongside Recall's existing ledger writes.
func (e *Engine) stampRecalled(ctx context.Context, results []RecallResult) error {
	if len(results) == 0 {
		return nil
	}
	epoch, err := e.Epoch(ctx)
	if err != nil {
		return err
	}
	ph := make([]string, 0, len(results))
	args := make([]any, 0, len(results)+1)
	args = append(args, epoch)
	for _, r := range results {
		ph = append(ph, "?")
		args = append(args, r.Record.ID)
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`UPDATE memories SET last_used_epoch = ?, recall_count = recall_count + 1
			  WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		return err
	})
}
```

In `internal/memory/recall.go`, inside `Recall`, replace the success tail (the two `led.Incr` lines before `return out, nil`) so stamping happens just before returning:

```go
	_ = e.led.Incr(ctx, "lifetime", "memories_recalled", int64(len(out)))
	_ = e.led.Incr(ctx, "lifetime", "recall_tokens_injected", int64(used))
	// Stamp recall usage so decay can measure "epochs since last used"
	// (Phase 3 compaction). Best-effort — must not fail the recall.
	_ = e.stampRecalled(ctx, out)
	return out, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestRecall' -v`
Expected: PASS — `TestRecallStampsLastUsedEpoch` and the existing recall tests stay green.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/recall.go internal/memory/compact.go internal/memory/recall_test.go
git commit -m "feat(memory): recall stamps last_used_epoch for decay"
```

---

### Task 4: Decay scoring (pure functions)

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go`:

```go
func TestRelevanceDecaysWithAge(t *testing.T) {
	fresh := relevance(KindLearning, 0, 0, 1.0)
	stale := relevance(KindLearning, 60, 0, 1.0)
	if !(fresh > stale) {
		t.Errorf("relevance should decay with age: fresh=%v stale=%v", fresh, stale)
	}
}

func TestActionDecaysFasterThanLearning(t *testing.T) {
	age := 10.0
	if !(relevance(KindAction, age, 0, 1.0) < relevance(KindLearning, age, 0, 1.0)) {
		t.Error("action should decay faster than learning at the same age")
	}
}

func TestEvictableKinds(t *testing.T) {
	for _, k := range []Kind{KindAction, KindPreference, KindLearning} {
		if !evictable(k) {
			t.Errorf("%s should be evictable", k)
		}
	}
	for _, k := range []Kind{KindDecision, KindBug, KindDoNotRepeat} {
		if evictable(k) {
			t.Errorf("%s must be exempt from eviction", k)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestRelevance|TestAction|TestEvictable' -v`
Expected: FAIL — `relevance`/`evictable` undefined.

- [ ] **Step 3: Implement the scoring functions**

Add to `internal/memory/compact.go` (add `"math"` to its import block):

```go
// evictable reports whether a kind may be archived by decay. do_not_repeat is
// pinned; decision and bug carry durable rationale/fix history and are exempt
// (they can still be deduped). Only action/preference/learning decay.
func evictable(k Kind) bool {
	switch k {
	case KindAction, KindPreference, KindLearning:
		return true
	}
	return false
}

// halfLifeEpochs is the decay half-life (in activity epochs) for a kind.
func halfLifeEpochs(k Kind) float64 {
	if k == KindAction {
		return halfLifeActionEpochs
	}
	return halfLifeStdEpochs // preference / learning
}

// relevance scores an eviction candidate; lower = more evictable. Decay is
// exponential in epochs-since-last-used, boosted by reinforcement and scaled by
// confidence. It only RANKS what to evict when over budget — it never by itself
// triggers eviction.
func relevance(k Kind, ageEpochs float64, reinforcement int, confidence float64) float64 {
	if ageEpochs < 0 {
		ageEpochs = 0
	}
	decay := math.Exp(-ageEpochs / halfLifeEpochs(k))
	return decay * (1 + math.Log1p(float64(reinforcement))) * confidence
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestRelevance|TestAction|TestEvictable' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): decay scoring + evictable-kind policy"
```

---

### Task 5: Superseded-record GC

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go`:

```go
func TestGCSupersededDeletesRows(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(13_000_000)))
	ctx := context.Background()
	oldID, _, _ := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body: "we will store markdown files committed in the repo"})
	// A distinct-body superseding record marks oldID superseded.
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body:         "markdown is on-demand export only, sqlite is source of truth",
		SupersedesID: oldID}); err != nil {
		t.Fatal(err)
	}

	n, err := e.gcSuperseded(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("gc'd %d superseded rows, want 1", n)
	}
	var remaining int
	e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE status='superseded'`).Scan(&remaining)
	if remaining != 0 {
		t.Errorf("%d superseded rows remain after GC", remaining)
	}
	// The superseding (active) row survives.
	var active int
	e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE status='active'`).Scan(&active)
	if active != 1 {
		t.Errorf("active rows = %d, want 1", active)
	}
}

func TestGCSupersededDryRunCounts(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(13_500_000)))
	ctx := context.Background()
	oldID, _, _ := e.Record(ctx, NewMemory{Kind: KindDecision, Body: "old fact about the thing"})
	e.Record(ctx, NewMemory{Kind: KindDecision, Body: "totally new and different statement", SupersedesID: oldID})

	n, err := e.gcSuperseded(ctx, true) // dry-run
	if err != nil || n != 1 {
		t.Fatalf("dry-run count = %d (err %v), want 1", n, err)
	}
	var remaining int
	e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE status='superseded'`).Scan(&remaining)
	if remaining != 1 {
		t.Errorf("dry-run must not delete; remaining superseded = %d, want 1", remaining)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestGCSuperseded -v`
Expected: FAIL — `e.gcSuperseded undefined`.

- [ ] **Step 3: Implement `deleteMemoryRows` + `gcSuperseded`**

Add to `internal/memory/compact.go`:

```go
// idsWithStatus returns the ids of rows in the given status (read on the pooled DB).
func (e *Engine) idsWithStatus(ctx context.Context, status string) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT id FROM memories WHERE status=?`, status)
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

// deleteMemoryRows removes the lexical index entries for ids then deletes the
// memory rows (CASCADE clears memory_refs + memory_minhash). Runs on the
// caller's transaction connection.
func (e *Engine) deleteMemoryRows(ctx context.Context, c store.Conn, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if err := e.lex.Delete(ctx, c, id); err != nil {
			return err
		}
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	_, err := c.ExecContext(ctx,
		`DELETE FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

// gcSuperseded hard-deletes all superseded rows. Returns the count removed (or,
// for dryRun, the count that would be removed).
func (e *Engine) gcSuperseded(ctx context.Context, dryRun bool) (int, error) {
	ids, err := e.idsWithStatus(ctx, "superseded")
	if err != nil {
		return 0, err
	}
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.deleteMemoryRows(ctx, c, ids)
	}); err != nil {
		return 0, err
	}
	return len(ids), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestGCSuperseded -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): superseded-record GC"
```

---

### Task 6: `LiveTokens` + budget-triggered eviction

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Extend the import block of `internal/memory/compact_test.go` so it reads:

```go
import (
	"context"
	"sync"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)
```

> (`sync` is used by Task 10; adding it now keeps the import block stable.)

Add the shared test helpers (used by Tasks 6–8):

```go
// insertActive inserts an active memory directly (bypassing Record's dedup so
// the test controls the row set), then sets its last_used_epoch. ids.New()
// guarantees a unique id even for identical bodies.
func insertActive(t *testing.T, e *Engine, kind Kind, body string, lastUsedEpoch int64) string {
	t.Helper()
	ctx := context.Background()
	id := ids.New()
	nm := NewMemory{Kind: kind, Body: body}
	st := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		if err := e.insert(ctx, c, id, nm, st, simHash64(set), bandHashes(minHashSignature(set)), 1); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, `UPDATE memories SET last_used_epoch=? WHERE id=?`, lastUsedEpoch, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func statusOf(t *testing.T, e *Engine, id string) string {
	t.Helper()
	var s string
	if err := e.st.DB.QueryRowContext(context.Background(),
		`SELECT status FROM memories WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEvictQuietRepoInvariant(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(14_000_000)))
	ctx := context.Background()
	id := insertActive(t, e, KindLearning, "a small but still-true fact", 1)
	// Epoch advanced far into the future, but the store is tiny.
	archived, _, _, err := e.evictToBudget(ctx, 100000, DefaultBudgetTokens, false)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Errorf("quiet repo under budget archived %d rows, want 0", archived)
	}
	if statusOf(t, e, id) != "active" {
		t.Error("memory was archived despite the store being far under budget")
	}
}

func TestEvictArchivesLowestScoredFirst(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(14_100_000)))
	ctx := context.Background()
	body := "this body is sized so three of them exceed the test budget xxxx"
	stale := insertActive(t, e, KindLearning, body+" one", 1)  // age 99 -> lowest relevance
	mid := insertActive(t, e, KindLearning, body+" two", 90)   // age 10
	fresh := insertActive(t, e, KindLearning, body+" three", 95) // age 5
	rowTok := ledger.EstimateTokens(body + " one") // each row ~ same size
	budget := 2*rowTok + 1                          // fits exactly two rows

	archived, before, after, err := e.evictToBudget(ctx, 100, budget, false)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Fatalf("archived %d, want 1 (before=%d after=%d budget=%d)", archived, before, after, budget)
	}
	if statusOf(t, e, stale) != "archived" {
		t.Error("the stalest (lowest-relevance) memory should be archived first")
	}
	if statusOf(t, e, mid) != "active" || statusOf(t, e, fresh) != "active" {
		t.Error("higher-relevance memories must be kept")
	}
}

func TestEvictExemptKindsNeverArchived(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(14_200_000)))
	ctx := context.Background()
	dec := insertActive(t, e, KindDecision, "a durable architectural decision record", 1)
	bug := insertActive(t, e, KindBug, "a fix we never want to lose from history", 1)
	// Force eviction pressure with a tiny budget.
	if _, _, _, err := e.evictToBudget(ctx, 100, 1, false); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, e, dec) != "active" || statusOf(t, e, bug) != "active" {
		t.Error("decision/bug must be exempt from eviction even under budget pressure")
	}
}

func TestEvictRemovesFromFTS(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(14_300_000)))
	ctx := context.Background()
	insertActive(t, e, KindLearning, "uniquetoken about archival removal from index", 1)
	if _, _, _, err := e.evictToBudget(ctx, 100, 1, false); err != nil {
		t.Fatal(err)
	}
	res, err := e.lex.Search(ctx, "uniquetoken", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("archived memory still in FTS index: %d hits", len(res))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestEvict -v`
Expected: FAIL — `e.evictToBudget undefined`.

- [ ] **Step 3: Implement `LiveTokens` + `evictToBudget`**

Add to `internal/memory/compact.go` (add `"sort"` and the ledger import `"git.gerwim.nl/gerwim/recarto/internal/ledger"` to the import block):

```go
// candidate is an in-memory snapshot of an active row used for budgeting/eviction.
type candidate struct {
	id            string
	kind          Kind
	tokens        int
	lastUsedEpoch int64
	reinforcement int
	confidence    float64
}

// loadActive scans every active row once, returning each as a candidate and the
// total live-set token count. Token cost mirrors recall's tokensOf exactly
// (EstimateTokens(title + " " + body)).
func (e *Engine) loadActive(ctx context.Context) (cands []candidate, liveTokens int, err error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id, kind, title, body, last_used_epoch, reinforcement_count, confidence
		   FROM memories WHERE status='active'`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var c candidate
		var kind, title, body string
		if err := rows.Scan(&c.id, &kind, &title, &body, &c.lastUsedEpoch,
			&c.reinforcement, &c.confidence); err != nil {
			return nil, 0, err
		}
		c.kind = Kind(kind)
		c.tokens = ledger.EstimateTokens(title + " " + body)
		liveTokens += c.tokens
		cands = append(cands, c)
	}
	return cands, liveTokens, rows.Err()
}

// LiveTokens is the estimated token size of the active memory set.
func (e *Engine) LiveTokens(ctx context.Context) (int, error) {
	_, n, err := e.loadActive(ctx)
	return n, err
}

// archiveRows flips ids to status='archived', stamps archived_epoch, and drops
// their FTS entries (so they stop being lexical candidates).
func (e *Engine) archiveRows(ctx context.Context, c store.Conn, ids []string, epoch, now int64) error {
	for _, id := range ids {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET status='archived', archived_epoch=?, updated_at=?
			  WHERE id=? AND status='active'`, epoch, now, id); err != nil {
			return err
		}
		if err := e.lex.Delete(ctx, c, id); err != nil {
			return err
		}
	}
	return nil
}

// evictToBudget archives the lowest-relevance evictable rows until the live set
// is within budget. If already within budget it archives nothing (the quiet-repo
// invariant): eviction is budget-triggered, never age-triggered. Returns the
// number archived and live tokens before/after.
func (e *Engine) evictToBudget(ctx context.Context, epoch int64, budget int, dryRun bool) (archived, before, after int, err error) {
	cands, live, err := e.loadActive(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	before, after = live, live
	if live <= budget {
		return 0, before, after, nil // under budget: never evict
	}

	// Eviction-eligible candidates, ranked by relevance ascending (worst first).
	var elig []candidate
	for _, c := range cands {
		if evictable(c.kind) {
			elig = append(elig, c)
		}
	}
	sort.Slice(elig, func(i, j int) bool {
		ri := relevance(elig[i].kind, float64(epoch-elig[i].lastUsedEpoch), elig[i].reinforcement, elig[i].confidence)
		rj := relevance(elig[j].kind, float64(epoch-elig[j].lastUsedEpoch), elig[j].reinforcement, elig[j].confidence)
		return ri < rj
	})

	var toArchive []string
	for _, c := range elig {
		if after <= budget {
			break
		}
		toArchive = append(toArchive, c.id)
		after -= c.tokens
	}
	if dryRun || len(toArchive) == 0 {
		return len(toArchive), before, after, nil
	}
	now := e.clk.Now().UnixMilli()
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.archiveRows(ctx, c, toArchive, epoch, now)
	}); err != nil {
		return 0, before, before, err
	}
	return len(toArchive), before, after, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestEvict -v`
Expected: PASS — all four eviction tests, including the quiet-repo invariant.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): budget-triggered decay eviction (archive)"
```

---

### Task 7: Global dedup sweep (bounded, resumable)

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go`:

```go
func TestDedupSweepMergesNearDuplicates(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(15_000_000)))
	ctx := context.Background()
	// Two IDENTICAL-body same-kind rows inserted directly (Record would have
	// deduped them at write time). They share all MinHash bands -> collide.
	a := insertActive(t, e, KindLearning, "set busy timeout so writers wait instead of erroring", 5)
	b := insertActive(t, e, KindLearning, "set busy timeout so writers wait instead of erroring", 5)

	merged, err := e.dedupSweep(ctx, false, defaultDedupBuckets)
	if err != nil {
		t.Fatal(err)
	}
	if merged != 1 {
		t.Fatalf("merged %d, want 1", merged)
	}
	// Exactly one of {a,b} is now superseded; the other is the active canonical
	// with a bumped reinforcement_count.
	sa, sb := statusOf(t, e, a), statusOf(t, e, b)
	if !((sa == "superseded") != (sb == "superseded")) {
		t.Fatalf("expected exactly one superseded; got a=%s b=%s", sa, sb)
	}
	var rc int
	e.st.DB.QueryRowContext(ctx,
		`SELECT reinforcement_count FROM memories WHERE status='active'`).Scan(&rc)
	if rc < 1 {
		t.Errorf("canonical reinforcement_count = %d, want >= 1", rc)
	}
}

func TestDedupSweepRespectsKind(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(15_100_000)))
	ctx := context.Background()
	a := insertActive(t, e, KindLearning, "identical body across two different kinds here", 5)
	b := insertActive(t, e, KindPreference, "identical body across two different kinds here", 5)

	merged, err := e.dedupSweep(ctx, false, defaultDedupBuckets)
	if err != nil {
		t.Fatal(err)
	}
	if merged != 0 {
		t.Errorf("merged %d across kinds, want 0", merged)
	}
	if statusOf(t, e, a) != "active" || statusOf(t, e, b) != "active" {
		t.Error("rows of different kinds must not be merged")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestDedupSweep -v`
Expected: FAIL — `e.dedupSweep undefined`.

- [ ] **Step 3: Implement the bounded dedup sweep**

Add to `internal/memory/compact.go`:

```go
// bucket identifies a MinHash collision bucket.
type bucket struct {
	band int
	hash int64
}

// parseCursor decodes the "band,hash" dedup cursor; the zero value (-1, min)
// selects every bucket as "after the cursor".
func parseCursor(s string) bucket {
	b := bucket{band: -1, hash: -1 << 62}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) == 2 {
		if bd, err := strconv.Atoi(parts[0]); err == nil {
			b.band = bd
		}
		if h, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			b.hash = h
		}
	}
	return b
}

func formatCursor(b bucket) string {
	return strconv.Itoa(b.band) + "," + strconv.FormatInt(b.hash, 10)
}

// dedupMember is an active row sharing a collision bucket.
type dedupMember struct {
	id            string
	body          string
	reinforcement int
	confidence    float64
	createdAt     int64
}

// dedupSweep walks up to maxBuckets MinHash collision buckets (resuming from the
// stored cursor), and within each merges same-kind active near-duplicates
// (Jaccard >= dedupThreshold) into the highest-reinforced canonical, marking the
// rest superseded. Returns the number of rows superseded by merging.
func (e *Engine) dedupSweep(ctx context.Context, dryRun bool, maxBuckets int) (merged int, err error) {
	cursorStr, err := getMetaStr(ctx, e.st.DB, metaDedupCursor, "")
	if err != nil {
		return 0, err
	}
	cur := parseCursor(cursorStr)

	// Gather candidate buckets via the pooled DB (read-only) so the write mutex
	// is not held across the GROUP BY scan.
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT band, hash FROM memory_minhash
		  WHERE (band, hash) > (?, ?)
		  GROUP BY band, hash HAVING COUNT(*) > 1
		  ORDER BY band, hash LIMIT ?`, cur.band, cur.hash, maxBuckets)
	if err != nil {
		return 0, err
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.band, &b.hash); err != nil {
			rows.Close()
			return 0, err
		}
		buckets = append(buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := e.clk.Now().UnixMilli()
	last := cur
	wrap := len(buckets) < maxBuckets // fewer than the budget => reached the end

	for _, bk := range buckets {
		n, err := e.mergeBucket(ctx, bk, now, dryRun)
		if err != nil {
			return merged, err
		}
		merged += n
		last = bk
	}

	if dryRun {
		return merged, nil
	}
	next := formatCursor(last)
	if wrap {
		next = "" // start over next pass
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaStr(ctx, c, metaDedupCursor, next)
	}); err != nil {
		return merged, err
	}
	return merged, nil
}

// mergeBucket merges same-kind active near-dups within one collision bucket.
func (e *Engine) mergeBucket(ctx context.Context, bk bucket, now int64, dryRun bool) (int, error) {
	// Members are grouped by kind: a bucket can hold rows of several kinds.
	byKind := map[Kind][]dedupMember{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT m.id, m.kind, m.body, m.reinforcement_count, m.confidence, m.created_at
		   FROM memory_minhash mh JOIN memories m ON m.id = mh.memory_id
		  WHERE mh.band=? AND mh.hash=? AND m.status='active'`, bk.band, bk.hash)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var mem dedupMember
		var kind string
		if err := rows.Scan(&mem.id, &kind, &mem.body, &mem.reinforcement, &mem.confidence, &mem.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		byKind[Kind(kind)] = append(byKind[Kind(kind)], mem)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	merged := 0
	for _, members := range byKind {
		if len(members) < 2 {
			continue
		}
		// Greedy clustering: each member joins the first canonical it matches.
		type cluster struct {
			canon dedupMember
			set   map[string]struct{}
			dups  []dedupMember
		}
		var clusters []*cluster
		for _, mem := range members {
			set := shingleSet(mem.body)
			joined := false
			for _, cl := range clusters {
				if jaccard(set, cl.set) >= dedupThreshold {
					// Keep the better-reinforced row as canonical.
					if betterCanonical(mem, cl.canon) {
						cl.dups = append(cl.dups, cl.canon)
						cl.canon = mem
						cl.set = set
					} else {
						cl.dups = append(cl.dups, mem)
					}
					joined = true
					break
				}
			}
			if !joined {
				clusters = append(clusters, &cluster{canon: mem, set: set})
			}
		}
		for _, cl := range clusters {
			if len(cl.dups) == 0 {
				continue
			}
			merged += len(cl.dups)
			if dryRun {
				continue
			}
			if err := e.applyMerge(ctx, cl.canon.id, cl.dups, now); err != nil {
				return merged, err
			}
		}
	}
	return merged, nil
}

// betterCanonical reports whether a should be canonical over b: more
// reinforcement, then higher confidence, then newer.
func betterCanonical(a, b dedupMember) bool {
	if a.reinforcement != b.reinforcement {
		return a.reinforcement > b.reinforcement
	}
	if a.confidence != b.confidence {
		return a.confidence > b.confidence
	}
	return a.createdAt > b.createdAt
}

// applyMerge supersedes dups into canon and bumps canon's reinforcement_count.
func (e *Engine) applyMerge(ctx context.Context, canonID string, dups []dedupMember, now int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, d := range dups {
			if _, err := c.ExecContext(ctx,
				`UPDATE memories SET status='superseded', superseded_by=?, updated_at=?
				  WHERE id=? AND status='active'`, canonID, now, d.id); err != nil {
				return err
			}
			if err := e.lex.Delete(ctx, c, d.id); err != nil {
				return err
			}
		}
		_, err := c.ExecContext(ctx,
			`UPDATE memories SET reinforcement_count = reinforcement_count + ?, updated_at=?
			  WHERE id=?`, len(dups), now, canonID)
		return err
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestDedupSweep -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): bounded global dedup sweep"
```

---

### Task 8: Archive GC (grace period)

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go`:

```go
func TestArchiveGCRespectsGrace(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(16_000_000)))
	ctx := context.Background()
	id := insertActive(t, e, KindLearning, "a memory archived long ago in epoch terms", 1)
	// Archive it at epoch 1.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.archiveRows(ctx, c, []string{id}, 1, 16_000_000)
	}); err != nil {
		t.Fatal(err)
	}

	// At epoch within grace: not collected.
	n, err := e.gcArchived(ctx, 1+archiveGraceEpochs-1, false)
	if err != nil || n != 0 {
		t.Fatalf("within grace gc'd %d (err %v), want 0", n, err)
	}
	// At epoch past grace: collected.
	n, err = e.gcArchived(ctx, 1+archiveGraceEpochs+1, false)
	if err != nil || n != 1 {
		t.Fatalf("past grace gc'd %d (err %v), want 1", n, err)
	}
	var total int
	e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE id=?`, id).Scan(&total)
	if total != 0 {
		t.Error("archived row past grace should be hard-deleted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestArchiveGC -v`
Expected: FAIL — `e.gcArchived undefined`.

- [ ] **Step 3: Implement `gcArchived`**

Add to `internal/memory/compact.go`:

```go
// gcArchived hard-deletes archived rows older than archiveGraceEpochs (measured
// in epochs against the supplied current epoch). Returns the count removed.
func (e *Engine) gcArchived(ctx context.Context, epoch int64, dryRun bool) (int, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE status='archived' AND (? - archived_epoch) > ?`, epoch, archiveGraceEpochs)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.deleteMemoryRows(ctx, c, ids)
	}); err != nil {
		return 0, err
	}
	return len(ids), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestArchiveGC -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): archive GC with epoch grace"
```

---

### Task 9: `Engine.Compact` orchestration (gate, lease, epoch, report)

**Files:**
- Modify: `internal/memory/compact.go`
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go`:

```go
func TestCompactAdvancesEpochWhenForced(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()
	rep, err := e.Compact(ctx, CompactOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ran || rep.Epoch != 2 { // fresh epoch is 1; one pass -> 2
		t.Fatalf("ran=%v epoch=%d, want ran=true epoch=2", rep.Ran, rep.Epoch)
	}
}

func TestCompactSkipsWithinInterval(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()
	if _, err := e.Compact(ctx, CompactOptions{}); err != nil { // first run sets last_run
		t.Fatal(err)
	}
	rep, err := e.Compact(ctx, CompactOptions{}) // immediately again: gated
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ran {
		t.Error("second compaction within minInterval should be skipped")
	}
	// --force overrides the gate.
	rep, err = e.Compact(ctx, CompactOptions{Force: true})
	if err != nil || !rep.Ran {
		t.Fatalf("forced compaction should run; ran=%v err=%v", rep.Ran, err)
	}
}

func TestCompactDryRunDoesNotWrite(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()
	// Seed a superseded row.
	oldID, _, _ := e.Record(ctx, NewMemory{Kind: KindDecision, Body: "old decision text here"})
	e.Record(ctx, NewMemory{Kind: KindDecision, Body: "a wholly different decision now", SupersedesID: oldID})

	before, _ := e.Epoch(ctx)
	rep, err := e.Compact(ctx, CompactOptions{Force: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SupersededGCd != 1 {
		t.Errorf("dry-run report SupersededGCd = %d, want 1", rep.SupersededGCd)
	}
	after, _ := e.Epoch(ctx)
	if after != before {
		t.Errorf("dry-run advanced the epoch %d -> %d", before, after)
	}
	var superseded int
	e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE status='superseded'`).Scan(&superseded)
	if superseded != 1 {
		t.Error("dry-run must not delete superseded rows")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestCompact -v`
Expected: FAIL — `CompactOptions`/`e.Compact` undefined.

- [ ] **Step 3: Implement `CompactOptions`, `CompactReport`, and `Compact`**

Add to `internal/memory/compact.go`:

```go
// errCompactSkip is the sentinel used to roll back the gate transaction when a
// pass must not run (interval gate not elapsed, or another pass holds the lease).
var errCompactSkip = errors.New("memory: compaction skipped")

// CompactOptions configures a compaction pass.
type CompactOptions struct {
	Force        bool // skip the min-interval gate (still respects the lease)
	DryRun       bool // compute the report without writing anything
	BudgetTokens int  // 0 -> DefaultBudgetTokens
}

// CompactReport summarizes a pass.
type CompactReport struct {
	Ran              bool
	Epoch            int64
	SupersededGCd    int
	Archived         int
	Deduped          int
	ArchiveGCd       int
	LiveTokensBefore int
	LiveTokensAfter  int
	Budget           int
}

// Compact runs one bounded compaction pass: superseded-GC, budget-triggered
// decay eviction, a bounded dedup sweep, and archive-GC. It is gated to once per
// minInterval (unless Force) and serialized across worktrees by a single-flight
// lease in meta. A skipped pass returns CompactReport{Ran:false}, nil.
func (e *Engine) Compact(ctx context.Context, opts CompactOptions) (CompactReport, error) {
	now := e.clk.Now().UnixMilli()
	budget := opts.BudgetTokens
	if budget <= 0 {
		budget = DefaultBudgetTokens
	}
	rep := CompactReport{Budget: budget}

	if opts.DryRun {
		epoch, err := e.Epoch(ctx)
		if err != nil {
			return rep, err
		}
		return e.runSteps(ctx, epoch, budget, true)
	}

	// Gate + lease + epoch advance, atomically. Rolling back on errCompactSkip
	// leaves the epoch and lease untouched.
	var epoch int64
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		last, err := getMetaInt(ctx, c, metaLastRunAt, 0)
		if err != nil {
			return err
		}
		if !opts.Force && now-last < minIntervalMs {
			return errCompactSkip
		}
		leaseUntil, err := getMetaInt(ctx, c, metaLease, 0)
		if err != nil {
			return err
		}
		if now < leaseUntil {
			return errCompactSkip // another pass is in flight
		}
		cur, err := getMetaInt(ctx, c, metaActivityEpoch, 1)
		if err != nil {
			return err
		}
		epoch = cur + 1
		if err := setMetaInt(ctx, c, metaActivityEpoch, epoch); err != nil {
			return err
		}
		if err := setMetaInt(ctx, c, metaLastRunAt, now); err != nil {
			return err
		}
		return setMetaInt(ctx, c, metaLease, now+leaseTTLMs)
	})
	if errors.Is(err, errCompactSkip) {
		return CompactReport{Ran: false}, nil
	}
	if err != nil {
		return rep, err
	}
	defer func() { _ = e.clearLease(ctx) }()

	rep, err = e.runSteps(ctx, epoch, budget, false)
	if err != nil {
		return rep, err
	}

	// Best-effort lifetime counters (OUTSIDE the step transactions).
	_ = e.led.Incr(ctx, "lifetime", "compactions", 1)
	_ = e.led.Incr(ctx, "lifetime", "memories_archived", int64(rep.Archived))
	_ = e.led.Incr(ctx, "lifetime", "memories_deduped", int64(rep.Deduped))
	_ = e.led.Incr(ctx, "lifetime", "memories_gc", int64(rep.SupersededGCd+rep.ArchiveGCd))
	return rep, nil
}

// runSteps executes the four pass steps in order and assembles the report.
func (e *Engine) runSteps(ctx context.Context, epoch int64, budget int, dryRun bool) (CompactReport, error) {
	rep := CompactReport{Ran: true, Epoch: epoch, Budget: budget}
	var err error
	if rep.SupersededGCd, err = e.gcSuperseded(ctx, dryRun); err != nil {
		return rep, err
	}
	if rep.Archived, rep.LiveTokensBefore, rep.LiveTokensAfter, err = e.evictToBudget(ctx, epoch, budget, dryRun); err != nil {
		return rep, err
	}
	if rep.Deduped, err = e.dedupSweep(ctx, dryRun, defaultDedupBuckets); err != nil {
		return rep, err
	}
	if rep.ArchiveGCd, err = e.gcArchived(ctx, epoch, dryRun); err != nil {
		return rep, err
	}
	return rep, nil
}

// clearLease releases the single-flight lease so the next session is not blocked
// for the full TTL after a fast pass. Best-effort.
func (e *Engine) clearLease(ctx context.Context) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaInt(ctx, c, metaLease, 0)
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestCompact -v`
Expected: PASS — advance, skip/force, and dry-run-no-write.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact.go internal/memory/compact_test.go
git commit -m "feat(memory): Engine.Compact orchestration with gate + lease"
```

---

### Task 10: Single-flight concurrency proof (in-process)

**Files:**
- Test: `internal/memory/compact_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/compact_test.go` (`"sync"` was already added to the import block in Task 6):

```go
func TestCompactSingleFlightIsConsistent(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()

	const goroutines = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ran := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := e.Compact(ctx, CompactOptions{Force: true})
			if err != nil {
				t.Errorf("compact: %v", err)
				return
			}
			if rep.Ran {
				mu.Lock()
				ran++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Invariant: the epoch advances exactly once per pass that actually ran.
	epoch, err := e.Epoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != int64(1+ran) { // fresh epoch 1 + one bump per ran pass
		t.Errorf("epoch = %d, want %d (1 + ran=%d)", epoch, 1+ran, ran)
	}
	if ran < 1 {
		t.Error("at least one concurrent pass should have run")
	}
	var integrity string
	e.st.DB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Errorf("integrity_check = %q", integrity)
	}
}
```

- [ ] **Step 2: Run test to verify it fails or passes**

Run: `go test ./internal/memory/ -run TestCompactSingleFlight -race -v`
Expected: PASS (the lease + atomic epoch advance from Task 9 already provide this). If it FAILS, the lease logic has a race — fix `Compact` before proceeding.

- [ ] **Step 3: (No new implementation if green)**

This task verifies Task 9's lease. If green, no code change. If red, the typical fix is ensuring the gate/lease/epoch read-modify-write all happen inside the single `WriteTx` (already the case in Task 9) — re-check no read of epoch/lease leaks outside it.

- [ ] **Step 4: Run the full memory suite with the race detector**

Run: `go test ./internal/memory/ -race`
Expected: PASS, race-clean.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact_test.go
git commit -m "test(memory): single-flight compaction concurrency invariant"
```

---

### Task 11: CLI `recarto compact`

**Files:**
- Create: `internal/cli/compact.go`
- Create: `internal/cli/compact_test.go`
- Modify: `internal/cli/cli.go`

- [ ] **Step 1: Write the failing test**

Create `internal/cli/compact_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCompactForceReports(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Compact(ctx, a, &buf, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"epoch", "archived:", "deduped:", "live tokens:"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output missing %q:\n%s", want, out)
		}
	}
}

func TestCompactDryRunPrefixed(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Compact(ctx, a, &buf, []string{"--force", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("dry-run output should be prefixed:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestCompact -v`
Expected: FAIL — `Compact` (CLI) undefined.

- [ ] **Step 3: Implement the command and register it**

Create `internal/cli/compact.go`:

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

// Compact runs a compaction/decay pass over this repo's memory store.
func Compact(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	force := fs.Bool("force", false, "ignore the once-per-interval gate")
	dry := fs.Bool("dry-run", false, "report planned changes without writing")
	budget := fs.Int("budget-tokens", 0, "live-set token budget (0 = default)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	rep, err := a.Engine.Compact(ctx, memory.CompactOptions{
		Force: *force, DryRun: *dry, BudgetTokens: *budget,
	})
	if err != nil {
		return err
	}
	if !rep.Ran {
		fmt.Fprintln(w, "compaction skipped (ran recently; use --force to override)")
		return nil
	}
	p := ""
	if *dry {
		p = "[dry-run] "
	}
	fmt.Fprintf(w, "%sepoch:        %d\n", p, rep.Epoch)
	fmt.Fprintf(w, "%ssuperseded:   %d gc'd\n", p, rep.SupersededGCd)
	fmt.Fprintf(w, "%sarchived:     %d\n", p, rep.Archived)
	fmt.Fprintf(w, "%sdeduped:      %d\n", p, rep.Deduped)
	fmt.Fprintf(w, "%sarchive gc:   %d\n", p, rep.ArchiveGCd)
	fmt.Fprintf(w, "%slive tokens:  %d -> %d (budget %d)\n", p, rep.LiveTokensBefore, rep.LiveTokensAfter, rep.Budget)
	return nil
}
```

In `internal/cli/cli.go`, add `compact` to `known()`:

```go
	case "status", "doctor", "remember", "recall", "bug", "memory", "serve", "hook", "index", "graph", "compact":
		return true
```

Add the dispatch case (after the `graph` case):

```go
	case "compact":
		return Compact(ctx, a, out, rest)
```

Add a usage line (after the `graph` line in `usage`):

```go
	fmt.Fprintln(w, "  compact           prune/decay memory (--force --dry-run --budget-tokens)")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestCompact -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/compact.go internal/cli/compact_test.go internal/cli/cli.go
git commit -m "feat(cli): recarto compact command"
```

---

### Task 12: `status` shows compaction state

**Files:**
- Modify: `internal/cli/status.go`
- Modify: `internal/cli/status_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/status_test.go` (a new test; keep the existing one intact):

```go
func TestStatusShowsCompactionState(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Status(ctx, a, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"epoch", "archived", "live tokens", "compactions"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("status missing %q:\n%s", want, buf.String())
		}
	}
}
```

> Ensure `status_test.go` imports `"bytes"` and `"strings"` (the existing test likely already uses them; add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestStatusShowsCompactionState -v`
Expected: FAIL — the compaction lines are missing.

- [ ] **Step 3: Extend `Status`**

In `internal/cli/status.go`, add a compaction section before the final `return nil`. Add `"git.gerwim.nl/gerwim/recarto/internal/memory"` to the imports (for `memory.DefaultBudgetTokens`):

```go
	// Compaction / decay state.
	epoch, err := a.Engine.Epoch(ctx)
	if err != nil {
		return err
	}
	live, err := a.Engine.LiveTokens(ctx)
	if err != nil {
		return err
	}
	var archived int
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE status='archived'`).Scan(&archived); err != nil {
		return err
	}
	fmt.Fprintln(w, "compaction:")
	fmt.Fprintf(w, "  %-14s %d\n", "epoch", epoch)
	fmt.Fprintf(w, "  %-14s %d\n", "archived", archived)
	fmt.Fprintf(w, "  %-14s %d / %d\n", "live tokens", live, memory.DefaultBudgetTokens)

	fmt.Fprintln(w, "ledger (compaction):")
	for _, m := range []string{"compactions", "memories_archived", "memories_deduped", "memories_gc"} {
		v, err := a.Ledger.Get(ctx, "lifetime", m)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-24s %d\n", m, v)
	}
	return nil
```

> Remove the now-duplicated trailing `return nil` if the original ended with one right after the existing ledger loop — there must be exactly one `return nil` at the end of `Status`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestStatus -v`
Expected: PASS — both the existing status test and the new compaction-state test.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/status.go internal/cli/status_test.go
git commit -m "feat(cli): status shows compaction epoch/archived/budget"
```

---

### Task 13: `SessionStart` hook triggers compaction

**Files:**
- Modify: `internal/agents/claudecode/hooks.go`
- Modify: `internal/agents/claudecode/hooks_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/agents/claudecode/hooks_test.go`:

```go
func TestSessionStartRunsCompaction(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var out bytes.Buffer
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"s1","source":"startup"}`), &out); err != nil {
		t.Fatal(err)
	}
	epoch, err := a.Engine.Epoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 2 { // fresh epoch 1 + one gated pass on session start
		t.Errorf("epoch after SessionStart = %d, want 2 (compaction did not run)", epoch)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/claudecode/ -run TestSessionStartRunsCompaction -v`
Expected: FAIL — `epoch ... = 1, want 2`.

- [ ] **Step 3: Add the compaction trigger to `SessionStart`**

In `internal/agents/claudecode/hooks.go`, inside `SessionStart`, after the graph reconcile line and before the `a.Engine.Recall(...)` call, add:

```go
	// Opportunistic memory compaction (best-effort, gated to once per interval,
	// single-flight across worktrees). Runs before recall so this session's
	// recalls stamp the freshly-advanced epoch. Never blocks the agent.
	_, _ = a.Engine.Compact(ctx, memory.CompactOptions{})
```

> `memory` is already imported in `hooks.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agents/claudecode/ -v`
Expected: PASS — the new test plus the existing SessionStart tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agents/claudecode/hooks.go internal/agents/claudecode/hooks_test.go
git commit -m "feat(hooks): SessionStart triggers opportunistic compaction"
```

---

### Task 14: Multi-process compaction concurrency proof

**Files:**
- Create: `internal/memory/multiproc_compact_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/memory/multiproc_compact_test.go` (mirrors `internal/graph/multiproc_test.go`):

```go
package memory_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMultiProcessCompactSharedStore proves that N separate `recarto compact
// --force` PROCESSES against ONE shared store stay integrity-clean and that the
// single-flight lease keeps the epoch consistent (epoch == number of passes that
// actually ran == the "compactions" counter).
func TestMultiProcessCompactSharedStore(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the recarto binary (CGO); skipped under -short")
	}
	bin := filepath.Join(t.TempDir(), "recarto")
	build := exec.Command("go", "build", "-o", bin, "git.gerwim.nl/gerwim/recarto/cmd/recarto")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recarto: %v\n%s", err, out)
	}

	repo := t.TempDir()
	runCompactCmd(t, repo, "git", "init")
	runCompactCmd(t, repo, "git", "config", "user.email", "t@t")
	runCompactCmd(t, repo, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCompactCmd(t, repo, "git", "add", ".")
	runCompactCmd(t, repo, "git", "commit", "-m", "init")

	cache := t.TempDir()
	env := append(os.Environ(), "RECARTO_CACHE_DIR="+cache)

	// Seed a couple of memories (one process, sequential).
	seed := exec.Command(bin, "remember", "learning", "--body", "first seeded learning fact")
	seed.Dir, seed.Env = repo, env
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 5; attempt++ {
				cmd := exec.Command(bin, "compact", "--force")
				cmd.Dir, cmd.Env = repo, env
				out, err := cmd.CombinedOutput()
				if err == nil {
					return
				}
				if strings.Contains(string(out), "SQLITE_BUSY") || strings.Contains(string(out), "database is locked") {
					time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
					continue
				}
				t.Errorf("compact: %v\n%s", err, out)
				return
			}
		}()
	}
	wg.Wait()

	matches, _ := filepath.Glob(filepath.Join(cache, "*", "store.db"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 shared store.db, found %v", matches)
	}
	db, err := sql.Open("sqlite", matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}

	var epoch, compactions int64
	db.QueryRow(`SELECT value FROM meta WHERE key='compact_activity_epoch'`).Scan(&epoch)
	db.QueryRow(`SELECT value FROM counters WHERE scope='lifetime' AND metric='compactions'`).Scan(&compactions)
	// Each pass that ran advanced the epoch once and bumped the counter once.
	// Epoch starts at 1; after `compactions` passes it is 1 + compactions.
	if epoch != 1+compactions {
		t.Errorf("epoch=%d, compactions=%d; want epoch == 1+compactions", epoch, compactions)
	}
	if compactions < 1 {
		t.Error("at least one compaction pass should have run")
	}
}

func runCompactCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
	}
}
```

> This test reads `meta.value`/`counters.value` as integers — `database/sql` scans the TEXT/INTEGER values into `int64` fine here because they hold numeric strings/ints.

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestMultiProcessCompactSharedStore -v`
Expected: PASS (skipped under `-short`). If `epoch != 1+compactions`, the lease/epoch atomicity is broken — revisit Task 9.

- [ ] **Step 3: (No new implementation if green)**

- [ ] **Step 4: Run with the race detector (short to skip the build-heavy proc test)**

Run: `go test ./internal/memory/ -race -short`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/multiproc_compact_test.go
git commit -m "test(memory): multi-process compaction concurrency proof"
```

---

### Task 15: `BenchmarkCompact` over a 50k synthetic store (resolves D8)

**Files:**
- Create: `internal/memory/compact_bench_test.go`

- [ ] **Step 1: Write the benchmark**

Create `internal/memory/compact_bench_test.go`:

```go
package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// BenchmarkCompact measures a full DRY-RUN pass over a 50k-record store. Dry-run
// is idempotent (no writes), so it is repeatable across b.N and exercises every
// read/compute path (live-token scan, eviction ranking, dedup candidate scan,
// GC counts) — the latency that determines whether the synchronous hook path is
// fast enough or a detached process is warranted (spec D8 / §11). Because this
// file is package memory it may call the unexported e.insert / shingleSet / etc.
func BenchmarkCompact(b *testing.B) {
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	st := mustBenchStore(b)
	defer st.Close()
	e := New(st, ledger.New(st), clk)
	ctx := context.Background()

	kinds := []Kind{KindAction, KindPreference, KindLearning, KindDecision, KindBug}
	// Seed 50k rows in ONE transaction (fast). last_used_epoch stays 0 — the
	// dry-run scan/sort cost we are measuring is independent of its exact value.
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		for i := 0; i < 50_000; i++ {
			id := fmt.Sprintf("bench-%d", i)
			body := fmt.Sprintf("synthetic memory number %d about subsystem %d and its behavior", i, i%97)
			nm := NewMemory{Kind: kinds[i%len(kinds)], Body: body}
			set := shingleSet(body)
			if err := e.insert(ctx, c, id, nm, BuildSearchText(nm.Title, nm.Body, nm.Refs),
				simHash64(set), bandHashes(minHashSignature(set)), 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Compact(ctx, CompactOptions{Force: true, DryRun: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func mustBenchStore(b *testing.B) *store.Store {
	b.Helper()
	st, err := store.Open(filepath.Join(b.TempDir(), "store.db"))
	if err != nil {
		b.Fatal(err)
	}
	return st
}
```

- [ ] **Step 2: Run the benchmark**

Run: `go test ./internal/memory/ -run '^$' -bench BenchmarkCompact -benchmem`
Expected: a reported ns/op for a dry-run pass over 50k records.

- [ ] **Step 3: Record the result and the D8 decision**

Capture the ns/op (convert to ms) in the commit message and note it in `.wolf/cerebrum.md`. Decision rule: if a dry-run pass is **well under ~50ms**, the synchronous hook path is confirmed and the detached-spawn follow-up (spec §3) stays deferred. If it is materially higher, open a follow-up to add detached execution.

- [ ] **Step 4: Verify the benchmark file builds under normal test runs**

Run: `go test ./internal/memory/ -run TestEpochDefaultsToOne`
Expected: PASS (the bench file compiles; benchmarks don't run without `-bench`).

- [ ] **Step 5: Commit**

```bash
git add internal/memory/compact_bench_test.go
git commit -m "test(memory): BenchmarkCompact over 50k store (resolves D8: <Xms>)"
```

---

### Task 16: Final verification & wrap-up

**Files:** none (verification only) + `.wolf` bookkeeping.

- [ ] **Step 1: Full suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 2: Race detector (short skips the binary-build proc tests)**

Run: `go test ./... -race -short`
Expected: PASS, race-clean.

- [ ] **Step 3: Vet + gofmt (CI gates on gofmt)**

Run:
```bash
go vet ./...
gofmt -l internal/ cmd/
```
Expected: `go vet` clean; `gofmt -l` prints **nothing** (any printed file must be `gofmt -w`'d, then re-commit).

- [ ] **Step 4: End-to-end binary smoke**

Run:
```bash
go build -o /tmp/recarto ./cmd/recarto
cd $(mktemp -d) && git init -q && git commit -q --allow-empty -m init
RECARTO_CACHE_DIR=$(mktemp -d) /tmp/recarto remember learning --body "smoke test learning"
RECARTO_CACHE_DIR=$(mktemp -d) /tmp/recarto compact --dry-run --force
/tmp/recarto compact --force
/tmp/recarto status
```
Expected: `compact` prints the report (dry-run prefixed), `status` shows the `compaction:` section with `epoch` ≥ 1.

> Use the SAME `RECARTO_CACHE_DIR` across the last three commands to exercise one store; the snippet above uses fresh dirs for illustration — export one dir and reuse it.

- [ ] **Step 5: Update `.wolf` bookkeeping and finish**

- Append a session summary line to `.wolf/memory.md`.
- Add the build outcome + the benchmark number to `.wolf/cerebrum.md` (Key Learnings: "Phase 3 compaction/decay BUILT; BenchmarkCompact = <Xms> over 50k → synchronous path confirmed / detached deferred").
- `.wolf/anatomy.md` auto-rescans; if not, add `internal/memory/compact.go` and the new test files.

This plan is complete. Each task is independently committable and leaves the suite green.
