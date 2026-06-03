package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestRecallStructuralBeatsLexical(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	// A: shares the query FILE (structural — strongest signal), weak words.
	structID, _, err := e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "store opens with pragmas",
		Body:   "open applies wal and busy timeout pragmas on every connection",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
		Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// B: matches the query WORDS strongly but no shared ref.
	_, _, err = e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "auth session refresh",
		Body:   "auth session refresh token rotation auth session refresh",
		Refs:   []Ref{{Type: RefFile, Value: "internal/auth/session.go"}},
		Branch: "main",
		Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		TaskHint:    "auth session refresh",              // favors B lexically
		Files:       []string{"internal/store/store.go"}, // favors A structurally
		Branch:      "main",
		TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 2 {
		t.Fatalf("expected both records, got %d", len(res))
	}
	if res[0].Record.ID != structID {
		t.Errorf("structural match should rank first; got %q (signals %+v)",
			res[0].Record.ID, res[0].Signals)
	}
}

func TestRecallDropsSuperseded(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(11_000_000)))
	ctx := context.Background()
	oldID, _, _ := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body: "we will store markdown files committed in the repo",
		Refs: []Ref{{Type: RefFile, Value: "internal/export/render.go"}}})
	// Supersede it.
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision,
		Body:         "markdown is on-demand export only, sqlite is source of truth",
		Refs:         []Ref{{Type: RefFile, Value: "internal/export/render.go"}},
		SupersedesID: oldID}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{
		Files: []string{"internal/export/render.go"}, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Record.ID == oldID {
			t.Error("superseded record should not be recalled")
		}
	}
}
