package memory

import (
	"context"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
)

// TestAllRecordsIncludesEveryStatus pins the difference between List
// (status='active' by default, for recall/markdown export) and AllRecords
// (every status, for the full-history CLI export): archived/done/superseded
// rows must round-trip too.
func TestAllRecordsIncludesEveryStatus(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	openID, _, _, err := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "open", Body: "open todo body", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	doneID, _, _, err := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "done", Body: "done todo body", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	all, err := e.AllRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("AllRecords = %d, want 2 (both statuses)", len(all))
	}

	byID := map[string]*Record{}
	for _, r := range all {
		byID[r.ID] = r
	}
	if byID[openID] == nil || byID[openID].Status != "active" {
		t.Fatalf("open todo missing or wrong status: %+v", byID[openID])
	}
	if byID[doneID] == nil || byID[doneID].Status != "done" {
		t.Fatalf("done todo missing or wrong status: %+v", byID[doneID])
	}

	// List (default active-only) must still exclude the done row.
	active, err := e.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != openID {
		t.Fatalf("List() active-only = %+v, want only the open todo", active)
	}
}

// TestAllTombstonesIncludesAlreadyPushed pins the difference between
// PendingTombstones (pushed=0 only, the local push queue) and AllTombstones
// (every tombstone, for the full-history export, which must ship deletes
// already confirmed pushed by ordinary sync too).
func TestAllTombstonesIncludesAlreadyPushed(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "will be forgotten locally"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Forget(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}

	pending, err := e.PendingTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("PendingTombstones = %+v, want one pending tombstone for %s", pending, id)
	}

	// Mark it pushed, mirroring a confirmed sync push.
	if err := e.MarkTombstonesPushed(ctx, []IDVersion{{ID: id, Version: pending[0].UpdatedAt}}); err != nil {
		t.Fatal(err)
	}
	pendingAfter, err := e.PendingTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingAfter) != 0 {
		t.Fatalf("PendingTombstones after MarkTombstonesPushed = %+v, want none pending", pendingAfter)
	}

	all, err := e.AllTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != id || !all[0].Deleted {
		t.Fatalf("AllTombstones = %+v, want the already-pushed tombstone for %s", all, id)
	}
}
