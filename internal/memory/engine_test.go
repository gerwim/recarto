package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

func newTestEngine(t *testing.T, clk clock.Clock) *Engine {
	t.Helper()
	st := newTestStore(t)
	return New(st, ledger.New(st), clk)
}

func TestInsertAndGetByID(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id := "mem-1"
	nm := NewMemory{
		Kind:   KindLearning,
		Title:  "WAL needs busy_timeout",
		Body:   "set busy_timeout so writers wait instead of erroring SQLITE_BUSY",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
		Source: "cli",
	}
	st := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	if err := e.insert(ctx, id, nm, st, simHash64(set), bandHashes(minHashSignature(set)),
		clk.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	got, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("GetByID returned nil")
	}
	if got.Title != nm.Title || got.Kind != KindLearning {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Branch != "main" || got.Confidence != 1.0 || got.Status != "active" {
		t.Errorf("defaults wrong: %+v", got)
	}
	if len(got.Refs) != 1 || got.Refs[0].Value != "internal/store/store.go" {
		t.Errorf("refs not persisted: %+v", got.Refs)
	}

	miss, err := e.GetByID(ctx, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		t.Error("expected nil for missing id")
	}
}
