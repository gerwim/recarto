package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestResolveFeedback(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, clock.Real{})
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindFeedback, Title: "t", Body: "b", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.ResolveFeedback(ctx, id, "fixed in v2")
	if err != nil || !ok {
		t.Fatalf("ResolveFeedback = (%v, %v), want (true, nil)", ok, err)
	}
	rec, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "resolved" {
		t.Fatalf("status = %q, want resolved", rec.Status)
	}
	if note, _ := rec.Extra["resolution_note"].(string); note != "fixed in v2" {
		t.Fatalf("resolution_note = %q, want %q", note, "fixed in v2")
	}

	// Idempotent: a second resolve finds no active feedback with that id.
	ok, err = e.ResolveFeedback(ctx, id, "")
	if err != nil || ok {
		t.Fatalf("second ResolveFeedback = (%v, %v), want (false, nil)", ok, err)
	}
}
