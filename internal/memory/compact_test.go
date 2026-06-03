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
