# Recarto Phase 1B — Memory Engine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Every task is RED→GREEN→REFACTOR (TDD is non-negotiable on this project).

**Goal:** Build `internal/memory` — the write+dedup and token-budgeted lexical+structural recall engine — plus the FTS5 lexical backend and the token estimator, so memories can be recorded (reinforcing near-duplicates) and recalled by relevance, all below the not-yet-built MCP/CLI surfaces (Phase 1C).

**Architecture:** A new `internal/memory` package exposes an `Engine` over the existing concurrency-safe `internal/store`. `Engine.Record` computes an identifier-split + synonym-expanded `search_text`, a SimHash, and a MinHash signature; it uses MinHash-LSH banding to find near-duplicate candidates and confirms with exact Jaccard on word-shingles, reinforcing an existing same-kind row at Jaccard ≥ 0.9 instead of inserting. `Engine.Recall` scores candidates by structural ref-overlap (highest weight), BM25 lexical match, recency, reinforcement, and branch affinity, then greedily fills a token budget after always pinning `do_not_repeat`. The lexical backend sits behind a swappable `Lexical` interface (FTS5 by default; a Go inverted index is the documented fallback — see Risk Gate).

**Tech Stack:** Go 1.26.4, pure-Go `modernc.org/sqlite` (FTS5), stdlib `hash/fnv` for MinHash/SimHash, the existing `internal/{store,ledger,ids,clock}` packages. Module path `git.gerwim.nl/gerwim/recarto`. No new third-party dependencies.

---

## Background: what already exists (Phase 1A, on `master`)

These are built and tested — **use them, do not reimplement**:

- **`internal/store`** — `store.Open(path) (*Store, error)`; `st.DB *sql.DB` (read pool); `st.WriteTx(ctx, func(c store.Conn) error) error` (serialized `BEGIN IMMEDIATE` writer); `store.Conn` interface (`ExecContext`/`QueryContext`/`QueryRowContext`). Migrations live in `internal/store/migrate.go` (`migrations []string`, `schemaVersion`, `Migrate(db)`). Schema v1 already created: `meta`, `memories`, `memory_refs`, `memory_minhash` (PK `(memory_id, band)`), `counters`, `sessions`. **`memories_fts` does NOT exist yet — this plan adds it (Task 2).**
- **`internal/ledger`** — `ledger.New(st) *Ledger`; `l.Incr(ctx, scope, metric string, n int64) error` (atomic UPSERT); `l.Get(ctx, scope, metric) (int64, error)`. **No token estimator yet — this plan adds it (Task 3).**
- **`internal/ids`** — `ids.New() string` (UUIDv7, time-ordered).
- **`internal/clock`** — `clock.Clock` interface (`Now() time.Time`); `clock.Real{}`; `clock.NewFake(t) *Fake` with `.Advance(d)`.

The `memories` columns 1B writes to (from migrate.go): `id, kind, title, body, search_text, created_at, updated_at, branch, worktree, author, source, confidence, reinforcement_count, pinned, status, superseded_by, simhash, extra`. `kind` is CHECK-constrained to `('preference','learning','decision','do_not_repeat','bug','action')`; `status` to `('active','superseded','archived')`.

**Test-setup pattern (copy it):**
```go
p := filepath.Join(t.TempDir(), "store.db")
st, err := store.Open(p)
if err != nil { t.Fatal(err) }
defer st.Close()
```

**Running tests (use the module toolchain — ignore IDE gopls "undefined" false positives):**
```bash
go test ./...                       # whole suite
go test ./internal/memory/ -run TestName -v
go test -race ./internal/memory/    # race check
```

**Branch note for the executor:** Phase 1A landed on `master`; there is no `phase-1-memory-spine` branch. Do all 1B work on a fresh feature branch (e.g. `phase-1b-memory-engine`) — never commit 1B directly to `master`.

## Risk Gate (do Task 1 first)

§15 of the spec flags **`modernc.org/sqlite` FTS5 support as the primary risk.** Task 1 is an empirical spike that proves FTS5 + `bm25()` work in this environment. The whole plan's primary path assumes it passes (modernc ships FTS5 enabled).

**If Task 1 FAILS** (FTS5 unavailable or `bm25()` errors): STOP and report. The fallback (designed-for, swappable) is to keep the `Lexical` interface (Task 8) but replace `ftsLexical` with an `invertedLexical` backed by a `terms(memory_id, term, tf)` table plus a Go BM25 over the pre-split tokens, and skip Task 2's FTS migration in favor of a `terms` table. The interface boundary (Task 8) is deliberately drawn so this swap touches only `lexical.go` + the Task 2 migration. Do not attempt the fallback without checking in.

## File Structure

| File | Responsibility | Tasks |
|------|----------------|-------|
| `internal/store/fts_test.go` | FTS5 availability + bm25 ranking spike (regression test) | T1 |
| `internal/store/migrate.go` (modify) | Add migration v2: `memories_fts` FTS5 vtable | T2 |
| `internal/ledger/estimate.go` + `_test.go` | `EstimateTokens` heuristic (pure, golden) | T3 |
| `internal/memory/record.go` + `_test.go` | `Kind`, `RefType`, `Ref`, `NewMemory`, `Record` types | T4 |
| `internal/memory/tokenize.go` + `_test.go` | identifier split + tokenization | T5 |
| `internal/memory/synonyms.go`, `synonyms.txt` + `_test.go` | curated synonym expansion + `BuildSearchText` | T6 |
| `internal/memory/fingerprint.go` + `_test.go` | shingles, Jaccard, MinHash, LSH bands, SimHash | T7 |
| `internal/memory/lexical.go` + `_test.go` | `Lexical` interface + `ftsLexical` (Index/Delete/Search) | T8 |
| `internal/memory/engine.go` + `_test.go` | `Engine`, low-level `insert`, `GetByID`, scan helpers | T9 |
| `internal/memory/engine.go` (extend) | `findNearDup`, `Record` (reinforce-or-insert) | T10 |
| `internal/memory/recall.go` + `_test.go` | scoring, structural/pinned/load helpers, `Recall` | T11 |
| `internal/memory/recall.go` (extend) | `selectWithinBudget` + ledger counters | T12 |

---

### Task 1: FTS5 availability spike (risk gate)

**Files:**
- Create: `internal/store/fts_test.go`

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestFTS5IsAvailable is the §15 risk-gate spike: it proves modernc.org/sqlite
// ships FTS5 with a working bm25() ranking function. If this fails, switch the
// lexical backend to the documented Go inverted-index fallback (see plan).
func TestFTS5IsAvailable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fts.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.DB.ExecContext(ctx,
		`CREATE VIRTUAL TABLE t USING fts5(body)`); err != nil {
		t.Fatalf("FTS5 unavailable: %v", err)
	}
	docs := []string{"the quick brown fox", "lazy dog sleeps", "quick quick quick fox"}
	for _, d := range docs {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO t(body) VALUES (?)`, d); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB.QueryContext(ctx,
		`SELECT body, bm25(t) FROM t WHERE t MATCH 'quick' ORDER BY bm25(t) ASC`)
	if err != nil {
		t.Fatalf("bm25/MATCH failed: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows matched 'quick'")
	}
	var top string
	var score float64
	if err := rows.Scan(&top, &score); err != nil {
		t.Fatal(err)
	}
	// The doc with the most/least-diluted 'quick' must rank first.
	if !strings.Contains(top, "quick quick quick") {
		t.Fatalf("bm25 ranking wrong: top=%q score=%v", top, score)
	}
}
```

- [ ] **Step 2: Run test to verify it passes (this is a gate, not a RED step)**

Run: `go test ./internal/store/ -run TestFTS5IsAvailable -v`
Expected: PASS. **If it FAILS, STOP and report — do not continue to Task 2.**

- [ ] **Step 3: Commit**

```bash
git add internal/store/fts_test.go
git commit -m "test: prove FTS5 + bm25 available in modernc.org/sqlite"
```

---

### Task 2: Migration v2 — `memories_fts`

**Files:**
- Modify: `internal/store/migrate.go`
- Modify/Create: `internal/store/migrate_test.go` (add a test)

- [ ] **Step 1: Write the failing test** (append to `internal/store/migrate_test.go`)

```go
func TestMigrateCreatesFTS(t *testing.T) {
	p := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var name string
	err = st.DB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memories_fts'`).Scan(&name)
	if err != nil {
		t.Fatalf("memories_fts not created: %v", err)
	}

	var uv int
	if err := st.DB.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != 2 {
		t.Fatalf("user_version = %d, want 2", uv)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestMigrateCreatesFTS -v`
Expected: FAIL — `memories_fts not created` and `user_version = 1, want 2`.

- [ ] **Step 3: Add the v2 migration** — in `internal/store/migrate.go`, change `schemaVersion` to `2` and append a second element to `migrations`:

```go
// schemaVersion is the number of applied migrations.
const schemaVersion = 2
```

Append after the v1 string (keep the trailing comma on v1):

```go
	// v2: FTS5 full-text index over search_text (Phase 1B). memory_id is
	// UNINDEXED so it is stored and returned but not tokenized. Rows are synced
	// manually from the memory write path — no external-content linkage, because
	// memories.id is a TEXT UUID, not the INTEGER rowid FTS5 external content
	// requires.
	`
CREATE VIRTUAL TABLE memories_fts USING fts5(
  memory_id UNINDEXED,
  search_text
);
`,
```

(`Migrate` already loops `for i := current; i < len(migrations)` and sets `user_version = len(migrations)`, so no other change is needed.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestMigrate' -v`
Expected: PASS (including the existing `TestMigrateIsIdempotent`, `TestMigrateCreatesTables`).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrate.go internal/store/migrate_test.go
git commit -m "feat: schema v2 adds memories_fts FTS5 index"
```

---

### Task 3: Token estimator (`internal/ledger`)

**Files:**
- Create: `internal/ledger/estimate.go`
- Create: `internal/ledger/estimate_test.go`

- [ ] **Step 1: Write the failing test**

```go
package ledger

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},                          // < 1 token rounds up to 1
		{"hello world", 3},                // 11 chars / 3.8 ≈ 2.9 → 3
		{"abcdefghij", 3},                 // 10 / 3.8 ≈ 2.6 → 3
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ledger/ -run TestEstimateTokens -v`
Expected: FAIL — `undefined: EstimateTokens`.

- [ ] **Step 3: Write the implementation** (`internal/ledger/estimate.go`)

```go
package ledger

// charsPerToken is the heuristic ratio for estimating model tokens from
// character length (~3.8 ≈ OpenWolf parity, good to ~15%).
const charsPerToken = 3.8

// EstimateTokens approximates the number of model tokens in s. It is a pure
// function so callers can budget recall output without a tokenizer dependency.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := float64(len(s)) / charsPerToken
	if n < 1 {
		return 1
	}
	return int(n + 0.5)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ledger/ -run TestEstimateTokens -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ledger/estimate.go internal/ledger/estimate_test.go
git commit -m "feat: pure token-count estimator"
```

---

### Task 4: Memory record types

**Files:**
- Create: `internal/memory/record.go`
- Create: `internal/memory/record_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import "testing"

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindPreference, KindLearning, KindDecision,
		KindDoNotRepeat, KindBug, KindAction} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if Kind("nonsense").Valid() {
		t.Error(`"nonsense" should be invalid`)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestKindValid -v`
Expected: FAIL — package `memory` does not exist / `undefined: Kind`.

- [ ] **Step 3: Write the implementation** (`internal/memory/record.go`)

```go
// Package memory is Recarto's write+dedup and recall engine over the store.
package memory

// Kind enumerates the unified memory kinds (mirrors the memories.kind CHECK).
type Kind string

const (
	KindPreference  Kind = "preference"
	KindLearning    Kind = "learning"
	KindDecision    Kind = "decision"
	KindDoNotRepeat Kind = "do_not_repeat"
	KindBug         Kind = "bug"
	KindAction      Kind = "action"
)

var validKinds = map[Kind]bool{
	KindPreference: true, KindLearning: true, KindDecision: true,
	KindDoNotRepeat: true, KindBug: true, KindAction: true,
}

// Valid reports whether k is a known memory kind.
func (k Kind) Valid() bool { return validKinds[k] }

// RefType enumerates structural reference types (mirrors memory_refs.ref_type).
type RefType string

const (
	RefFile   RefType = "file"
	RefSymbol RefType = "symbol"
	RefTag    RefType = "tag"
)

// Ref is a structural reference attached to a memory.
type Ref struct {
	Type  RefType
	Value string
}

// NewMemory is the input to Engine.Record.
type NewMemory struct {
	Kind         Kind
	Title        string
	Body         string
	Refs         []Ref
	Branch       string // "" → repo-global (stored as NULL)
	Worktree     string
	Author       string
	Source       string // "mcp" | "hook" | "cli"
	Pinned       bool
	SupersedesID string         // optional: a prior memory this one supersedes
	Extra        map[string]any // kind-specific fields (bug: error_message, ...)
}

// Record is a stored memory row plus its refs.
type Record struct {
	ID                 string
	Kind               Kind
	Title              string
	Body               string
	SearchText         string
	CreatedAt          int64 // unix ms
	UpdatedAt          int64 // unix ms
	Branch             string // "" if NULL
	Worktree           string
	Author             string
	Source             string
	Confidence         float64
	ReinforcementCount int
	Pinned             bool
	Status             string
	SupersededBy       string
	SimHash            uint64
	Extra              map[string]any
	Refs               []Ref
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestKindValid -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/record.go internal/memory/record_test.go
git commit -m "feat: memory record types (Kind/Ref/NewMemory/Record)"
```

---

### Task 5: Identifier split + tokenization

**Files:**
- Create: `internal/memory/tokenize.go`
- Create: `internal/memory/tokenize_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"strings"
	"testing"
)

func hasAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	got := tokenize("getUserConfig")
	if !hasAll(got, "getuserconfig", "get", "user", "config") {
		t.Errorf("camelCase split missing parts: %v", got)
	}

	got = tokenize("auth_token-refresh.go")
	if !hasAll(got, "auth", "token", "refresh", "go") {
		t.Errorf("separator split missing parts: %v", got)
	}

	got = tokenize("HTTPServer")
	if !hasAll(got, "http", "server") {
		t.Errorf("acronym split missing parts: %v", got)
	}

	// Deduped + lowercased.
	got = tokenize("Foo foo FOO")
	if strings.Count(strings.Join(got, " "), "foo") != 1 {
		t.Errorf("expected deduped lowercase tokens, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestTokenizeSplitsIdentifiers -v`
Expected: FAIL — `undefined: tokenize`.

- [ ] **Step 3: Write the implementation** (`internal/memory/tokenize.go`)

```go
package memory

import (
	"strings"
	"unicode"
)

// splitCamel splits camelCase / PascalCase and acronym boundaries
// ("HTTPServer" → "HTTP","Server"; "getUser" → "get","User").
func splitCamel(s string) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) &&
			i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			boundary = true // end of an acronym run: HTTPServer → HTTP|Server
		case unicode.IsLetter(prev) != unicode.IsLetter(cur):
			boundary = true // letter↔digit boundary
		}
		if boundary {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	return append(words, string(runes[start:]))
}

// splitIdentifier breaks one token on separators, then on camel/acronym
// boundaries. It returns only the component words (not the original token).
func splitIdentifier(tok string) []string {
	fields := strings.FieldsFunc(tok, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r)
	})
	var out []string
	for _, f := range fields {
		out = append(out, splitCamel(f)...)
	}
	return out
}

// tokenize lowercases text and returns a deduplicated token list containing
// both the original tokens and their identifier-split components.
func tokenize(text string) []string {
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			r != '_' && r != '-' && r != '.' && r != '/'
	})
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, tok := range raw {
		add(tok)
		for _, part := range splitIdentifier(tok) {
			add(part)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestTokenizeSplitsIdentifiers -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/tokenize.go internal/memory/tokenize_test.go
git commit -m "feat: identifier-split tokenizer (camel/snake/kebab/dotted)"
```

---

### Task 6: Synonym expansion + `BuildSearchText`

**Files:**
- Create: `internal/memory/synonyms.txt`
- Create: `internal/memory/synonyms.go`
- Create: `internal/memory/synonyms_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"strings"
	"testing"
)

func TestExpandSynonymsBidirectional(t *testing.T) {
	got := strings.Join(expandSynonyms([]string{"cfg"}), " ")
	if !strings.Contains(got, "config") {
		t.Errorf("cfg should expand to config: %q", got)
	}
	got = strings.Join(expandSynonyms([]string{"config"}), " ")
	if !strings.Contains(got, "cfg") {
		t.Errorf("config should expand to cfg (bidirectional): %q", got)
	}
}

func TestBuildSearchTextExpandsAndSplits(t *testing.T) {
	st := BuildSearchText("Fix authCfg bug", "the db connection", []Ref{
		{Type: RefFile, Value: "internal/auth/session.go"},
	})
	for _, want := range []string{"auth", "cfg", "config", "database", "session"} {
		if !strings.Contains(st, want) {
			t.Errorf("search_text missing %q: %q", want, st)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestExpandSynonyms|TestBuildSearchText' -v`
Expected: FAIL — `undefined: expandSynonyms` / `undefined: BuildSearchText`.

- [ ] **Step 3a: Create the embedded synonym data** (`internal/memory/synonyms.txt`)

```
# Curated abbreviation / synonym pairs, "a=b", expanded bidirectionally.
# Scope is DELIBERATELY narrow: known abbreviations + a hand-picked set only,
# NOT open-ended domain paraphrase (that is left to the structural signal).
cfg=config
config=configuration
auth=authentication
authn=authentication
authz=authorization
db=database
repo=repository
fs=filesystem
ctx=context
tx=transaction
id=identifier
msg=message
err=error
btn=button
impl=implementation
```

- [ ] **Step 3b: Write the implementation** (`internal/memory/synonyms.go`)

```go
package memory

import (
	"bufio"
	_ "embed"
	"strings"
)

//go:embed synonyms.txt
var synonymsData string

// synonymMap maps each token to its curated expansions (loaded bidirectionally).
var synonymMap = parseSynonyms(synonymsData)

func parseSynonyms(data string) map[string][]string {
	m := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		a := strings.ToLower(strings.TrimSpace(parts[0]))
		b := strings.ToLower(strings.TrimSpace(parts[1]))
		if a == "" || b == "" {
			continue
		}
		m[a] = appendUnique(m[a], b)
		m[b] = appendUnique(m[b], a)
	}
	return m
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// expandSynonyms returns tokens plus their curated synonym expansions, deduped.
func expandSynonyms(tokens []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, t := range tokens {
		add(t)
		for _, syn := range synonymMap[t] {
			add(syn)
		}
	}
	return out
}

// BuildSearchText derives the FTS search_text for a memory: identifier-split,
// synonym-expanded tokens from title + body + ref values, space-joined.
func BuildSearchText(title, body string, refs []Ref) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteByte(' ')
	b.WriteString(body)
	for _, r := range refs {
		b.WriteByte(' ')
		b.WriteString(r.Value)
	}
	return strings.Join(expandSynonyms(tokenize(b.String())), " ")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run 'TestExpandSynonyms|TestBuildSearchText' -v`
Expected: PASS. (`config` expands to both `configuration` and `cfg`; `db`→`database`.)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/synonyms.txt internal/memory/synonyms.go internal/memory/synonyms_test.go
git commit -m "feat: curated synonym expansion + BuildSearchText"
```

---

### Task 7: Fingerprints — shingles, Jaccard, MinHash, LSH bands, SimHash

**Files:**
- Create: `internal/memory/fingerprint.go`
- Create: `internal/memory/fingerprint_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import "testing"

func TestJaccard(t *testing.T) {
	a := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	b := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	if got := jaccard(a, b); got != 1 {
		t.Errorf("identical jaccard = %v, want 1", got)
	}
	c := map[string]struct{}{"p": {}, "q": {}}
	if got := jaccard(a, c); got != 0 {
		t.Errorf("disjoint jaccard = %v, want 0", got)
	}
}

func TestMinHashNearDupShareBand(t *testing.T) {
	near1 := shingleSet("the auth token refresh path leaks a goroutine on error")
	near2 := shingleSet("the auth token refresh path leaks a goroutine on failure")
	far := shingleSet("rendering the markdown export to stdout for the cli status")

	b1 := bandHashes(minHashSignature(near1))
	b2 := bandHashes(minHashSignature(near2))
	bf := bandHashes(minHashSignature(far))

	shares := func(x, y [lshBands]uint64) bool {
		for i := 0; i < lshBands; i++ {
			if x[i] == y[i] {
				return true
			}
		}
		return false
	}
	if !shares(b1, b2) {
		t.Error("near-duplicate texts should share at least one LSH band")
	}
	if shares(b1, bf) {
		t.Error("unrelated texts should not share an LSH band (probabilistic)")
	}
}

func TestSimHashStableAndClose(t *testing.T) {
	s := shingleSet("the auth token refresh path leaks a goroutine")
	if simHash64(s) != simHash64(s) {
		t.Error("simhash must be deterministic")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestJaccard|TestMinHash|TestSimHash' -v`
Expected: FAIL — `undefined: jaccard` (etc.).

- [ ] **Step 3: Write the implementation** (`internal/memory/fingerprint.go`)

```go
package memory

import (
	"hash/fnv"
	"math"
	"strings"
)

const (
	shingleSize = 3                     // words per shingle
	numHashes   = 128                   // MinHash signature length
	lshBands    = 16                    // LSH band count
	lshRows     = numHashes / lshBands  // rows per band (8)
)

// shingleSet returns the set of word-level k-shingles of text. Texts with fewer
// than shingleSize words fall back to the set of individual words.
func shingleSet(text string) map[string]struct{} {
	words := tokenize(text)
	set := map[string]struct{}{}
	if len(words) < shingleSize {
		for _, w := range words {
			set[w] = struct{}{}
		}
		return set
	}
	for i := 0; i+shingleSize <= len(words); i++ {
		set[strings.Join(words[i:i+shingleSize], " ")] = struct{}{}
	}
	return set
}

// jaccard returns |a∩b| / |a∪b|. Two empty sets are defined as identical (1).
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// hash64 hashes s with a per-permutation seed (FNV-1a over seed bytes + s).
func hash64(s string, seed uint64) uint64 {
	h := fnv.New64a()
	var sb [8]byte
	for i := 0; i < 8; i++ {
		sb[i] = byte(seed >> (8 * i))
	}
	_, _ = h.Write(sb[:])
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// minHashSignature computes the numHashes-length MinHash signature of set.
func minHashSignature(set map[string]struct{}) [numHashes]uint64 {
	var sig [numHashes]uint64
	for i := range sig {
		sig[i] = math.MaxUint64
	}
	for sh := range set {
		for i := 0; i < numHashes; i++ {
			if h := hash64(sh, uint64(i)); h < sig[i] {
				sig[i] = h
			}
		}
	}
	return sig
}

// bandHashes condenses a signature into one hash per LSH band; two sets that
// agree on any band are near-duplicate candidates.
func bandHashes(sig [numHashes]uint64) [lshBands]uint64 {
	var bands [lshBands]uint64
	for b := 0; b < lshBands; b++ {
		h := fnv.New64a()
		var buf [8]byte
		for r := 0; r < lshRows; r++ {
			v := sig[b*lshRows+r]
			for i := 0; i < 8; i++ {
				buf[i] = byte(v >> (8 * i))
			}
			_, _ = h.Write(buf[:])
		}
		bands[b] = h.Sum64()
	}
	return bands
}

// simHash64 is a 64-bit SimHash fingerprint over the set's shingles. Stored on
// each memory for future compaction (Phase 3); dedup uses MinHash+Jaccard.
func simHash64(set map[string]struct{}) uint64 {
	var v [64]int
	for sh := range set {
		h := hash64(sh, 0)
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run 'TestJaccard|TestMinHash|TestSimHash' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/fingerprint.go internal/memory/fingerprint_test.go
git commit -m "feat: shingles, Jaccard, MinHash-LSH bands, SimHash"
```

---

### Task 8: Lexical backend (`Lexical` interface + FTS5 impl)

**Files:**
- Create: `internal/memory/lexical.go`
- Create: `internal/memory/lexical_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFTSLexicalIndexAndSearch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	lex := NewFTSLexical(st)

	docs := map[string]string{
		"a": "auth token refresh leaks goroutine on error",
		"b": "markdown export renders to stdout",
		"c": "auth session config database connection",
	}
	for id, txt := range docs {
		if err := st.WriteTx(ctx, func(c store.Conn) error {
			return lex.Index(ctx, c, id, txt)
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := lex.Search(ctx, "auth token", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("expected lexical matches for 'auth token'")
	}
	if res[0].ID != "a" {
		t.Errorf("top match = %q, want \"a\"", res[0].ID)
	}
	if res[0].Score < res[len(res)-1].Score {
		t.Errorf("scores not descending: %+v", res)
	}

	// Delete then re-search: "b" gone.
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		return lex.Delete(ctx, c, "b")
	}); err != nil {
		t.Fatal(err)
	}
	res, _ = lex.Search(ctx, "markdown export", 10)
	for _, r := range res {
		if r.ID == "b" {
			t.Error("deleted doc b still returned")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestFTSLexical -v`
Expected: FAIL — `undefined: NewFTSLexical`.

- [ ] **Step 3: Write the implementation** (`internal/memory/lexical.go`)

```go
package memory

import (
	"context"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// LexResult is one lexical match: a memory id and a normalized score in [0,1]
// (higher = more relevant).
type LexResult struct {
	ID    string
	Score float64
}

// Lexical is the swappable lexical-search backend. The default is FTS5; a
// Go-side inverted index is the documented fallback if FTS5 is unavailable
// (spec §15). Index/Delete run inside the caller's write transaction so the
// lexical index stays atomic with the memories row.
type Lexical interface {
	Index(ctx context.Context, c store.Conn, id, searchText string) error
	Delete(ctx context.Context, c store.Conn, id string) error
	Search(ctx context.Context, query string, limit int) ([]LexResult, error)
}

// ftsLexical is the FTS5-backed implementation over the memories_fts vtable.
type ftsLexical struct{ st *store.Store }

// NewFTSLexical returns the default FTS5 lexical backend.
func NewFTSLexical(st *store.Store) Lexical { return &ftsLexical{st: st} }

func (f *ftsLexical) Index(ctx context.Context, c store.Conn, id, searchText string) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO memories_fts(memory_id, search_text) VALUES (?, ?)`, id, searchText)
	return err
}

func (f *ftsLexical) Delete(ctx context.Context, c store.Conn, id string) error {
	_, err := c.ExecContext(ctx, `DELETE FROM memories_fts WHERE memory_id = ?`, id)
	return err
}

// matchQuery builds a safe FTS5 MATCH expression: each query token is quoted
// and OR-combined, so arbitrary punctuation cannot trigger an FTS5 syntax error.
func matchQuery(query string) string {
	toks := tokenize(query)
	if len(toks) == 0 {
		return ""
	}
	quoted := make([]string, len(toks))
	for i, t := range toks {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}

func (f *ftsLexical) Search(ctx context.Context, query string, limit int) ([]LexResult, error) {
	m := matchQuery(query)
	if m == "" {
		return nil, nil
	}
	// NOTE: FTS5 bm25() returns NEGATIVE scores; more negative = more relevant.
	rows, err := f.st.DB.QueryContext(ctx,
		`SELECT memory_id, bm25(memories_fts) AS s
		   FROM memories_fts
		  WHERE memories_fts MATCH ?
		  ORDER BY s ASC
		  LIMIT ?`, m, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	var raw []float64
	for rows.Next() {
		var id string
		var s float64
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		raw = append(raw, -s) // flip so higher = better
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return normalizeScores(ids, raw), nil
}

// normalizeScores min-max normalizes raw scores into [0,1]; if all scores are
// equal (or a single result), every result scores 1.0.
func normalizeScores(ids []string, raw []float64) []LexResult {
	out := make([]LexResult, len(ids))
	if len(ids) == 0 {
		return out
	}
	min, max := raw[0], raw[0]
	for _, v := range raw {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	for i := range ids {
		score := 1.0
		if span > 0 {
			score = (raw[i] - min) / span
		}
		out[i] = LexResult{ID: ids[i], Score: score}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestFTSLexical -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/lexical.go internal/memory/lexical_test.go
git commit -m "feat: swappable Lexical interface + FTS5 backend (bm25)"
```

---

### Task 9: Engine — low-level insert, GetByID, scan helpers

**Files:**
- Create: `internal/memory/engine.go`
- Create: `internal/memory/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

func newTestEngine(t *testing.T, clk clock.Clock) *Engine {
	t.Helper()
	st := newTestStore(t)
	return New(st, ledger.New(st), clk)
}

func TestInsertAndGetByID(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id := "mem-1"
	nm := NewMemory{
		Kind:   KindLearning,
		Title:  "WAL needs busy_timeout",
		Body:   "set busy_timeout so writers wait instead of erroring SQLITE_BUSY",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
		Source: "cli",
	}
	st := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	if err := e.insert(ctx, id, nm, st, simHash64(set), bandHashes(minHashSignature(set)),
		clk.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	got, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("GetByID returned nil")
	}
	if got.Title != nm.Title || got.Kind != KindLearning {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Branch != "main" || got.Confidence != 1.0 || got.Status != "active" {
		t.Errorf("defaults wrong: %+v", got)
	}
	if len(got.Refs) != 1 || got.Refs[0].Value != "internal/store/store.go" {
		t.Errorf("refs not persisted: %+v", got.Refs)
	}

	miss, err := e.GetByID(ctx, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		t.Error("expected nil for missing id")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestInsertAndGetByID -v`
Expected: FAIL — `undefined: New` / `undefined: Engine`.

- [ ] **Step 3: Write the implementation** (`internal/memory/engine.go`)

```go
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Engine is the memory subsystem: write+dedup and recall over the store.
type Engine struct {
	st  *store.Store
	led *ledger.Ledger
	clk clock.Clock
	lex Lexical
}

// New returns an Engine with the default FTS5 lexical backend.
func New(st *store.Store, led *ledger.Ledger, clk clock.Clock) *Engine {
	return &Engine{st: st, led: led, clk: clk, lex: NewFTSLexical(st)}
}

const selectMemoriesSQL = `
SELECT id, kind, title, body, search_text, created_at, updated_at,
       COALESCE(branch,''), COALESCE(worktree,''), author, source, confidence,
       reinforcement_count, pinned, status, COALESCE(superseded_by,''), simhash, extra
  FROM memories`

// insert writes a brand-new memory row + refs + minhash bands + FTS entry,
// optionally superseding a prior record, all in one transaction.
func (e *Engine) insert(ctx context.Context, id string, nm NewMemory,
	searchText string, sim uint64, bands [lshBands]uint64, now int64) error {
	extraJSON, err := marshalExtra(nm.Extra)
	if err != nil {
		return err
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memories
			   (id, kind, title, body, search_text, created_at, updated_at,
			    branch, worktree, author, source, confidence, reinforcement_count,
			    pinned, status, superseded_by, simhash, extra)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, string(nm.Kind), nm.Title, nm.Body, searchText, now, now,
			nullStr(nm.Branch), nm.Worktree, nm.Author, nm.Source, 1.0, 0,
			boolToInt(nm.Pinned), "active", nil, int64(sim), extraJSON,
		); err != nil {
			return err
		}
		for _, r := range nm.Refs {
			if _, err := c.ExecContext(ctx,
				`INSERT OR IGNORE INTO memory_refs(memory_id, ref_type, ref_value)
				 VALUES (?,?,?)`, id, string(r.Type), r.Value); err != nil {
				return err
			}
		}
		for b := 0; b < lshBands; b++ {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memory_minhash(memory_id, band, hash) VALUES (?,?,?)`,
				id, b, int64(bands[b])); err != nil {
				return err
			}
		}
		if nm.SupersedesID != "" {
			if _, err := c.ExecContext(ctx,
				`UPDATE memories SET status='superseded', superseded_by=?, updated_at=?
				  WHERE id=? AND status='active'`, id, now, nm.SupersedesID); err != nil {
				return err
			}
		}
		return e.lex.Index(ctx, c, id, searchText)
	})
}

// GetByID returns a stored record (with refs) by id, or (nil, nil) if absent.
func (e *Engine) GetByID(ctx context.Context, id string) (*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx, selectMemoriesSQL+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs[0], nil
}

func scanMemories(rows *sql.Rows) ([]*Record, error) {
	var out []*Record
	for rows.Next() {
		var r Record
		var kind, status, extra string
		var sim int64
		var pinned int
		if err := rows.Scan(&r.ID, &kind, &r.Title, &r.Body, &r.SearchText,
			&r.CreatedAt, &r.UpdatedAt, &r.Branch, &r.Worktree, &r.Author, &r.Source,
			&r.Confidence, &r.ReinforcementCount, &pinned, &status, &r.SupersededBy,
			&sim, &extra); err != nil {
			return nil, err
		}
		r.Kind = Kind(kind)
		r.Status = status
		r.Pinned = pinned != 0
		r.SimHash = uint64(sim)
		if extra != "" && extra != "{}" {
			_ = json.Unmarshal([]byte(extra), &r.Extra)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (e *Engine) loadRefs(ctx context.Context, recs []*Record) error {
	for _, r := range recs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT ref_type, ref_value FROM memory_refs WHERE memory_id = ?`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t, v string
			if err := rows.Scan(&t, &v); err != nil {
				rows.Close()
				return err
			}
			r.Refs = append(r.Refs, Ref{Type: RefType(t), Value: v})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullStr maps "" → SQL NULL so branch-global memories store NULL, not "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func marshalExtra(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// errInvalidKind is returned by Record for an unknown kind.
var errInvalidKind = errors.New("memory: invalid kind")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestInsertAndGetByID -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/engine.go internal/memory/engine_test.go
git commit -m "feat: memory engine insert + GetByID round-trip"
```

---

### Task 10: Write + dedup (`Record`: reinforce-or-insert)

**Files:**
- Modify: `internal/memory/engine.go` (add `Record`, `findNearDup`, `reinforce`)
- Modify: `internal/memory/engine_test.go` (add tests)

- [ ] **Step 1: Write the failing test** (append to `internal/memory/engine_test.go`)

```go
func TestRecordReinforcesNearDup(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(2_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	nm := NewMemory{
		Kind:   KindBug,
		Title:  "goroutine leak",
		Body:   "the auth token refresh path leaks a goroutine on error return",
		Source: "cli",
	}
	id1, reinf1, err := e.Record(ctx, nm)
	if err != nil {
		t.Fatal(err)
	}
	if reinf1 {
		t.Error("first record should insert, not reinforce")
	}

	// Near-identical body, same kind → should reinforce id1, not insert.
	nm2 := nm
	nm2.Body = "the auth token refresh path leaks a goroutine on error returns"
	id2, reinf2, err := e.Record(ctx, nm2)
	if err != nil {
		t.Fatal(err)
	}
	if !reinf2 || id2 != id1 {
		t.Fatalf("near-dup should reinforce id1=%s, got id2=%s reinforced=%v", id1, id2, reinf2)
	}

	got, _ := e.GetByID(ctx, id1)
	if got.ReinforcementCount != 1 {
		t.Errorf("reinforcement_count = %d, want 1", got.ReinforcementCount)
	}

	var total int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("memories rows = %d, want 1 (reinforced, not duplicated)", total)
	}
}

func TestRecordInsertsDistinct(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(3_000_000)))
	ctx := context.Background()

	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "use WAL mode and busy_timeout for cross-process writers"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "render markdown export to stdout on demand only"}); err != nil {
		t.Fatal(err)
	}

	var total int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("memories rows = %d, want 2 (distinct inserts)", total)
	}
}

func TestRecordRejectsInvalidKind(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1)))
	if _, _, err := e.Record(context.Background(), NewMemory{Kind: "bogus"}); err == nil {
		t.Error("expected error for invalid kind")
	}
}

func TestRecordPinsDoNotRepeat(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1)))
	ctx := context.Background()
	id, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never run go.mod with a redundant toolchain directive"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.GetByID(ctx, id)
	if !got.Pinned {
		t.Error("do_not_repeat must be pinned")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestRecord' -v`
Expected: FAIL — `undefined: e.Record`.

- [ ] **Step 3: Write the implementation** (append to `internal/memory/engine.go`)

```go
// dedupThreshold is the Jaccard similarity at/above which a new memory
// reinforces an existing same-kind record instead of inserting a new row.
const dedupThreshold = 0.9

// Record inserts nm, or reinforces a near-duplicate same-kind active memory.
// It returns the affected row id and whether it was a reinforcement.
func (e *Engine) Record(ctx context.Context, nm NewMemory) (id string, reinforced bool, err error) {
	if !nm.Kind.Valid() {
		return "", false, errInvalidKind
	}
	if nm.Kind == KindDoNotRepeat {
		nm.Pinned = true
	}
	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	bands := bandHashes(minHashSignature(set))
	sim := simHash64(set)
	now := e.clk.Now().UnixMilli()

	candID, found, err := e.findNearDup(ctx, nm.Kind, bands, set)
	if err != nil {
		return "", false, err
	}
	if found {
		if err := e.reinforce(ctx, candID, nm, now); err != nil {
			return "", false, err
		}
		_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
		return candID, true, nil
	}

	newID := ids.New()
	if err := e.insert(ctx, newID, nm, searchText, sim, bands, now); err != nil {
		return "", false, err
	}
	_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
	return newID, false, nil
}

// findNearDup returns the id of an active same-kind memory whose body has
// Jaccard ≥ dedupThreshold with set, using MinHash-LSH bands to gather
// candidates and exact shingle Jaccard to confirm.
func (e *Engine) findNearDup(ctx context.Context, kind Kind,
	bands [lshBands]uint64, set map[string]struct{}) (string, bool, error) {
	seen := map[string]bool{}
	var candidates []string
	for b := 0; b < lshBands; b++ {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT mh.memory_id
			   FROM memory_minhash mh
			   JOIN memories m ON m.id = mh.memory_id
			  WHERE mh.band = ? AND mh.hash = ? AND m.kind = ? AND m.status = 'active'`,
			b, int64(bands[b]), string(kind))
		if err != nil {
			return "", false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", false, err
			}
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", false, err
		}
		rows.Close()
	}
	for _, id := range candidates {
		var body string
		if err := e.st.DB.QueryRowContext(ctx,
			`SELECT body FROM memories WHERE id = ?`, id).Scan(&body); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return "", false, err
		}
		if jaccard(set, shingleSet(body)) >= dedupThreshold {
			return id, true, nil
		}
	}
	return "", false, nil
}

// reinforce bumps an existing memory's reinforcement_count and confidence,
// refreshes updated_at, and unions in any new refs.
func (e *Engine) reinforce(ctx context.Context, id string, nm NewMemory, now int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories
			    SET reinforcement_count = reinforcement_count + 1,
			        updated_at = ?,
			        confidence = MIN(1.0, confidence + 0.1)
			  WHERE id = ?`, now, id); err != nil {
			return err
		}
		for _, r := range nm.Refs {
			if _, err := c.ExecContext(ctx,
				`INSERT OR IGNORE INTO memory_refs(memory_id, ref_type, ref_value)
				 VALUES (?,?,?)`, id, string(r.Type), r.Value); err != nil {
				return err
			}
		}
		return nil
	})
}
```

Add `"git.gerwim.nl/gerwim/recarto/internal/ids"` to the import block of `engine.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run 'TestRecord' -v`
Expected: PASS (all four).

- [ ] **Step 5: Run the whole package with the race detector**

Run: `go test -race ./internal/memory/ -v`
Expected: PASS, no race warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/engine.go internal/memory/engine_test.go
git commit -m "feat: write+dedup — MinHash-LSH reinforce-or-insert"
```

---

### Task 11: Recall scoring (structural + lexical + recency + reinforcement + branch)

**Files:**
- Create: `internal/memory/recall.go`
- Create: `internal/memory/recall_test.go`

This task implements `Recall` returning **all active candidates scored and sorted** (no budget yet — Task 12 adds the budgeter). Selection wiring is intentionally split so scoring can be golden-tested in isolation.

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestRecallStructuralBeatsLexical(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	// A: shares the query FILE (structural — strongest signal), weak words.
	structID, _, err := e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "store opens with pragmas",
		Body:   "open applies wal and busy timeout pragmas on every connection",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
		Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// B: matches the query WORDS strongly but no shared ref.
	_, _, err = e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "auth session refresh",
		Body:   "auth session refresh token rotation auth session refresh",
		Refs:   []Ref{{Type: RefFile, Value: "internal/auth/session.go"}},
		Branch: "main",
		Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		TaskHint: "auth session refresh",                 // favors B lexically
		Files:    []string{"internal/store/store.go"},    // favors A structurally
		Branch:   "main",
		TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 2 {
		t.Fatalf("expected both records, got %d", len(res))
	}
	if res[0].Record.ID != structID {
		t.Errorf("structural match should rank first; got %q (signals %+v)",
			res[0].Record.ID, res[0].Signals)
	}
}

func TestRecallDropsSuperseded(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(11_000_000)))
	ctx := context.Background()
	oldID, _, _ := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body: "we will store markdown files committed in the repo",
		Refs: []Ref{{Type: RefFile, Value: "internal/export/render.go"}}})
	// Supersede it.
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body: "markdown is on-demand export only, sqlite is source of truth",
		Refs: []Ref{{Type: RefFile, Value: "internal/export/render.go"}},
		SupersedesID: oldID}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{
		Files: []string{"internal/export/render.go"}, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Record.ID == oldID {
			t.Error("superseded record should not be recalled")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestRecall -v`
Expected: FAIL — `undefined: RecallQuery` / `undefined: e.Recall`.

- [ ] **Step 3: Write the implementation** (`internal/memory/recall.go`)

```go
package memory

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Recall scoring weights (golden-tested defaults). Structural overlap is the
// strongest signal: an exact file/symbol match is the best relevance evidence.
const (
	wStructural    = 0.45
	wLexical       = 0.25
	wRecency       = 0.15
	wReinforcement = 0.10
	wBranch        = 0.05

	recencyHalfLifeMs  = float64(14 * 24 * 60 * 60 * 1000) // 14 days
	defaultTokenBudget = 1500
	defaultLimit       = 50
)

// RecallQuery is the input to Engine.Recall.
type RecallQuery struct {
	TaskHint    string
	Files       []string
	Symbols     []string
	Kind        Kind   // "" → any kind
	Branch      string // current branch, for branch-affinity boost
	Limit       int    // max lexical candidates (0 → default)
	TokenBudget int    // 0 → default
}

// RecallResult is a selected memory with its score and per-signal breakdown.
type RecallResult struct {
	Record  *Record
	Score   float64
	Signals map[string]float64
}

// Recall scores and ranks memories by structural overlap, lexical (BM25),
// recency, reinforcement, and branch affinity, all multiplied by confidence.
// Task 12 wraps the budgeter around the sorted output.
func (e *Engine) Recall(ctx context.Context, q RecallQuery) ([]RecallResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	lex, err := e.lex.Search(ctx, lexicalQueryText(q), limit)
	if err != nil {
		return nil, err
	}
	lexScore := make(map[string]float64, len(lex))
	for _, r := range lex {
		lexScore[r.ID] = r.Score
	}

	refScore, err := e.structuralScores(ctx, q)
	if err != nil {
		return nil, err
	}

	pinned, err := e.pinnedIDs(ctx, q.Branch)
	if err != nil {
		return nil, err
	}

	idset := map[string]bool{}
	for id := range lexScore {
		idset[id] = true
	}
	for id := range refScore {
		idset[id] = true
	}
	for _, id := range pinned {
		idset[id] = true
	}

	recs, err := e.loadByIDs(ctx, idset, q.Kind)
	if err != nil {
		return nil, err
	}

	now := e.clk.Now().UnixMilli()
	scored := make([]RecallResult, 0, len(recs))
	for _, r := range recs {
		if r.Status != "active" {
			continue
		}
		sig := map[string]float64{
			"lexical":       lexScore[r.ID],
			"structural":    refScore[r.ID],
			"recency":       math.Exp(-float64(now-r.UpdatedAt) / recencyHalfLifeMs),
			"reinforcement": math.Min(1, math.Log1p(float64(r.ReinforcementCount))/math.Log(11)),
			"branch":        0,
		}
		if r.Branch == "" || r.Branch == q.Branch {
			sig["branch"] = 1
		}
		score := (wStructural*sig["structural"] + wLexical*sig["lexical"] +
			wRecency*sig["recency"] + wReinforcement*sig["reinforcement"] +
			wBranch*sig["branch"]) * r.Confidence
		scored = append(scored, RecallResult{Record: r, Score: score, Signals: sig})
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	return scored, nil
}

// lexicalQueryText assembles the BM25 query from the task hint, file basenames,
// and symbol names.
func lexicalQueryText(q RecallQuery) string {
	parts := []string{q.TaskHint}
	for _, f := range q.Files {
		parts = append(parts, filepath.Base(f))
	}
	parts = append(parts, q.Symbols...)
	return strings.Join(parts, " ")
}

// structuralScores returns memory_id → weighted Jaccard between a memory's refs
// and the query's file/symbol refs.
func (e *Engine) structuralScores(ctx context.Context, q RecallQuery) (map[string]float64, error) {
	type qref struct{ t, v string }
	var qrefs []qref
	for _, f := range q.Files {
		qrefs = append(qrefs, qref{string(RefFile), f})
	}
	for _, s := range q.Symbols {
		qrefs = append(qrefs, qref{string(RefSymbol), s})
	}
	scores := map[string]float64{}
	if len(qrefs) == 0 {
		return scores, nil
	}
	inter := map[string]int{}
	for _, qr := range qrefs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT memory_id FROM memory_refs WHERE ref_type=? AND ref_value=?`, qr.t, qr.v)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			inter[id]++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	for id, n := range inter {
		var total int
		if err := e.st.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_refs WHERE memory_id=?`, id).Scan(&total); err != nil {
			return nil, err
		}
		union := len(qrefs) + total - n
		if union > 0 {
			scores[id] = float64(n) / float64(union)
		}
	}
	return scores, nil
}

// pinnedIDs returns active pinned do_not_repeat ids visible on branch (or repo-global).
func (e *Engine) pinnedIDs(ctx context.Context, branch string) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE pinned=1 AND status='active' AND kind='do_not_repeat'
		    AND (branch IS NULL OR branch=?)`, branch)
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

// loadByIDs loads records (with refs) for the id set, optionally filtered by kind.
func (e *Engine) loadByIDs(ctx context.Context, idset map[string]bool, kind Kind) ([]*Record, error) {
	if len(idset) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(idset)+1)
	ph := make([]string, 0, len(idset))
	for id := range idset {
		args = append(args, id)
		ph = append(ph, "?")
	}
	query := selectMemoriesSQL + ` WHERE id IN (` + strings.Join(ph, ",") + `)`
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, string(kind))
	}
	rows, err := e.st.DB.QueryContext(ctx, query, args...)
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

Run: `go test ./internal/memory/ -run TestRecall -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go
git commit -m "feat: recall scoring (structural+lexical+recency+reinforcement+branch)"
```

---

### Task 12: Budgeter + pins + ledger counters

**Files:**
- Modify: `internal/memory/recall.go` (add `selectWithinBudget`, wire into `Recall`, increment counters)
- Modify: `internal/memory/recall_test.go` (add tests)

- [ ] **Step 1: Write the failing test** (append to `internal/memory/recall_test.go`)

```go
// add to the existing recall_test.go import block:
//   "git.gerwim.nl/gerwim/recarto/internal/ledger"

func TestRecallBudgetCapsAndPinsFirst(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(20_000_000)))
	ctx := context.Background()

	// One pinned do_not_repeat (always included, budget-exempt).
	dnrID, _, _ := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never add a redundant toolchain directive to go.mod it breaks builds",
		Refs: []Ref{{Type: RefFile, Value: "go.mod"}}})

	// Five DISTINCT same-file learnings (distinct bodies so dedup does not
	// collapse them) — each ~20 tokens, so a tiny budget must exclude several.
	bodies := []string{
		"alpha concurrency wal busy timeout immediate writer pool serialized mutex per connection",
		"bravo migration guarded user_version pragma idempotent first run lock serializes processes",
		"charlie recall structural lexical recency reinforcement branch budget greedy selection order",
		"delta synonym expansion identifier split camel snake kebab dotted token search text build",
		"echo minhash lsh band signature jaccard shingle near duplicate reinforce threshold confirm",
	}
	for _, b := range bodies {
		if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
			Body: b,
			Refs: []Ref{{Type: RefFile, Value: "go.mod"}}}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := e.Recall(ctx, RecallQuery{
		Files: []string{"go.mod"}, TokenBudget: 80})
	if err != nil {
		t.Fatal(err)
	}

	// Pin present despite the tiny budget; total recalled < 6 (some excluded).
	var sawPin bool
	for _, r := range res {
		if r.Record.ID == dnrID {
			sawPin = true
		}
	}
	if !sawPin {
		t.Error("pinned do_not_repeat must always be included")
	}
	if len(res) == 6 {
		t.Error("budget should have excluded some non-pinned learnings")
	}

	// Counters were written.
	recalled, _ := ledger.New(e.st).Get(ctx, "lifetime", "memories_recalled")
	if recalled <= 0 {
		t.Error("memories_recalled counter not incremented")
	}
}

func TestRecallMissIncrementsCounter(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(21_000_000)))
	ctx := context.Background()
	// Empty store + a query that matches nothing.
	res, err := e.Recall(ctx, RecallQuery{TaskHint: "nonexistent zzz", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no results, got %d", len(res))
	}
	misses, _ := ledger.New(e.st).Get(ctx, "lifetime", "recall_misses")
	if misses != 1 {
		t.Errorf("recall_misses = %d, want 1", misses)
	}
}
```

Note: `e.st` is unexported. Add this accessor near the top of `engine.go` so tests in-package can build a `ledger` on the same store (they are in `package memory`, so direct field access already works — `ledger.New(e.st)` compiles in-package). No production accessor needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run 'TestRecallBudget|TestRecallMiss' -v`
Expected: FAIL — both tests fail: budget not enforced (all 6 returned) and `recall_misses` is 0.

- [ ] **Step 3: Modify `Recall` to call the budgeter, and add `selectWithinBudget`** (in `internal/memory/recall.go`)

Replace the tail of `Recall` (the `sort.Slice(...)` + `return scored, nil`) with:

```go
	if len(scored) == 0 {
		_ = e.led.Incr(ctx, "lifetime", "recall_misses", 1)
		return nil, nil
	}
	budget := q.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	return e.selectWithinBudget(ctx, scored, budget), nil
```

Then add:

```go
// selectWithinBudget always includes pinned do_not_repeat (a dedicated, budget-
// exempt slice), then greedily adds the highest-scoring remaining records until
// the token budget is exhausted. It records the recall counters.
func (e *Engine) selectWithinBudget(ctx context.Context, scored []RecallResult, budget int) []RecallResult {
	var pinned, rest []RecallResult
	for _, s := range scored {
		if s.Record.Kind == KindDoNotRepeat && s.Record.Pinned {
			pinned = append(pinned, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].Score > pinned[j].Score })
	sort.Slice(rest, func(i, j int) bool { return rest[i].Score > rest[j].Score })

	tokensOf := func(r *Record) int { return ledger.EstimateTokens(r.Title + " " + r.Body) }
	var out []RecallResult
	used := 0
	for _, s := range pinned {
		out = append(out, s)
		used += tokensOf(s.Record)
	}
	for _, s := range rest {
		t := tokensOf(s.Record)
		if used+t > budget {
			continue
		}
		out = append(out, s)
		used += t
	}

	_ = e.led.Incr(ctx, "lifetime", "memories_recalled", int64(len(out)))
	_ = e.led.Incr(ctx, "lifetime", "recall_tokens_injected", int64(used))
	return out
}
```

Add `"git.gerwim.nl/gerwim/recarto/internal/ledger"` to the import block of `recall.go`.

(`sort` is already imported from Task 11. The `sort.Slice` previously at the tail of `Recall` is removed — `selectWithinBudget` now owns ordering.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run 'TestRecall' -v`
Expected: PASS (ordering, supersede, budget, and miss tests all green).

- [ ] **Step 5: Full suite + race + vet + fmt**

Run:
```bash
gofmt -l internal/        # expect: no output
go vet ./...              # expect: no output
go test -race ./...       # expect: ok for every package
```
Expected: all clean. **If `gofmt -l` lists any file, run `gofmt -w` on it and re-commit** (CI gates on gofmt — see cerebrum).

- [ ] **Step 6: Commit**

```bash
git add internal/memory/recall.go internal/memory/recall_test.go
git commit -m "feat: recall budgeter — pins-first greedy fill + ledger counters"
```

---

## Definition of Done (Phase 1B exit)

- `internal/memory` exposes `Engine` with `Record` (reinforce-or-insert near-dups) and `Recall` (token-budgeted, branch-aware, structural+lexical ranking), plus `GetByID`.
- `memories_fts` (schema v2) backs a swappable `Lexical` interface; FTS5 + `bm25()` proven by the Task 1 spike.
- Token estimator lives in `internal/ledger`; recall increments `memories_recorded`, `memories_recalled`, `recall_tokens_injected`, and `recall_misses`.
- `do_not_repeat` is pinned and always recalled; superseded records are never recalled.
- `go test -race ./...` is green; `gofmt -l internal/` and `go vet ./...` are clean.

**Not in 1B (deferred to 1C — Surfaces):** the MCP server + tools (`record_memory`/`recall_memory`/`record_bug`/`search_bugs`/`get_memory`/`list_memory`), the cobra CLI, the `gitctx` wiring that fills `RecallQuery.Branch`/`NewMemory.Branch`, Claude Code hooks/installer, and markdown export. `Engine` is deliberately git-agnostic — callers pass branch/worktree in — so 1C wires `internal/gitctx`.

## After all tasks complete

Announce and follow **superpowers:finishing-a-development-branch** to verify the suite and choose how to integrate (the work is on the `phase-1b-memory-engine` branch). Then update OpenWolf bookkeeping: `.wolf/anatomy.md` (new files), `.wolf/cerebrum.md` (any learnings — e.g. the FTS5 bm25-is-negative gotcha), and `.wolf/memory.md` (session line). Update the `recarto-overview` auto-memory to mark Plan 1B BUILT.
