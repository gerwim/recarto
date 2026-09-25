package memory

import (
	"context"
	"math"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

const floorTestBranch = "feature/index-speed-improvements"

func TestFloorUnrelatedTaskRecallsNothing(t *testing.T) {
	e := floorTestEngine(t, true)
	set, err := e.Recall(context.Background(), RecallQuery{TaskHint: "integrate Stripe payment webhooks for subscriptions", Branch: floorTestBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Results) != 0 {
		t.Fatalf("unrelated recall returned %v, want empty", resultIDs(set.Results))
	}
	var misses int
	if err := e.st.DB.QueryRow(`SELECT value FROM counters WHERE scope='lifetime' AND metric='recall_misses'`).Scan(&misses); err != nil {
		t.Fatal(err)
	}
	if misses != 1 {
		t.Fatalf("recall_misses = %d, want 1", misses)
	}
}

func TestFloorRelatedTaskKeepsPins(t *testing.T) {
	e := floorTestEngine(t, true)
	set, err := e.Recall(context.Background(), RecallQuery{TaskHint: "concurrent recarto processes fail with SQLITE_BUSY database is locked", Branch: floorTestBranch, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, result := range set.Results {
		got[result.Record.ID] = true
	}
	for _, id := range []string{"019e9de6-15da-7ac6-a4b8-4c7c990100f8", "019eb052-584e-7bea-8f6f-6aa710987675", "019eb056-b953-78c2-86fb-256b310b4353"} {
		if !got[id] {
			t.Errorf("related recall omitted %s: %v", id, resultIDs(set.Results))
		}
	}
}

func TestFloorStructuralBypass(t *testing.T) {
	e := floorTestEngine(t, true)
	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "integrate Stripe payment webhooks for subscriptions",
		Files:    []string{"internal/store/migrate.go"}, Branch: floorTestBranch, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, result := range set.Results {
		got[result.Record.ID] = true
	}
	if !got["019e9de6-15da-7ac6-a4b8-4c7c990100f8"] {
		t.Fatalf("structural match was hidden: %v", resultIDs(set.Results))
	}
	if !got["019eb052-584e-7bea-8f6f-6aa710987675"] || !got["019eb056-b953-78c2-86fb-256b310b4353"] {
		t.Fatalf("structural related recall omitted pins: %v", resultIDs(set.Results))
	}
}

func TestFloorDisabledByEnv(t *testing.T) {
	t.Setenv("RECARTO_RECALL_FLOOR", "0")
	e := floorTestEngine(t, false)
	query := "integrate Stripe payment webhooks for subscriptions"
	if e.emb == nil {
		t.Fatal("semantic model is unavailable")
	}
	set, err := e.Recall(context.Background(), RecallQuery{TaskHint: query, Branch: floorTestBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Results) == 0 {
		t.Fatal("floor-disabled unrelated query returned no semantic candidates")
	}
}

func TestFloorNotAppliedWithoutText(t *testing.T) {
	e := floorTestEngine(t, true)
	base, err := e.Recall(context.Background(), RecallQuery{Baseline: true, Branch: floorTestBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Results) == 0 {
		t.Fatal("baseline recall returned no memories")
	}
	files, err := e.Recall(context.Background(), RecallQuery{Files: []string{"***"}, Branch: floorTestBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Results) == 0 {
		t.Fatal("files-only query without terms should retain ordinary pin behavior")
	}
}

func TestCoverageIDF(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()
	for _, doc := range []struct{ title, body string }{
		{"alpha rare", "first distinct document"},
		{"alpha common", "second distinct document"},
		{"beta common", "third distinct document"},
	} {
		if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: doc.title, Body: doc.body}); err != nil {
			t.Fatal(err)
		}
	}
	weights, err := e.idfWeights(ctx, []string{"alpha", "rare"})
	if err != nil {
		t.Fatal(err)
	}
	wantAlpha := math.Log(1 + (3.0-2+0.5)/(2+0.5))
	wantRare := math.Log(1 + (3.0-1+0.5)/(1+0.5))
	if math.Abs(weights["alpha"]-wantAlpha) > 1e-12 || math.Abs(weights["rare"]-wantRare) > 1e-12 {
		t.Fatalf("IDF weights = %v, want alpha %.12f rare %.12f", weights, wantAlpha, wantRare)
	}
	partial := coverage([]string{"alpha", "rare"}, []string{"alpha"}, weights)
	complete := coverage([]string{"alpha", "rare"}, []string{"alpha", "rare"}, weights)
	if !(partial < complete && complete == 1) {
		t.Fatalf("coverage absent rare=%.4f present rare=%.4f, want absent lower", partial, complete)
	}
}

func TestFloorFailsOpen(t *testing.T) {
	e := floorTestEngine(t, true)
	if _, err := e.st.DB.Exec(`DROP TABLE memories_fts_vocab`); err != nil {
		t.Fatal(err)
	}
	set, err := e.Recall(context.Background(), RecallQuery{TaskHint: "concurrent recarto processes fail with SQLITE_BUSY database is locked", Branch: floorTestBranch})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Results) == 0 {
		t.Fatal("floor error hid a lexical match instead of failing open")
	}
}

func TestSelectWithinBudgetDeterministicTies(t *testing.T) {
	var records []RecallResult
	for _, id := range []string{"id-c", "id-a", "id-d", "id-b"} {
		records = append(records, RecallResult{Record: &Record{ID: id, Title: "tie"}, Score: 1})
	}
	want := []string{"id-a", "id-b", "id-c", "id-d"}
	for run := 0; run < 20; run++ {
		got, _, _ := selectWithinBudget(records, 1<<20, 10)
		if !equalStrings(resultIDs(got), want) {
			t.Fatalf("run %d tie order = %v, want %v", run, resultIDs(got), want)
		}
	}
}

func floorTestEngine(t *testing.T, disableSemantic bool) *Engine {
	t.Helper()
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if disableSemantic {
		e.SetEmbedder(nil)
	}
	fixture := readRecallFixture(t)
	wanted := map[string]bool{
		"019e9de6-15da-7ac6-a4b8-4c7c990100f8": true,
		"019eb052-584e-7bea-8f6f-6aa710987675": true,
		"019eb056-b953-78c2-86fb-256b310b4353": true,
	}
	for i := 0; i < 7; i++ {
		wanted[fixture.Memories[i].ID] = true
	}
	for _, item := range fixture.Memories {
		if !wanted[item.ID] {
			continue
		}
		if outcome, id, err := e.ImportMemory(context.Background(), item.importValue()); err != nil || outcome != ImportInserted || id != item.ID {
			t.Fatalf("ImportMemory(%s) = (%v, %q, %v)", item.ID, outcome, id, err)
		}
	}
	return e
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
