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
