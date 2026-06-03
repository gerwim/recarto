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

func TestRecordReinforcesNearDup(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(2_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	nm := NewMemory{
		Kind:   KindBug,
		Title:  "goroutine leak",
		Body:   "the auth token refresh path leaks a goroutine on error return",
		Source: "cli",
	}
	id1, reinf1, err := e.Record(ctx, nm)
	if err != nil {
		t.Fatal(err)
	}
	if reinf1 {
		t.Error("first record should insert, not reinforce")
	}

	// Near-identical body, same kind → should reinforce id1, not insert.
	nm2 := nm
	nm2.Body = "the auth token refresh path leaks a goroutine on error returns"
	id2, reinf2, err := e.Record(ctx, nm2)
	if err != nil {
		t.Fatal(err)
	}
	if !reinf2 || id2 != id1 {
		t.Fatalf("near-dup should reinforce id1=%s, got id2=%s reinforced=%v", id1, id2, reinf2)
	}

	got, _ := e.GetByID(ctx, id1)
	if got.ReinforcementCount != 1 {
		t.Errorf("reinforcement_count = %d, want 1", got.ReinforcementCount)
	}

	var total int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("memories rows = %d, want 1 (reinforced, not duplicated)", total)
	}
}

func TestRecordInsertsDistinct(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(3_000_000)))
	ctx := context.Background()

	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "use WAL mode and busy_timeout for cross-process writers"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "render markdown export to stdout on demand only"}); err != nil {
		t.Fatal(err)
	}

	var total int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("memories rows = %d, want 2 (distinct inserts)", total)
	}
}

func TestRecordRejectsInvalidKind(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1)))
	if _, _, err := e.Record(context.Background(), NewMemory{Kind: "bogus"}); err == nil {
		t.Error("expected error for invalid kind")
	}
}

func TestRecordPinsDoNotRepeat(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1)))
	ctx := context.Background()
	id, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never run go.mod with a redundant toolchain directive"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.GetByID(ctx, id)
	if !got.Pinned {
		t.Error("do_not_repeat must be pinned")
	}
}
