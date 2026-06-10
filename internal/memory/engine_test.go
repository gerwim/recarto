package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
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
	// insert runs on a transaction connection; wrap it in a WriteTx like Record does.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.insert(ctx, c, id, nm, st, simHash64(set), bandHashes(minHashSignature(set)),
			clk.Now().UnixMilli())
	}); err != nil {
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
	id1, reinf1, _, err := e.Record(ctx, nm)
	if err != nil {
		t.Fatal(err)
	}
	if reinf1 {
		t.Error("first record should insert, not reinforce")
	}

	// Near-identical body, same kind → should reinforce id1, not insert.
	nm2 := nm
	nm2.Body = "the auth token refresh path leaks a goroutine on error returns"
	id2, reinf2, _, err := e.Record(ctx, nm2)
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

	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "use WAL mode and busy_timeout for cross-process writers"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
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
	if _, _, _, err := e.Record(context.Background(), NewMemory{Kind: "bogus"}); err == nil {
		t.Error("expected error for invalid kind")
	}
}

func TestRecordPinsDoNotRepeat(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1)))
	ctx := context.Background()
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never run go.mod with a redundant toolchain directive"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.GetByID(ctx, id)
	if !got.Pinned {
		t.Error("do_not_repeat must be pinned")
	}
}

// TestRecordConcurrentDedupConverges proves the dedup check-and-act is atomic:
// many goroutines recording identical content converge to ONE row (with the
// rest reinforcing) instead of racing to insert duplicates (spec §7.9). Under
// the old design (near-dup read on the pool, outside the write tx) this would
// produce multiple rows. Run with -race.
func TestRecordConcurrentDedupConverges(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(5_000_000)))
	ctx := context.Background()

	const n = 8
	nm := NewMemory{
		Kind:   KindLearning,
		Body:   "the auth token refresh path leaks a goroutine on error return",
		Source: "cli",
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := e.Record(ctx, nm); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Record failed: %v", err)
	}

	var rows, reinf int
	if err := e.st.DB.QueryRow(
		`SELECT count(*), COALESCE(MAX(reinforcement_count), 0) FROM memories`).
		Scan(&rows, &reinf); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("memories rows = %d, want 1 (concurrent identical records must converge)", rows)
	}
	if reinf != n-1 {
		t.Errorf("reinforcement_count = %d, want %d", reinf, n-1)
	}
}

func TestRecordSurfacesMiddleBandConflictCandidates(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(6_000_000)))
	ctx := context.Background()
	_, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindPreference, Title: "tabs", Body: "always use hard tab characters when indenting Go code in this project",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Similar topic, opposite claim: middle-band similarity (Jaccard ~0.50), NOT a near-dup.
	_, reinforced, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindPreference, Title: "spaces", Body: "always use two spaces when indenting Go code in this project",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reinforced {
		t.Fatal("opposite claim must not auto-reinforce")
	}
	if len(conflicts) == 0 {
		t.Fatalf("middle-band similar memory must surface as a conflict candidate")
	}
	if conflicts[0].Snippet == "" || conflicts[0].ID == "" || conflicts[0].Reason == "" {
		t.Errorf("candidate must carry id/snippet/reason: %+v", conflicts[0])
	}
}

func TestRecordSurfacesSharedRefConflicts(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(7_000_000)))
	ctx := context.Background()
	ref := []Ref{{Type: RefFile, Value: "internal/auth/login.go"}}
	if _, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindDecision, Title: "sessions in redis", Body: "store auth sessions in redis",
		Refs: ref, Author: "t", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindDecision, Title: "jwt only", Body: "drop server-side state entirely and use signed jwt",
		Refs: ref, Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawRefReason bool
	for _, c := range conflicts {
		if c.Reason == "shares refs" {
			sawRefReason = true
		}
	}
	if !sawRefReason {
		t.Fatalf("same-kind memory sharing a file ref must surface, got %+v", conflicts)
	}
}

func TestRecordNoConflictsForFactualKindsOrReinforce(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(8_000_000)))
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "npe", Body: "nil deref in parser on empty file",
		Author: "t", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, conflicts, err := e.Record(ctx, NewMemory{
		Kind: KindBug, Title: "panic", Body: "panic slice bounds in lexer on empty file input",
		Author: "t", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("bug kind must not produce conflict candidates, got %+v", conflicts)
	}

	// Exact restatement reinforces and returns no candidates.
	body := "the user always wants conventional commit messages on every commit in this repo"
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindPreference, Body: body, Author: "t", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	_, reinforced, conflicts, err := e.Record(ctx, NewMemory{Kind: KindPreference, Body: body, Author: "t", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if !reinforced || len(conflicts) != 0 {
		t.Fatalf("reinforce path must return no candidates, got reinforced=%v %+v", reinforced, conflicts)
	}
}
