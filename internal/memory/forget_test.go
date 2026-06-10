package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestForgetRemovesMemory(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "x", Body: "forget me please"})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.Forget(ctx, id, "recorded in error")
	if err != nil || !ok {
		t.Fatalf("Forget = %v, %v; want true, nil", ok, err)
	}
	if rec, _ := e.GetByID(ctx, id); rec != nil {
		t.Error("memory still present after Forget")
	}
	recs, _ := e.List(ctx, ListFilter{})
	for _, r := range recs {
		if r.ID == id {
			t.Error("forgotten memory still listed")
		}
	}
	set, _ := e.Recall(ctx, RecallQuery{TaskHint: "forget me please"})
	for _, r := range set.Results {
		if r.Record.ID == id {
			t.Error("forgotten memory still a recall candidate")
		}
	}
}

func TestForgetUnknownIDIsNoOp(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ok, err := e.Forget(context.Background(), "nope", "")
	if err != nil || ok {
		t.Fatalf("Forget unknown = %v, %v; want false, nil", ok, err)
	}
}
