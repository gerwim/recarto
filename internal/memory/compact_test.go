package memory

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
	first, err := e.Compact(ctx, CompactOptions{}) // first run sets last_run
	if err != nil {
		t.Fatal(err)
	}
	if !first.Ran {
		t.Fatal("first compaction on a fresh store should run (gate not yet set)")
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
	// At the exact grace boundary (delta == archiveGraceEpochs): the strict `>`
	// means the row survives. This pins the boundary semantics.
	n, err = e.gcArchived(ctx, 1+archiveGraceEpochs, false)
	if err != nil || n != 0 {
		t.Fatalf("at grace boundary gc'd %d (err %v), want 0", n, err)
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

func TestTodoIsEvictable(t *testing.T) {
	if !evictable(KindTodo) {
		t.Error("todo should be evictable so untouched todos decay")
	}
	if got := halfLifeEpochs(KindTodo); got != halfLifeStdEpochs {
		t.Errorf("todo half-life = %v, want standard %v", got, halfLifeStdEpochs)
	}
}

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
