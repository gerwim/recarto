# Recarto Phase 1A — Foundation & Store — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the foundation of Recarto — a Go 1.26.4 module with a concurrency-safe, pure-Go SQLite store living in a per-repo global cache, git identity resolution, migrations, atomic counters, and a proven multi-process concurrency test.

**Architecture:** One Go module (`git.gerwim.nl/gerwim/recarto`). Injected `clock` and git `Runner` interfaces keep logic deterministic. The store opens SQLite via `modernc.org/sqlite` (no CGO) in WAL mode with `busy_timeout`; all writes go through a `WriteTx` helper that issues `BEGIN IMMEDIATE` and is serialized in-process by a mutex, while reads use the pooled connection. Counters update atomically via UPSERT so concurrent writers never lose increments. Schema is versioned via `PRAGMA user_version` with guarded, idempotent migrations.

**Tech Stack:** Go 1.26.4, `modernc.org/sqlite` (pure-Go SQLite), `github.com/google/uuid` (UUIDv7), `github.com/spf13/cobra` (CLI), standard `database/sql`, `crypto/sha256`, `os/exec`.

**Spec:** `docs/superpowers/specs/2026-06-03-recarto-memory-spine-design.md` (§5 modules, §6 identity, §7 concurrency, §8 schema, §10 ledger).

---

## File Structure (Phase 1A)

| File | Responsibility |
|------|----------------|
| `go.mod`, `go.sum` | Module def; `go 1.26.4` (pins/min-requires 1.26.4 and auto-downloads it; see Task 1 note); deps. |
| `cmd/recarto/main.go` | Entry point; minimal cobra root with `version` + hidden `debug-stress` (expanded into full CLI in Phase 1C). |
| `internal/version/version.go` | `Version` string + `version_test.go`. |
| `internal/clock/clock.go` | `Clock` interface, `Real`, `Fake`. |
| `internal/ids/ids.go` | `New()` → time-ordered UUIDv7 string. |
| `internal/gitctx/gitctx.go` | `Runner` interface, `Real`, `Fake`; `Resolve()` → `Context{RepoID,RepoName,Branch,Worktree,RootCommit,NonGit}`. |
| `internal/cache/cache.go` | `StorePath(ctx)` → `<cacheDir>/<repo-id>/store.db`, honoring `$RECARTO_CACHE_DIR`. |
| `internal/store/store.go` | `Open`, `Close`, `WriteTx`, connection/PRAGMA setup. |
| `internal/store/migrate.go` | Ordered migrations + `user_version` guard; schema v1. |
| `internal/ledger/ledger.go` | Atomic counter `Incr`/`Get`. |
| `internal/store/concurrency_test.go` | Multi-process + multi-goroutine proof. |

---

## Task 1: Module scaffold + version

**Files:**
- Create: `go.mod`
- Create: `cmd/recarto/main.go`
- Create: `internal/version/version.go`
- Test: `internal/version/version_test.go`

- [ ] **Step 1: Create the module file**

`go.mod`:
```
module git.gerwim.nl/gerwim/recarto

go 1.26.4
```

> **Toolchain note:** Use the `go 1.26.4` directive **alone** — do NOT add a separate `toolchain go1.26.4` line. When the toolchain version equals the `go` directive it is redundant, and Go canonicalizes it away: `go mod tidy` removes it and `go build` then errors `updates to go.mod needed`. The `go 1.26.4` directive already requires ≥1.26.4 and, with the default `GOTOOLCHAIN=auto`, auto-downloads exactly 1.26.4 when the local toolchain is older. Verify with `go version` inside the module — it must print `go1.26.4`. (A separate `toolchain` line is only valid when it is *newer* than the `go` line, e.g. `go 1.26.0` + `toolchain go1.26.4`.)

- [ ] **Step 2: Write the failing test**

`internal/version/version_test.go`:
```go
package version

import "testing"

func TestVersionNotEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/version/`
Expected: FAIL — compile error `undefined: Version`.
(Note: the first `go` invocation may download the `go1.26.4` toolchain because of the `toolchain` directive and `GOTOOLCHAIN=auto`. If the network blocks it, install Go 1.26.4 manually and re-run.)

- [ ] **Step 4: Write minimal implementation**

`internal/version/version.go`:
```go
// Package version holds the build version of Recarto.
package version

// Version is the current Recarto version.
const Version = "0.1.0-dev"
```

- [ ] **Step 5: Create the entry point**

`cmd/recarto/main.go`:
```go
// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"fmt"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	// Full cobra CLI is wired in Task 8 / Phase 1C. For now, handle --version.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "recarto: no command (Phase 1A scaffold)")
}
```

- [ ] **Step 6: Run test + build to verify pass**

Run: `go test ./internal/version/ && go build ./...`
Expected: PASS, build succeeds.

- [ ] **Step 7: Commit**

```bash
go mod tidy
git add go.mod go.sum cmd/ internal/version/
git commit -m "feat: module scaffold with go 1.26.4 and version package"
```

---

## Task 2: Injected clock

**Files:**
- Create: `internal/clock/clock.go`
- Test: `internal/clock/clock_test.go`

- [ ] **Step 1: Write the failing test**

`internal/clock/clock_test.go`:
```go
package clock

import (
	"testing"
	"time"
)

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	c := NewFake(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}
	c.Advance(90 * time.Minute)
	want := start.Add(90 * time.Minute)
	if !c.Now().Equal(want) {
		t.Fatalf("after Advance Now() = %v, want %v", c.Now(), want)
	}
}

func TestRealClockMonotonicish(t *testing.T) {
	var c Clock = Real{}
	a := c.Now()
	b := c.Now()
	if b.Before(a) {
		t.Fatalf("real clock went backwards: %v then %v", a, b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/clock/`
Expected: FAIL — `undefined: NewFake`, `undefined: Clock`, `undefined: Real`.

- [ ] **Step 3: Write minimal implementation**

`internal/clock/clock.go`:
```go
// Package clock provides an injectable time source for deterministic tests.
package clock

import "time"

// Clock is a source of the current time.
type Clock interface {
	Now() time.Time
}

// Real is the production clock backed by time.Now.
type Real struct{}

// Now returns the current wall-clock time.
func (Real) Now() time.Time { return time.Now() }

// Fake is a controllable clock for tests.
type Fake struct{ t time.Time }

// NewFake returns a Fake clock fixed at t.
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

// Now returns the Fake clock's current time.
func (f *Fake) Now() time.Time { return f.t }

// Advance moves the Fake clock forward by d.
func (f *Fake) Advance(d time.Duration) { f.t = f.t.Add(d) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/clock/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/clock/
git commit -m "feat: injectable clock (Real + Fake)"
```

---

## Task 3: UUIDv7 ids

**Files:**
- Create: `internal/ids/ids.go`
- Test: `internal/ids/ids_test.go`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/google/uuid@latest`

- [ ] **Step 2: Write the failing test**

`internal/ids/ids_test.go`:
```go
package ids

import (
	"sort"
	"testing"
)

func TestNewFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != 36 {
			t.Fatalf("id %q length = %d, want 36", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = true
	}
}

func TestNewIsTimeOrdered(t *testing.T) {
	const n = 200
	got := make([]string, n)
	for i := range got {
		got[i] = New()
	}
	sorted := make([]string, n)
	copy(sorted, got)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("UUIDv7 not lexicographically time-ordered at %d", i)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/ids/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Write minimal implementation**

`internal/ids/ids.go`:
```go
// Package ids generates time-ordered unique identifiers.
package ids

import "github.com/google/uuid"

// New returns a time-ordered UUIDv7 as a 36-char string.
// UUIDv7 values sort lexicographically by creation time, which gives the
// SQLite primary-key index good locality and lets us order records by id.
func New() string {
	return uuid.Must(uuid.NewV7()).String()
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/ids/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/ids/
git commit -m "feat: UUIDv7 id generation"
```

---

## Task 4: Git identity resolution

**Files:**
- Create: `internal/gitctx/gitctx.go`
- Test: `internal/gitctx/gitctx_test.go`

The `Runner` interface lets tests supply canned git output. `Resolve` derives a stable repo id from the **common** git dir (shared by all worktrees), the current branch (with detached-HEAD fallback), the worktree root, and the root commit. Non-git directories fall back to a cwd hash.

- [ ] **Step 1: Write the failing test**

`internal/gitctx/gitctx_test.go`:
```go
package gitctx

import (
	"errors"
	"strings"
	"testing"
)

// fakeRunner maps the joined args to a result.
type fakeRunner struct {
	out map[string]string
	err map[string]error
}

func (f fakeRunner) Run(args ...string) (string, error) {
	k := strings.Join(args, " ")
	if e, ok := f.err[k]; ok {
		return "", e
	}
	if v, ok := f.out[k]; ok {
		return v, nil
	}
	return "", errors.New("unexpected git call: " + k)
}

func TestResolveNormalRepo(t *testing.T) {
	r := fakeRunner{out: map[string]string{
		"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
		"symbolic-ref --short -q HEAD":                       "feature/login",
		"rev-parse --show-toplevel":                          "/home/u/proj",
		"rev-list --max-parents=0 HEAD":                      "abc123def456",
	}}
	ctx, err := Resolve(r, "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.NonGit {
		t.Fatal("expected git repo")
	}
	if ctx.Branch != "feature/login" {
		t.Fatalf("branch = %q", ctx.Branch)
	}
	if ctx.Worktree != "/home/u/proj" {
		t.Fatalf("worktree = %q", ctx.Worktree)
	}
	if ctx.RepoName != "proj" {
		t.Fatalf("repo name = %q", ctx.RepoName)
	}
	if len(ctx.RepoID) != 16 {
		t.Fatalf("repo id = %q (len %d), want 16 hex chars", ctx.RepoID, len(ctx.RepoID))
	}
	if ctx.RootCommit != "abc123def456" {
		t.Fatalf("root commit = %q", ctx.RootCommit)
	}
}

func TestResolveSharedAcrossWorktrees(t *testing.T) {
	// Two worktrees of the same repo share the same --git-common-dir,
	// so they must resolve to the same RepoID.
	base := map[string]string{
		"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
		"rev-list --max-parents=0 HEAD":                      "abc123",
	}
	wt1 := fakeRunner{out: mergeMap(base, map[string]string{
		"symbolic-ref --short -q HEAD": "main",
		"rev-parse --show-toplevel":    "/home/u/proj",
	})}
	wt2 := fakeRunner{out: mergeMap(base, map[string]string{
		"symbolic-ref --short -q HEAD": "feature/x",
		"rev-parse --show-toplevel":    "/home/u/proj-wt-feature",
	})}
	c1, _ := Resolve(wt1, "/home/u/proj")
	c2, _ := Resolve(wt2, "/home/u/proj-wt-feature")
	if c1.RepoID != c2.RepoID {
		t.Fatalf("worktrees must share repo id: %q != %q", c1.RepoID, c2.RepoID)
	}
	if c1.Branch == c2.Branch {
		t.Fatal("branches should differ between worktrees")
	}
}

func TestResolveDetachedHead(t *testing.T) {
	r := fakeRunner{
		out: map[string]string{
			"rev-parse --path-format=absolute --git-common-dir": "/home/u/proj/.git",
			"rev-parse --show-toplevel":                          "/home/u/proj",
			"rev-list --max-parents=0 HEAD":                      "abc123",
			"rev-parse --short HEAD":                             "deadbee",
		},
		err: map[string]error{
			"symbolic-ref --short -q HEAD": errors.New("exit status 1"),
		},
	}
	ctx, err := Resolve(r, "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Branch != "detached@deadbee" {
		t.Fatalf("branch = %q, want detached@deadbee", ctx.Branch)
	}
}

func TestResolveNonGit(t *testing.T) {
	r := fakeRunner{err: map[string]error{
		"rev-parse --path-format=absolute --git-common-dir": errors.New("not a git repository"),
	}}
	ctx, err := Resolve(r, "/tmp/scratch")
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.NonGit {
		t.Fatal("expected NonGit=true")
	}
	if ctx.Branch != "(none)" {
		t.Fatalf("branch = %q, want (none)", ctx.Branch)
	}
	if len(ctx.RepoID) != 16 {
		t.Fatalf("repo id len = %d, want 16", len(ctx.RepoID))
	}
}

func mergeMap(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitctx/`
Expected: FAIL — `undefined: Resolve`, `undefined: Context`.

- [ ] **Step 3: Write minimal implementation**

`internal/gitctx/gitctx.go`:
```go
// Package gitctx resolves the git identity of the working directory:
// a stable per-repo id (shared across worktrees), branch, worktree, and root commit.
package gitctx

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
)

// Context describes the resolved repo/worktree/branch identity.
type Context struct {
	RepoID     string // 16 hex chars; stable across worktrees of the same repo
	RepoName   string // human-readable (basename of worktree root or cwd)
	Branch     string // current branch, "detached@<sha>", or "(none)"
	Worktree   string // absolute worktree root (distinct per worktree)
	RootCommit string // first root commit sha (empty if non-git)
	NonGit     bool   // true when not inside a git repo
}

// Runner runs `git <args...>` and returns trimmed stdout.
type Runner interface {
	Run(args ...string) (string, error)
}

// Real runs git via os/exec inside dir.
type Real struct{ Dir string }

// Run executes git in r.Dir and returns trimmed combined stdout.
func (r Real) Run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func repoIDFromCommonDir(commonDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(commonDir)))
	return hex.EncodeToString(sum[:])[:16]
}

// Resolve derives the identity for cwd using the given Runner.
func Resolve(r Runner, cwd string) (Context, error) {
	commonDir, err := r.Run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || commonDir == "" {
		// Non-git fallback: hash the absolute cwd.
		sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
		return Context{
			RepoID:   hex.EncodeToString(sum[:])[:16],
			RepoName: filepath.Base(cwd),
			Branch:   "(none)",
			Worktree: cwd,
			NonGit:   true,
		}, nil
	}

	ctx := Context{RepoID: repoIDFromCommonDir(commonDir)}

	if top, err := r.Run("rev-parse", "--show-toplevel"); err == nil && top != "" {
		ctx.Worktree = top
		ctx.RepoName = filepath.Base(top)
	} else {
		ctx.Worktree = cwd
		ctx.RepoName = filepath.Base(cwd)
	}

	branch, err := r.Run("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || branch == "" {
		sha, _ := r.Run("rev-parse", "--short", "HEAD")
		if sha == "" {
			sha = "unknown"
		}
		ctx.Branch = "detached@" + sha
	} else {
		ctx.Branch = branch
	}

	if root, err := r.Run("rev-list", "--max-parents=0", "HEAD"); err == nil {
		// A repo can have multiple root commits; take the first line.
		if i := strings.IndexByte(root, '\n'); i >= 0 {
			root = root[:i]
		}
		ctx.RootCommit = strings.TrimSpace(root)
	}

	return ctx, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gitctx/`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/gitctx/
git commit -m "feat: git identity resolution (repo-id shared across worktrees)"
```

---

## Task 5: Cache path resolution

**Files:**
- Create: `internal/cache/cache.go`
- Test: `internal/cache/cache_test.go`

- [ ] **Step 1: Write the failing test**

`internal/cache/cache_test.go`:
```go
package cache

import (
	"path/filepath"
	"testing"
)

func TestStorePathHonorsEnvOverride(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", "/custom/cache")
	got, err := StorePath("abc123def456789a")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/cache", "abc123def456789a", "store.db")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestStorePathDefaultUsesOSCache(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", "")
	got, err := StorePath("repo01")
	if err != nil {
		t.Fatal(err)
	}
	// Must end with recarto/<repo-id>/store.db regardless of OS cache root.
	wantSuffix := filepath.Join("recarto", "repo01", "store.db")
	if filepath.Base(got) != "store.db" || !hasSuffix(got, wantSuffix) {
		t.Fatalf("StorePath = %q, want suffix %q", got, wantSuffix)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cache/`
Expected: FAIL — `undefined: StorePath`.

- [ ] **Step 3: Write minimal implementation**

`internal/cache/cache.go`:
```go
// Package cache resolves where Recarto stores its per-repo SQLite database:
// outside the working tree, shared across all worktrees of a repo.
package cache

import (
	"os"
	"path/filepath"
)

// Dir returns the Recarto cache root: $RECARTO_CACHE_DIR, else <os-cache>/recarto.
func Dir() (string, error) {
	if d := os.Getenv("RECARTO_CACHE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recarto"), nil
}

// StorePath returns the absolute path to the store.db for the given repo id.
func StorePath(repoID string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, repoID, "store.db"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cache/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cache/
git commit -m "feat: per-repo cache path resolution with env override"
```

---

## Task 6: Store open + PRAGMAs

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

Opens SQLite via `modernc.org/sqlite`. PRAGMAs are passed in the DSN so every pooled connection gets `busy_timeout`, WAL, `synchronous=NORMAL`, and `foreign_keys=ON`. `WriteTx` serializes writers in-process and issues `BEGIN IMMEDIATE`. (Migrations are added in Task 7; for now `Open` just opens + applies PRAGMAs.)

- [ ] **Step 1: Add the dependency**

Run: `go get modernc.org/sqlite@latest`

- [ ] **Step 2: Write the failing test**

`internal/store/store_test.go`:
```go
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAppliesPragmas(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var journal string
	if err := st.DB.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := st.DB.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy < 5000 {
		t.Fatalf("busy_timeout = %d, want >= 5000", busy)
	}
}

func TestWriteTxCommitsAndRollsBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx, `CREATE TABLE t (n INTEGER)`); err != nil {
		t.Fatal(err)
	}

	// Commit path.
	if err := st.WriteTx(ctx, func(c Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO t(n) VALUES (1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Rollback path: returning an error must roll back.
	wantErr := context.Canceled
	_ = st.WriteTx(ctx, func(c Conn) error {
		_, _ = c.ExecContext(ctx, `INSERT INTO t(n) VALUES (2)`)
		return wantErr
	})

	var count int
	if err := st.DB.QueryRow(`SELECT count(*) FROM t`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (rollback failed)", count)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `undefined: Open`, `undefined: Conn`.

- [ ] **Step 4: Write minimal implementation**

`internal/store/store.go`:
```go
// Package store opens and manages Recarto's concurrency-safe SQLite database.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (driver name: "sqlite")
)

// Conn is the subset of *sql.Conn that WriteTx callbacks use.
type Conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store wraps the SQLite connection pool plus an in-process write mutex.
type Store struct {
	DB   *sql.DB
	wmu  sync.Mutex
	path string
}

// Open creates (if needed) and opens the store at path, applies PRAGMAs on
// every connection, and runs migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL allows many readers + one writer across processes; in-process we
	// additionally serialize writers via wmu, so a small pool is fine.
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, path: path}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying pool.
func (s *Store) Close() error { return s.DB.Close() }

// WriteTx runs fn inside a serialized `BEGIN IMMEDIATE` transaction. The
// in-process mutex plus IMMEDIATE + busy_timeout guarantee that concurrent
// writers (this process and others) never corrupt state or lose updates.
func (s *Store) WriteTx(ctx context.Context, fn func(c Conn) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}
```

> Note: `*sql.Conn` satisfies the `Conn` interface (it has `ExecContext`/`QueryContext`/`QueryRowContext`), so `fn` receives the live connection bound to the open transaction.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/store/store.go internal/store/store_test.go
git commit -m "feat: concurrency-safe SQLite store (WAL, busy_timeout, IMMEDIATE WriteTx)"
```

---

## Task 7: Schema migrations (v1)

**Files:**
- Create: `internal/store/migrate.go`
- Test: `internal/store/migrate_test.go`

Migrations are ordered SQL strings applied inside a guarded `BEGIN IMMEDIATE` block keyed on `PRAGMA user_version`, so concurrent first-run processes serialize and apply each migration exactly once.

- [ ] **Step 1: Write the failing test**

`internal/store/migrate_test.go`:
```go
package store

import (
	"path/filepath"
	"testing"
)

func TestMigrateCreatesTables(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	want := []string{"meta", "memories", "memory_refs", "memory_minhash", "counters", "sessions"}
	for _, tbl := range want {
		var name string
		err := st.DB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", tbl, err)
		}
	}

	var v int
	if err := st.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st1, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	st1.Close()
	// Re-open: must not error or re-run migrations.
	st2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer st2.Close()
	var v int
	if err := st2.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version after reopen = %d, want %d", v, schemaVersion)
	}
}

func TestMemoriesKindConstraint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.DB.Exec(
		`INSERT INTO memories(id,kind,created_at,updated_at) VALUES('x','bogus',1,1)`)
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid kind")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestMigrate`
Expected: FAIL — `undefined: schemaVersion` / tables missing.

- [ ] **Step 3: Write minimal implementation**

`internal/store/migrate.go`:
```go
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the number of applied migrations.
const schemaVersion = 1

// migrations are applied in order; index+1 becomes user_version.
var migrations = []string{
	// v1: core schema (no FTS yet — FTS5 is added in Phase 1B).
	`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE memories (
  id                  TEXT PRIMARY KEY,
  kind                TEXT NOT NULL CHECK (kind IN
                        ('preference','learning','decision','do_not_repeat','bug','action')),
  title               TEXT NOT NULL DEFAULT '',
  body                TEXT NOT NULL DEFAULT '',
  search_text         TEXT NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  branch              TEXT,
  worktree            TEXT,
  author              TEXT NOT NULL DEFAULT '',
  source              TEXT NOT NULL DEFAULT '',
  confidence          REAL NOT NULL DEFAULT 1.0,
  reinforcement_count INTEGER NOT NULL DEFAULT 0,
  pinned              INTEGER NOT NULL DEFAULT 0,
  status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN
                        ('active','superseded','archived')),
  superseded_by       TEXT,
  simhash             INTEGER NOT NULL DEFAULT 0,
  extra               TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_memories_kind   ON memories(kind);
CREATE INDEX idx_memories_status ON memories(status);
CREATE INDEX idx_memories_branch ON memories(branch);

CREATE TABLE memory_refs (
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  ref_type  TEXT NOT NULL CHECK (ref_type IN ('file','symbol','tag')),
  ref_value TEXT NOT NULL,
  PRIMARY KEY (memory_id, ref_type, ref_value)
);
CREATE INDEX idx_memory_refs_lookup ON memory_refs(ref_type, ref_value);

CREATE TABLE memory_minhash (
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  band      INTEGER NOT NULL,
  hash      INTEGER NOT NULL,
  PRIMARY KEY (memory_id, band)
);
CREATE INDEX idx_memory_minhash_lookup ON memory_minhash(band, hash);

CREATE TABLE counters (
  scope  TEXT NOT NULL,
  metric TEXT NOT NULL,
  value  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (scope, metric)
);

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  agent      TEXT NOT NULL DEFAULT '',
  branch     TEXT,
  worktree   TEXT,
  started_at INTEGER NOT NULL,
  ended_at   INTEGER
);
`,
}

// Migrate applies pending migrations under an IMMEDIATE lock so that
// concurrent processes opening a fresh DB cannot double-apply.
func Migrate(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := func(e error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return e
	}

	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return rollback(err)
	}
	for i := current; i < len(migrations); i++ {
		if _, err := conn.ExecContext(ctx, migrations[i]); err != nil {
			return rollback(fmt.Errorf("migration %d: %w", i+1, err))
		}
	}
	// PRAGMA user_version does not accept bound params; format the literal int.
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS (all store tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat: schema v1 with guarded idempotent migrations"
```

---

## Task 8: Atomic ledger counters + debug-stress command

**Files:**
- Create: `internal/ledger/ledger.go`
- Test: `internal/ledger/ledger_test.go`
- Modify: `cmd/recarto/main.go` (add hidden `debug-stress` subcommand)

- [ ] **Step 1: Write the failing test**

`internal/ledger/ledger_test.go`:
```go
package ledger

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIncrAndGet(t *testing.T) {
	st := newStore(t)
	l := New(st)
	ctx := context.Background()

	if err := l.Incr(ctx, "lifetime", "reads", 3); err != nil {
		t.Fatal(err)
	}
	if err := l.Incr(ctx, "lifetime", "reads", 2); err != nil {
		t.Fatal(err)
	}
	got, err := l.Get(ctx, "lifetime", "reads")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("reads = %d, want 5", got)
	}
}

func TestGetMissingIsZero(t *testing.T) {
	st := newStore(t)
	l := New(st)
	got, err := l.Get(context.Background(), "lifetime", "never")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("missing counter = %d, want 0", got)
	}
}

func TestIncrConcurrentInProcess(t *testing.T) {
	st := newStore(t)
	l := New(st)
	ctx := context.Background()
	const g, n = 8, 250
	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
					t.Errorf("incr: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	got, err := l.Get(ctx, "lifetime", "stress")
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(g*n) {
		t.Fatalf("stress = %d, want %d (lost in-process updates)", got, g*n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ledger/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

`internal/ledger/ledger.go`:
```go
// Package ledger maintains atomic token/usage counters in the store.
package ledger

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Ledger updates counters atomically, safe across goroutines and processes.
type Ledger struct {
	st *store.Store
}

// New returns a Ledger backed by st.
func New(st *store.Store) *Ledger { return &Ledger{st: st} }

// Incr atomically adds n to counters[scope, metric], inserting if absent.
func (l *Ledger) Incr(ctx context.Context, scope, metric string, n int64) error {
	return l.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO counters(scope, metric, value) VALUES (?, ?, ?)
			 ON CONFLICT(scope, metric) DO UPDATE SET value = value + excluded.value`,
			scope, metric, n)
		return err
	})
}

// Get returns the counter value, or 0 if it does not exist.
func (l *Ledger) Get(ctx context.Context, scope, metric string) (int64, error) {
	var v int64
	err := l.st.DB.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE scope=? AND metric=?`, scope, metric).Scan(&v)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
}
```

> Note: prefer `errors.Is(err, sql.ErrNoRows)` over string-matching. Use:
> ```go
> import (
> 	"database/sql"
> 	"errors"
> )
> // ...
> if errors.Is(err, sql.ErrNoRows) {
> 	return 0, nil
> }
> ```
> Write it that way in the implementation; the string form above is only shown to keep the snippet self-contained.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ledger/`
Expected: PASS.

- [ ] **Step 5: Add the hidden `debug-stress` command (for the multi-process test)**

Replace `cmd/recarto/main.go` with:
```go
// Command recarto is the Recarto CLI / MCP server / hook entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "recarto: no command (Phase 1A scaffold)")
		os.Exit(0)
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
		fmt.Fprintln(os.Stderr, "recarto: unknown command", os.Args[1])
		os.Exit(2)
	}
}

// runDebugStress performs --iter atomic increments to counters[lifetime,stress].
// It exists to drive the cross-process concurrency test and is not a public command.
func runDebugStress(args []string) error {
	fs := flag.NewFlagSet("debug-stress", flag.ContinueOnError)
	dbPath := fs.String("db", "", "path to store.db")
	iter := fs.Int("iter", 100, "increments to perform")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("--db is required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	l := ledger.New(st)
	ctx := context.Background()
	for i := 0; i < *iter; i++ {
		if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 6: Run + build to verify**

Run: `go build ./... && go test ./internal/ledger/`
Expected: build OK, tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/ledger/ cmd/recarto/main.go
git commit -m "feat: atomic ledger counters + hidden debug-stress command"
```

---

## Task 9: Multi-process concurrency proof (the headline requirement)

**Files:**
- Create: `internal/store/concurrency_test.go`

This test proves the worktree requirement: many processes *and* goroutines hammering one shared DB lose no updates and never corrupt it. It builds the real binary and launches it as separate OS processes (simulating hooks firing from different worktrees) while goroutines write concurrently in-process.

- [ ] **Step 1: Write the test**

`internal/store/concurrency_test.go`:
```go
package store_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// buildBinary compiles the recarto binary into a temp dir and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "recarto")
	cmd := exec.Command("go", "build", "-o", bin, "git.gerwim.nl/gerwim/recarto/cmd/recarto")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestConcurrentWritersAcrossProcessesAndGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-process concurrency test in -short mode")
	}
	dbPath := filepath.Join(t.TempDir(), "store.db")

	// Initialize schema once so all writers share a migrated DB.
	st0, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st0.Close()

	bin := buildBinary(t)

	const (
		procs      = 4
		goroutines = 4
		iter       = 200
	)
	var wg sync.WaitGroup

	// External processes (simulate hooks from different worktrees).
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(bin, "debug-stress", "--db", dbPath, "--iter", strconv.Itoa(iter))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("subprocess failed: %v\n%s", err, out)
			}
		}()
	}

	// In-process goroutines on a second handle to the same DB.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := ledger.New(st)
	ctx := context.Background()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iter; j++ {
				if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
					t.Errorf("incr: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	got, err := l.Get(ctx, "lifetime", "stress")
	if err != nil {
		t.Fatal(err)
	}
	want := int64((procs + goroutines) * iter)
	if got != want {
		t.Fatalf("stress counter = %d, want %d (lost updates under concurrency)", got, want)
	}

	var integrity string
	if err := st.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
}
```

- [ ] **Step 2: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestConcurrentWriters -v`
Expected: PASS. The counter equals `(4+4)*200 = 1600` and `integrity_check = ok`. (If it fails with a lower count, a write path lost updates — revisit WriteTx/Incr. If `SQLITE_BUSY` surfaces, raise `busy_timeout` or confirm `BEGIN IMMEDIATE`.)

- [ ] **Step 3: Run the full suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 4: Commit**

```bash
git add internal/store/concurrency_test.go
git commit -m "test: prove multi-process + multi-goroutine concurrency safety"
```

---

## Task 10: CI workflow (cross-OS concurrency proof)

**Files:**
- Create: `.github/workflows/ci.yml`

Runs the suite — including the concurrency proof — on Linux, macOS, and Windows, the spec's §7 requirement.

- [ ] **Step 1: Create the workflow**

`.github/workflows/ci.yml`:
```yaml
name: CI
on:
  push:
    branches: [master, "phase-*"]
  pull_request:
jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26.4'
      - run: go build ./...
      - run: go vet ./...
      - run: go test ./... -v
```

- [ ] **Step 2: Verify locally that vet + tests pass**

Run: `go vet ./... && go test ./...`
Expected: PASS (CI mirrors this across three OSes).

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: build/vet/test on linux, macos, windows"
```

---

## Phase 1A Self-Review

**Spec coverage (against the design spec):**
- §5 module layout → Tasks 2–8 create `clock`, `gitctx`, `cache`, `store`, `ledger`, `version`, `cmd/recarto`. (`memory`, `mcp`, `agents`, `export`, `cli` are Phases 1B/1C — out of scope here, by design.)
- §6 identity (repo-id from `--git-common-dir`, branch + detached fallback, worktree, root commit, non-git fallback) → Task 4, all paths tested.
- §7 concurrency (WAL, busy_timeout, BEGIN IMMEDIATE, atomic counters, guarded migrations, multi-process proof, cross-OS CI) → Tasks 6, 7, 8, 9, 10.
- §8 schema v1 (memories, memory_refs, memory_minhash, counters, sessions, meta; FTS deferred to 1B) → Task 7.
- §10 ledger atomic counters → Task 8 (full metric set + estimator land in 1B).
- D1 Go 1.26.4 + toolchain → Task 1; D3 pure-Go modernc.org/sqlite → Task 6; D4 cache location → Task 5.

**Placeholder scan:** none — every step has runnable code/commands and expected output. The `sql.ErrNoRows` note in Task 8 explicitly tells the engineer the preferred form.

**Type consistency:** `store.Conn` interface used identically in `store.WriteTx` (Task 6), `ledger.Incr` (Task 8), and callbacks. `gitctx.Context` fields referenced in tests match the struct. `schemaVersion`/`migrations` defined in Task 7 and asserted in its tests. Module path `git.gerwim.nl/gerwim/recarto` consistent across all imports.

**Carried into 1B:** FTS5 virtual table (migration v2) + the day-1 FTS5 availability spike (spec §15); full ledger metric set + token estimator; memory model/DAO that writes `memory_refs` and `memory_minhash`.
