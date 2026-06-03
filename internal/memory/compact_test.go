package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
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

func TestGCSupersededDeletesRows(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(13_000_000)))
	ctx := context.Background()
	oldID, _, err := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body: "we will store markdown files committed in the repo"})
	if err != nil {
		t.Fatal(err)
	}
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
	oldID, _, err := e.Record(ctx, NewMemory{Kind: KindDecision, Body: "old fact about the thing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision, Body: "totally new and different statement", SupersedesID: oldID}); err != nil {
		t.Fatal(err)
	}

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
	stale := insertActive(t, e, KindLearning, body+" one", 1)    // age 99 -> lowest relevance
	mid := insertActive(t, e, KindLearning, body+" two", 90)     // age 10
	fresh := insertActive(t, e, KindLearning, body+" three", 95) // age 5
	rowTok := ledger.EstimateTokens(body + " one")               // each row ~ same size
	budget := 2*rowTok + 1                                       // fits exactly two rows

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

func TestEvictDryRunDoesNotArchive(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(14_400_000)))
	ctx := context.Background()
	id := insertActive(t, e, KindLearning, "an evictable learning under dry-run pressure", 1)
	// Tiny budget => the row WOULD be archived, but dry-run must not write.
	archived, _, _, err := e.evictToBudget(ctx, 100, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Errorf("dry-run plan archived = %d, want 1", archived)
	}
	if statusOf(t, e, id) != "active" {
		t.Error("dry-run must not actually archive the row")
	}
}
