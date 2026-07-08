package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestImportMemoryInsertReinforceSkip(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()
	im := ImportMemory{
		ID: "snap-1", Kind: KindLearning, Title: "t", Body: "a durable learning about caching layers",
		Branch: "main", Author: "alice", Source: "snapshot", Confidence: 0.8,
		ReinforcementCount: 3, CreatedAt: 1000,
		Refs: []Ref{{Type: RefFile, Value: "cache.go"}},
	}

	out, id, err := e.ImportMemory(ctx, im)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportInserted || id != "snap-1" {
		t.Fatalf("want inserted snap-1, got %v %s", out, id)
	}
	rec, _ := e.GetByID(ctx, "snap-1")
	if rec == nil || rec.CreatedAt != 1000 || rec.ReinforcementCount != 3 || rec.Confidence != 0.8 {
		t.Fatalf("metadata not preserved: %+v", rec)
	}

	out, _, err = e.ImportMemory(ctx, im)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportSkipped {
		t.Fatalf("re-import same id should skip, got %v", out)
	}

	dup := im
	dup.ID = "snap-2"
	dup.Body = "a durable learning about caching layers!"
	out, _, err = e.ImportMemory(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportReinforced {
		t.Fatalf("near-dup should reinforce, got %v", out)
	}

	rec2, _ := e.GetByID(ctx, "snap-1")
	if rec2 == nil {
		t.Fatal("snap-1 should still exist after reinforce")
	}
	if rec2.ReinforcementCount != 4 { // was 3, +1 from reinforce
		t.Fatalf("reinforce should bump count to 4, got %d", rec2.ReinforcementCount)
	}
	if rec2.UpdatedAt <= 1000 {
		t.Fatalf("reinforce must stamp current time, not the stale snapshot CreatedAt; got UpdatedAt=%d", rec2.UpdatedAt)
	}
}

func TestImportReinforceKeepsHigherPinnedAndConfidence(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	// Existing local memory: unpinned, low confidence.
	base := ImportMemory{
		ID: "b1", Kind: KindLearning, Title: "t",
		Body: "a durable learning about caching layers", Confidence: 0.5, CreatedAt: 1000,
	}
	if out, _, err := e.ImportMemory(ctx, base); err != nil || out != ImportInserted {
		t.Fatalf("setup insert: out=%v err=%v", out, err)
	}

	// Incoming near-duplicate (different id): pinned + higher confidence.
	inc := ImportMemory{
		ID: "b2", Kind: KindLearning, Title: "t",
		Body: "a durable learning about caching layers!", Confidence: 0.95, Pinned: true, CreatedAt: 2000,
	}
	out, id, err := e.ImportMemory(ctx, inc)
	if err != nil || out != ImportReinforced {
		t.Fatalf("want reinforced, got out=%v err=%v", out, err)
	}
	rec, _ := e.GetByID(ctx, id)
	if rec == nil {
		t.Fatal("reinforced record should exist")
	}
	if !rec.Pinned {
		t.Fatalf("reinforce must keep the higher pinned (incoming was pinned): %+v", rec)
	}
	if rec.Confidence < 0.95 {
		t.Fatalf("reinforce must keep the higher confidence 0.95, got %v", rec.Confidence)
	}
}

// TestImportMemoryQueuesRowsForHTTPPush proves the mechanism the
// session-start hook's ordering fix depends on: a memory absorbed from the
// committed team snapshot (internal/snapshot, via ImportMemory) must also be
// queued in the local HTTP-sync outbox, exactly like a locally-authored
// Record — otherwise reordering the session-start hook to push after the
// snapshot import would have nothing new to push.
func TestImportMemoryQueuesRowsForHTTPPush(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	// Fresh insert via ImportMemory (the snapshot-import path).
	inserted := ImportMemory{
		ID: "snap-ins", Kind: KindLearning, Title: "t",
		Body: "a durable learning imported from a teammate's snapshot", CreatedAt: 1000,
	}
	if out, _, err := e.ImportMemory(ctx, inserted); err != nil || out != ImportInserted {
		t.Fatalf("setup insert: out=%v err=%v", out, err)
	}
	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Record.ID != "snap-ins" {
		t.Fatalf("a fresh ImportMemory insert must be queued for push, got %+v", pending)
	}
	clearOutbox(t, e, ctx, "snap-ins")

	// Reinforce-by-near-dup via ImportMemory (independent authoring converging).
	reinforcing := ImportMemory{
		ID: "snap-reinf", Kind: KindLearning, Title: "t",
		Body: "a durable learning imported from a teammate's snapshot!", CreatedAt: 2000,
	}
	out, canonID, err := e.ImportMemory(ctx, reinforcing)
	if err != nil || out != ImportReinforced {
		t.Fatalf("setup reinforce: out=%v err=%v", out, err)
	}
	pending, err = e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pending {
		if p.Record.ID == canonID {
			found = true
		}
	}
	if !found {
		t.Fatalf("an ImportMemory reinforce must queue the canonical row for push, got %+v", pending)
	}
}
