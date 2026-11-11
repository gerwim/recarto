package graph

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/store"
)

// newTestEngine builds a graph engine over a fresh temp store.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e, err := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestNewEngine(t *testing.T) {
	e := newTestEngine(t)
	if e.reg == nil {
		t.Fatal("registry not built")
	}
}

func TestEnclosing(t *testing.T) {
	spans := []span{{"a", 0, 100}, {"b", 10, 40}, {"c", 50, 60}}
	if got := enclosing(spans, 20); got != "b" {
		t.Errorf("enclosing(20) = %q, want b (innermost)", got)
	}
	if got := enclosing(spans, 70); got != "a" {
		t.Errorf("enclosing(70) = %q, want a", got)
	}
	if got := enclosing(spans, 200); got != "" {
		t.Errorf("enclosing(200) = %q, want empty", got)
	}
}
