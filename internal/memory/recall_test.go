package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
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

func TestRecallBudgetCapsAndPinsFirst(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(20_000_000)))
	ctx := context.Background()

	// One pinned do_not_repeat (always included, budget-exempt).
	dnrID, _, _ := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never add a redundant toolchain directive to go.mod it breaks builds",
		Refs: []Ref{{Type: RefFile, Value: "go.mod"}}})

	// Five DISTINCT same-file learnings (distinct bodies so dedup does not
	// collapse them) — each ~20 tokens, so a tiny budget must exclude several.
	bodies := []string{
		"alpha concurrency wal busy timeout immediate writer pool serialized mutex per connection",
		"bravo migration guarded user_version pragma idempotent first run lock serializes processes",
		"charlie recall structural lexical recency reinforcement branch budget greedy selection order",
		"delta synonym expansion identifier split camel snake kebab dotted token search text build",
		"echo minhash lsh band signature jaccard shingle near duplicate reinforce threshold confirm",
	}
	for _, b := range bodies {
		if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
			Body: b,
			Refs: []Ref{{Type: RefFile, Value: "go.mod"}}}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := e.Recall(ctx, RecallQuery{
		Files: []string{"go.mod"}, TokenBudget: 80})
	if err != nil {
		t.Fatal(err)
	}

	// Pin present despite the tiny budget; total recalled < 6 (some excluded).
	var sawPin bool
	for _, r := range res {
		if r.Record.ID == dnrID {
			sawPin = true
		}
	}
	if !sawPin {
		t.Error("pinned do_not_repeat must always be included")
	}
	if len(res) == 6 {
		t.Error("budget should have excluded some non-pinned learnings")
	}

	// Counters were written.
	recalled, _ := ledger.New(e.st).Get(ctx, "lifetime", "memories_recalled")
	if recalled <= 0 {
		t.Error("memories_recalled counter not incremented")
	}
}

func TestRecallMissIncrementsCounter(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(21_000_000)))
	ctx := context.Background()
	// Empty store + a query that matches nothing.
	res, err := e.Recall(ctx, RecallQuery{TaskHint: "nonexistent zzz", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no results, got %d", len(res))
	}
	misses, _ := ledger.New(e.st).Get(ctx, "lifetime", "recall_misses")
	if misses != 1 {
		t.Errorf("recall_misses = %d, want 1", misses)
	}
}
