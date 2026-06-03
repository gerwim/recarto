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
