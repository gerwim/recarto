package ledger

import (
	"context"
	"path/filepath"
	"slices"
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

func TestMetricsByPrefix(t *testing.T) {
	l := New(newStore(t))
	ctx := context.Background()
	for _, c := range [][2]string{
		{"s", "lines:/a%_b@1:1-5"}, {"s", "lines:/a%_b@1:7-9"},
		{"s", "lines:/axb@1:1-5"}, // what LIKE would also match
		{"other", "lines:/a%_b@1:1-5"},
	} {
		if err := l.Incr(ctx, c[0], c[1], 1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Metrics(ctx, "s", "lines:/a%_b@1:")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"lines:/a%_b@1:1-5", "lines:/a%_b@1:7-9"}; !slices.Equal(got, want) {
		t.Errorf("Metrics = %q, want %q", got, want)
	}
}

func TestClaimOnce(t *testing.T) {
	l := New(newStore(t))
	ctx := context.Background()
	const g = 8
	won := make([]bool, g)
	var wg sync.WaitGroup
	for i := range won {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if won[i], err = l.Claim(ctx, "session:s", "large_read_nudged"); err != nil {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(slices.DeleteFunc(won, func(w bool) bool { return !w })); n != 1 {
		t.Errorf("%d concurrent claims won, want 1", n)
	}
	if v, _ := l.Get(ctx, "session:s", "large_read_nudged"); v != 1 {
		t.Errorf("value = %d, want 1", v)
	}
	if again, err := l.Claim(ctx, "session:s", "large_read_nudged"); err != nil || again {
		t.Errorf("repeat claim = %v, %v; want false", again, err)
	}
	if other, err := l.Claim(ctx, "session:t", "large_read_nudged"); err != nil || !other {
		t.Errorf("claim in another scope = %v, %v; want true", other, err)
	}
}
