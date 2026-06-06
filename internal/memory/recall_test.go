package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func TestMatchedOnDominantSignal(t *testing.T) {
	r := RecallResult{Signals: map[string]float64{
		"structural": 1.0, "lexical": 0, "recency": 0, "reinforcement": 0, "branch": 0,
	}}
	if got := r.MatchedOn(); len(got) != 1 || got[0] != "structural" {
		t.Fatalf("MatchedOn = %v, want [structural]", got)
	}

	none := RecallResult{Signals: map[string]float64{}}
	if got := none.MatchedOn(); got != nil {
		t.Fatalf("MatchedOn (no signals) = %v, want nil", got)
	}
}

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

// TestRecallPinnedSurvivesKindFilter guards C1: a kind-filtered recall must
// still surface pinned do_not_repeat memories (spec §9.2 — pins are always
// included), even though their kind differs from the requested kind.
func TestRecallPinnedSurvivesKindFilter(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(30_000_000)))
	ctx := context.Background()

	pinID, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat,
		Body: "never disable the busy_timeout pragma on the store connection"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug,
		Body: "nil deref in the recall budgeter when scored is empty",
		Refs: []Ref{{Type: RefFile, Value: "internal/memory/recall.go"}}}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		Kind:  KindBug, // restrict to bugs ...
		Files: []string{"internal/memory/recall.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawPin bool
	for _, r := range res {
		if r.Record.ID == pinID { // ... the pin must still appear
			sawPin = true
		}
	}
	if !sawPin {
		t.Error("pinned do_not_repeat must survive a kind-filtered recall (spec §9.2)")
	}
}

// TestRecallMissWhenBudgetExcludesAll guards I2: recall_misses must count a
// recall that found candidates but returned nothing because the budget excluded
// them — not only the empty-candidate case.
func TestRecallMissWhenBudgetExcludesAll(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(31_000_000)))
	ctx := context.Background()

	big := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi"
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Body: big,
		Refs: []Ref{{Type: RefFile, Value: "internal/memory/recall.go"}}}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		Files:       []string{"internal/memory/recall.go"},
		TokenBudget: 1, // far below the candidate's token cost
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("expected empty result under budget=1, got %d", len(res))
	}
	misses, _ := ledger.New(e.st).Get(ctx, "lifetime", "recall_misses")
	if misses != 1 {
		t.Errorf("recall_misses = %d, want 1 (candidate existed but budget excluded it)", misses)
	}
}

// TestRecallSynonymBridging locks in §9.3: a query using an abbreviation ("db")
// recalls a memory whose body used the full word ("database"), because write-
// time synonym expansion indexed both forms into search_text.
func TestRecallSynonymBridging(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(32_000_000)))
	ctx := context.Background()
	id, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Title: "pooling",
		Body:  "reuse the database connection pool instead of dialing per request"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{TaskHint: "db"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range res {
		if r.Record.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("query 'db' should recall a 'database' memory via synonym expansion")
	}
}

func TestBaselineRecallSurfacesCerebrumKinds(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	for _, nm := range []NewMemory{
		{Kind: KindPreference, Title: "pref", Body: "use tabs not spaces here", Branch: "main", Source: "cli"},
		{Kind: KindLearning, Title: "learn", Body: "store lives outside the worktree", Branch: "main", Source: "cli"},
		{Kind: KindDecision, Title: "decide", Body: "chose sqlite over flat files", Branch: "main", Source: "cli"},
		{Kind: KindAction, Title: "act", Body: "ran the formatter once today", Branch: "main", Source: "cli"},
	} {
		if _, _, err := e.Record(ctx, nm); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	got := map[Kind]bool{}
	for _, r := range res {
		got[r.Record.Kind] = true
	}
	for _, k := range []Kind{KindPreference, KindLearning, KindDecision} {
		if !got[k] {
			t.Errorf("baseline recall missing kind %s", k)
		}
	}
	if got[KindAction] {
		t.Errorf("baseline recall must exclude action (ephemeral)")
	}
}

func TestBaselineRecallExcludesBug(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "keep", Body: "a durable learning fact", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "boom", Body: "npe in the request handler", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Record.Kind == KindBug {
			t.Errorf("baseline recall must exclude bug, got %q", r.Record.Title)
		}
	}
}

func TestBaselineRecallBranchVisible(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "global", Body: "alpha unique global one", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "onbranch", Body: "beta unique feature two", Branch: "feature", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "otherbranch", Body: "gamma unique main three", Branch: "main", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "feature", Baseline: true, TokenBudget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, r := range res {
		titles[r.Record.Title] = true
	}
	if !titles["global"] || !titles["onbranch"] {
		t.Errorf("want global+onbranch visible on feature branch; got %v", titles)
	}
	if titles["otherbranch"] {
		t.Errorf("main-branch memory leaked onto feature branch")
	}
}

func TestBaselineRecallRespectsBudgetAndPinsFirst(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(10_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()
	pinID, _, err := e.Record(ctx, NewMemory{Kind: KindDoNotRepeat, Title: "never", Body: "never hardcode secrets in source", Branch: "main", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf("decision number %d with a fairly long body to consume token budget quickly", i)
		if _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision, Title: fmt.Sprintf("d%d", i), Body: body, Branch: "main", Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 20})
	if err != nil {
		t.Fatal(err)
	}
	hasPin := false
	for _, r := range res {
		if r.Record.ID == pinID {
			hasPin = true
		}
	}
	if !hasPin {
		t.Errorf("pinned do_not_repeat must be included regardless of budget")
	}
	if len(res) > 3 {
		t.Errorf("budget not enforced: got %d results", len(res))
	}
}

func TestBaselineRecallIncludesOpenTodo(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(5_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	todoID, _, err := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "finish the parser", Body: "finish the parser work", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	doneID, _, _ := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "already finished", Body: "already finished work", Branch: "main", Source: "cli",
	})
	if _, err := e.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	results, err := e.Recall(ctx, RecallQuery{Branch: "main", Baseline: true, TokenBudget: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var sawOpen, sawDone bool
	for _, r := range results {
		switch r.Record.ID {
		case todoID:
			sawOpen = true
		case doneID:
			sawDone = true
		}
	}
	if !sawOpen {
		t.Error("open todo missing from session-start baseline recall")
	}
	if sawDone {
		t.Error("done todo must not appear in baseline recall")
	}
}

func TestRecallStampsLastUsedEpoch(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(12_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id, _, err := e.Record(ctx, NewMemory{
		Kind:   KindLearning,
		Title:  "store wal pragmas",
		Body:   "open applies wal and busy timeout pragmas on every connection",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pretend we are at epoch 7.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaInt(ctx, c, metaActivityEpoch, 7)
	}); err != nil {
		t.Fatal(err)
	}

	res, err := e.Recall(ctx, RecallQuery{
		Files:       []string{"internal/store/store.go"},
		Branch:      "main",
		TokenBudget: 100000,
	})
	if err != nil || len(res) == 0 {
		t.Fatalf("recall returned %d results, err=%v", len(res), err)
	}

	var epoch, count int64
	if err := e.st.DB.QueryRowContext(ctx,
		`SELECT last_used_epoch, recall_count FROM memories WHERE id=?`, id).
		Scan(&epoch, &count); err != nil {
		t.Fatal(err)
	}
	if epoch != 7 {
		t.Errorf("last_used_epoch = %d, want 7", epoch)
	}
	if count != 1 {
		t.Errorf("recall_count = %d, want 1", count)
	}
}
