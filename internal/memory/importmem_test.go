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
