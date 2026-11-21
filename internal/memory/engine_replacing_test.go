package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
)

func TestRecordWithAbsentSupersedesIDOnlyQueuesNewMemory(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: "a distinct replacement body", SupersedesID: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.st.DB.Query(`SELECT memory_id FROM memory_sync_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	})
	var queued []string
	for rows.Next() {
		var queuedID string
		if err := rows.Scan(&queuedID); err != nil {
			t.Fatal(err)
		}
		queued = append(queued, queuedID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0] != id {
		t.Fatalf("outbox ids = %v, want only new id %s", queued, id)
	}
}

func TestRecordReplacing(t *testing.T) {
	t.Run("active target is replaced", func(t *testing.T) {
		e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
		ctx := context.Background()
		oldID, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: "old decision about the locking order"})
		if err != nil {
			t.Fatal(err)
		}
		newID, reinforced, replaced, _, err := e.RecordReplacing(ctx, NewMemory{Kind: KindLearning, Body: "new fact about index ownership", SupersedesID: oldID})
		if err != nil || reinforced || !replaced {
			t.Fatalf("RecordReplacing = id %q, reinforced %v, replaced %v, err %v", newID, reinforced, replaced, err)
		}
		old, err := e.GetByID(ctx, oldID)
		if err != nil || old.Status != "superseded" || old.SupersededBy != newID {
			t.Fatalf("old memory = %+v, err %v", old, err)
		}
	})

	t.Run("inactive target is not reported replaced", func(t *testing.T) {
		e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
		ctx := context.Background()
		oldID, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: "first distinct old locking fact"})
		if err != nil {
			t.Fatal(err)
		}
		priorID, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: "second distinct replacement fact", SupersedesID: oldID})
		if err != nil {
			t.Fatal(err)
		}
		_, _, replaced, _, err := e.RecordReplacing(ctx, NewMemory{Kind: KindLearning, Body: "third distinct replacement fact", SupersedesID: oldID})
		if err != nil || replaced {
			t.Fatalf("RecordReplacing replaced=%v, err=%v, want false,nil", replaced, err)
		}
		old, err := e.GetByID(ctx, oldID)
		if err != nil || old.Status != "superseded" || old.SupersededBy != priorID {
			t.Fatalf("old memory = %+v, err %v", old, err)
		}
	})

	t.Run("absent target leaves store unchanged", func(t *testing.T) {
		e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
		before := memoryCount(t, e)
		_, _, _, _, err := e.RecordReplacing(context.Background(), NewMemory{Kind: KindLearning, Body: "unique new memory body here", SupersedesID: "absent"})
		if !errors.Is(err, ErrNotHeld) {
			t.Fatalf("RecordReplacing err = %v, want ErrNotHeld", err)
		}
		if got := memoryCount(t, e); got != before {
			t.Fatalf("memory count = %d, want unchanged %d", got, before)
		}
	})

	t.Run("absent target does not reinforce", func(t *testing.T) {
		e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
		ctx := context.Background()
		id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: "the same durable fact about safe index writes"})
		if err != nil {
			t.Fatal(err)
		}
		before, err := e.GetByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, _, err = e.RecordReplacing(ctx, NewMemory{Kind: KindLearning, Body: "the same durable fact about safe index writes", SupersedesID: "absent"})
		if !errors.Is(err, ErrNotHeld) {
			t.Fatalf("RecordReplacing err = %v, want ErrNotHeld", err)
		}
		after, err := e.GetByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if after.ReinforcementCount != before.ReinforcementCount {
			t.Fatalf("reinforcement count = %d, want unchanged %d", after.ReinforcementCount, before.ReinforcementCount)
		}
		if got := memoryCount(t, e); got != 1 {
			t.Fatalf("memory count = %d, want 1", got)
		}
	})
}

func memoryCount(t *testing.T, e *Engine) int {
	t.Helper()
	var count int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
