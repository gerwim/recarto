package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
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

// TestImportMemorySkipsTombstonedID reproduces the round-7 gap: a stale
// committed .recarto/memory.jsonl snapshot still lists an id that was Forgotten
// locally (the row and its MinHash bands are gone). Importing it must not
// resurrect the memory.
func TestImportMemorySkipsTombstonedID(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "ephemeral note about caches"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)
	clearOutbox(t, e, ctx, id)
	if ok, err := e.Forget(ctx, id, "test"); err != nil || !ok {
		t.Fatalf("Forget: ok=%v err=%v", ok, err)
	}

	stale := ImportMemory{
		ID: id, Kind: KindLearning, Title: "t", Body: "ephemeral note about caches",
		CreatedAt: before.CreatedAt,
	}
	out, _, err := e.ImportMemory(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportSkipped {
		t.Fatalf("a stale snapshot record for a tombstoned id must be skipped, got %v", out)
	}
	if got, _ := e.GetByID(ctx, id); got != nil {
		t.Fatalf("tombstoned id must not be resurrected: %+v", got)
	}
	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.Record.ID == id {
			t.Fatalf("a skipped tombstoned import must not be queued for push: %+v", pending)
		}
	}
	all, err := e.List(ctx, ListFilter{Kind: KindLearning})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.ID == id {
			t.Fatalf("a tombstoned id must not surface after a stale import, got %+v", all)
		}
	}
}

// TestImportMemorySkipsTombstonedIDLearnedViaSyncApply is the same scenario,
// but the tombstone was learned via the actual session-start pull mechanism
// (SyncApply applying a remote delete), not a local Forget — this is the exact
// "pull tombstone, then legacy snapshot still lists the id" sequence.
func TestImportMemorySkipsTombstonedIDLearnedViaSyncApply(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "always jitter retry backoff"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)
	deleteAt := before.UpdatedAt + 1000
	if out, _, err := e.SyncApply(ctx, syncwire.Record{ID: id, Deleted: true, UpdatedAt: deleteAt}); err != nil || out != SyncTombstoned {
		t.Fatalf("setup tombstone via SyncApply: out=%v err=%v", out, err)
	}

	stale := ImportMemory{
		ID: id, Kind: KindLearning, Title: "t", Body: "always jitter retry backoff",
		CreatedAt: before.CreatedAt,
	}
	out, _, err := e.ImportMemory(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if out != ImportSkipped {
		t.Fatalf("a stale snapshot import of a remotely-tombstoned id must be skipped, got %v", out)
	}
	if got, _ := e.GetByID(ctx, id); got != nil {
		t.Fatalf("id learned as tombstoned via sync pull must not be resurrected by snapshot import: %+v", got)
	}
}

// TestImportMemoryAllowsReauthoredIDAfterTombstone guards against
// over-blocking: an id whose alias was merged into a since-tombstoned
// canonical row may legitimately reinsert as its own row when its CreatedAt is
// newer than the tombstone — mirroring SyncApply's "a record strictly newer
// than the tombstone may reinsert" rule. The reinsertion must also retire the
// stale alias so a later tombstone for the same id deletes the right row.
func TestImportMemoryAllowsReauthoredIDAfterTombstone(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	canonID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always cap goroutine pool size under load",
	})
	if err != nil {
		t.Fatal(err)
	}
	clearOutbox(t, e, ctx, canonID)

	// Merge alias "origin-b" -> canonID via SyncApply's near-dup reinforce path.
	dup := syncwire.Record{
		ID: "origin-b", Kind: string(KindLearning), Title: "t",
		Body: "always cap goroutine pool size under load!", CreatedAt: 500, UpdatedAt: 500,
	}
	out, mergedInto, err := e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || mergedInto != canonID {
		t.Fatalf("setup: want reinforced into %s, got %v %s", canonID, out, mergedInto)
	}

	canonAfterMerge, _ := e.GetByID(ctx, canonID)
	deleteAt := canonAfterMerge.UpdatedAt + 1000
	if out, target, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-b", Deleted: true, UpdatedAt: deleteAt}); err != nil || out != SyncTombstoned || target != canonID {
		t.Fatalf("setup tombstone via alias: out=%v target=%s err=%v", out, target, err)
	}

	// A snapshot record for "origin-b" with a CreatedAt strictly NEWER than the
	// tombstone must be allowed through (a genuine re-authoring), not blocked.
	reauthored := ImportMemory{
		ID: "origin-b", Kind: KindLearning, Title: "t",
		Body: "always cap goroutine pool size under load, reborn", CreatedAt: deleteAt + 1000,
	}
	out2, id2, err := e.ImportMemory(ctx, reauthored)
	if err != nil {
		t.Fatal(err)
	}
	if out2 != ImportInserted || id2 != "origin-b" {
		t.Fatalf("a record newer than the tombstone must legitimately reinsert, got %v %s", out2, id2)
	}
	if got, _ := e.GetByID(ctx, "origin-b"); got == nil {
		t.Fatal("reauthored id must exist as its own row")
	}

	// A later tombstone for "origin-b" must delete THAT row, not misdirect
	// through the stale (pre-reinsertion) alias to the long-gone canonical id.
	forgetAt := deleteAt + 2000
	out3, target3, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-b", Deleted: true, UpdatedAt: forgetAt})
	if err != nil {
		t.Fatal(err)
	}
	if out3 != SyncTombstoned || target3 != "origin-b" {
		t.Fatalf("a stale alias must not misdirect the tombstone: want target=origin-b, got out=%v target=%s", out3, target3)
	}
	if got, _ := e.GetByID(ctx, "origin-b"); got != nil {
		t.Fatalf("the reauthored row must be deleted, but survived: %+v", got)
	}
}

// TestImportMemoryTombstoneGuardResolvesAlias exercises the aliased-canonical
// branch of tombstoneCovers directly: the tombstone is recorded under the
// CANONICAL id, and the import id is only linked to it via memory_sync_merges.
func TestImportMemoryTombstoneGuardResolvesAlias(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	ctx := context.Background()

	canonID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always vendor go modules for reproducible builds",
	})
	if err != nil {
		t.Fatal(err)
	}
	clearOutbox(t, e, ctx, canonID)

	dup := syncwire.Record{
		ID: "origin-c", Kind: string(KindLearning), Title: "t",
		Body: "always vendor go modules for reproducible builds!", CreatedAt: 500, UpdatedAt: 500,
	}
	out, mergedInto, err := e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || mergedInto != canonID {
		t.Fatalf("setup: want reinforced into %s, got %v %s", canonID, out, mergedInto)
	}

	canonAfterMerge, _ := e.GetByID(ctx, canonID)
	deleteAt := canonAfterMerge.UpdatedAt + 1000
	if out, target, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-c", Deleted: true, UpdatedAt: deleteAt}); err != nil || out != SyncTombstoned || target != canonID {
		t.Fatalf("setup tombstone via alias: out=%v target=%s err=%v", out, target, err)
	}

	// A snapshot import for "origin-c" whose CreatedAt is at-or-before the
	// canonical tombstone must be dropped, resolved through the alias.
	stale := ImportMemory{
		ID: "origin-c", Kind: KindLearning, Title: "t",
		Body: "always vendor go modules for reproducible builds!", CreatedAt: deleteAt - 1,
	}
	out2, _, err := e.ImportMemory(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if out2 != ImportSkipped {
		t.Fatalf("import must resolve the alias to the canonical tombstone and skip, got %v", out2)
	}
	if got, _ := e.GetByID(ctx, "origin-c"); got != nil {
		t.Fatalf("aliased id must not be resurrected: %+v", got)
	}
	if got, _ := e.GetByID(ctx, canonID); got != nil {
		t.Fatalf("canonical row must stay deleted: %+v", got)
	}
}
