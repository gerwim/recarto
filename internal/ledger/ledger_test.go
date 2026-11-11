package ledger

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gerwim/recarto/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIncrAndGet(t *testing.T) {
	st := newStore(t)
	l := New(st)
	ctx := context.Background()

	if err := l.Incr(ctx, "lifetime", "reads", 3); err != nil {
		t.Fatal(err)
	}
	if err := l.Incr(ctx, "lifetime", "reads", 2); err != nil {
		t.Fatal(err)
	}
	got, err := l.Get(ctx, "lifetime", "reads")
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("reads = %d, want 5", got)
	}
}

func TestGetMissingIsZero(t *testing.T) {
	st := newStore(t)
	l := New(st)
	got, err := l.Get(context.Background(), "lifetime", "never")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("missing counter = %d, want 0", got)
	}
}

func TestIncrConcurrentInProcess(t *testing.T) {
	st := newStore(t)
	l := New(st)
	ctx := context.Background()
	const g, n = 8, 250
	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
					t.Errorf("incr: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	got, err := l.Get(ctx, "lifetime", "stress")
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(g*n) {
		t.Fatalf("stress = %d, want %d (lost in-process updates)", got, g*n)
	}
}
