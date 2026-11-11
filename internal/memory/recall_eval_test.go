package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/embedding"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/store"
)

type recallFixture struct {
	Memories  []recallFixtureMemory `json:"memories"`
	Related   []recallFixtureQuery  `json:"related"`
	Unrelated []string              `json:"unrelated"`
}

type recallFixtureMemory struct {
	ID                 string             `json:"id"`
	Kind               string             `json:"kind"`
	Title              string             `json:"title"`
	Body               string             `json:"body"`
	Branch             string             `json:"branch"`
	Pinned             bool               `json:"pinned"`
	Confidence         float64            `json:"confidence"`
	ReinforcementCount int                `json:"reinforcement_count"`
	CreatedAt          int64              `json:"created_at"`
	Refs               []recallFixtureRef `json:"refs"`
}

type recallFixtureRef struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type recallFixtureQuery struct {
	Q   string   `json:"q"`
	Set string   `json:"set"`
	Rel []string `json:"rel"`
}

func (m recallFixtureMemory) importValue() ImportMemory {
	refs := make([]Ref, 0, len(m.Refs))
	for _, ref := range m.Refs {
		refs = append(refs, Ref{Type: RefType(ref.Type), Value: ref.Value})
	}
	return ImportMemory{
		ID: m.ID, Kind: Kind(m.Kind), Title: m.Title, Body: m.Body, Branch: m.Branch,
		Pinned: m.Pinned, Confidence: m.Confidence, ReinforcementCount: m.ReinforcementCount,
		CreatedAt: m.CreatedAt, Refs: refs, Source: "recall-eval",
	}
}

func readRecallFixture(t testing.TB) recallFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/recall_eval.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture recallFixture
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func importRecallFixture(t testing.TB, e *Engine, memories []recallFixtureMemory) {
	t.Helper()
	ctx := context.Background()
	for _, item := range memories {
		outcome, id, err := e.ImportMemory(ctx, item.importValue())
		if err != nil || outcome != ImportInserted || id != item.ID {
			t.Fatalf("ImportMemory(%s) = (%v, %q, %v), want inserted with same id", item.ID, outcome, id, err)
		}
	}
}

func TestRecallQualityGate(t *testing.T) {
	fixture := readRecallFixture(t)
	st := newTestStore(t)
	eng := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_790_000_000_000)))
	importRecallFixture(t, eng, fixture.Memories)
	var count int
	if err := st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 73 || len(fixture.Memories) != 73 {
		t.Fatalf("fixture/store rows = %d/%d, want 73", len(fixture.Memories), count)
	}

	hybrid := evaluateFixture(t, eng, fixture)
	t.Logf("hybrid + floor: lexical R@5=%.3f Hit@5=%.3f; paraphrase R@5=%.3f Hit@5=%.3f; long R@5=%.3f; unrelated empty=%d/%d",
		hybrid.lexical.rAt5, hybrid.lexical.hitAt5, hybrid.paraphrase.rAt5, hybrid.paraphrase.hitAt5,
		hybrid.long.rAt5, hybrid.unrelatedEmpty, len(fixture.Unrelated))
	if hybrid.lexical.rAt5 < 0.88 || hybrid.lexical.hitAt5 < 0.92 {
		t.Errorf("lexical quality below floor: %+v", hybrid.lexical)
	}
	if hybrid.paraphrase.rAt5 < 0.72 || hybrid.paraphrase.hitAt5 < 0.82 {
		t.Errorf("paraphrase quality below floor: %+v", hybrid.paraphrase)
	}
	if hybrid.long.rAt5 < 0.90 {
		t.Errorf("long-query R@5 = %.3f, want >= 0.90", hybrid.long.rAt5)
	}
	if hybrid.unrelatedEmpty < 38 {
		t.Errorf("unrelated empty = %d/%d, want at least 38", hybrid.unrelatedEmpty, len(fixture.Unrelated))
	}

	eng.SetEmbedder(nil)
	lexicalOnly := evaluateCategory(t, eng, fixture.Related, "paraphrase")
	t.Logf("lexical-only paraphrase: R@5=%.3f Hit@5=%.3f", lexicalOnly.rAt5, lexicalOnly.hitAt5)
	if hybrid.paraphrase.rAt5-lexicalOnly.rAt5 < 0.08 {
		t.Errorf("semantic lift = %.3f, want >= 0.08", hybrid.paraphrase.rAt5-lexicalOnly.rAt5)
	}

	eng.SetEmbedder(embedding.Default())
	eng.floor = false
	floorOff := evaluateFixture(t, eng, fixture)
	t.Logf("hybrid floor off: paraphrase R@5=%.3f Hit@5=%.3f; unrelated empty=%d/%d",
		floorOff.paraphrase.rAt5, floorOff.paraphrase.hitAt5, floorOff.unrelatedEmpty, len(fixture.Unrelated))
	if floorOff.unrelatedEmpty != 0 {
		t.Errorf("floor-off unrelated empty = %d, want 0", floorOff.unrelatedEmpty)
	}
	if floorOff.paraphrase.rAt5 < hybrid.paraphrase.rAt5 {
		t.Errorf("floor-off paraphrase R@5 %.3f below floor-on %.3f", floorOff.paraphrase.rAt5, hybrid.paraphrase.rAt5)
	}
}

type fixtureMetrics struct {
	lexical, paraphrase, long queryMetrics
	unrelatedEmpty            int
}

type queryMetrics struct {
	rAt5, hitAt5 float64
	queries      int
}

func evaluateFixture(t testing.TB, e *Engine, fixture recallFixture) fixtureMetrics {
	return fixtureMetrics{
		lexical:        evaluateCategory(t, e, fixture.Related, "lexical"),
		paraphrase:     evaluateCategory(t, e, fixture.Related, "paraphrase"),
		long:           evaluateCategory(t, e, fixture.Related, "long"),
		unrelatedEmpty: countUnrelatedEmpty(t, e, fixture.Unrelated),
	}
}

func evaluateCategory(t testing.TB, e *Engine, queries []recallFixtureQuery, category string) queryMetrics {
	t.Helper()
	var metrics queryMetrics
	for _, query := range queries {
		if query.Set != category {
			continue
		}
		set, err := e.Recall(context.Background(), RecallQuery{TaskHint: query.Q, Branch: "master", Limit: 50, TokenBudget: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(set.Results, func(i, j int) bool {
			if set.Results[i].Score != set.Results[j].Score {
				return set.Results[i].Score > set.Results[j].Score
			}
			return set.Results[i].Record.ID < set.Results[j].Record.ID
		})
		relevant := make(map[string]bool, len(query.Rel))
		for _, id := range query.Rel {
			relevant[id] = true
		}
		found := 0
		for i := 0; i < min(5, len(set.Results)); i++ {
			if relevant[set.Results[i].Record.ID] {
				found++
			}
		}
		if len(relevant) > 0 {
			metrics.rAt5 += float64(found) / float64(len(relevant))
		}
		if found > 0 {
			metrics.hitAt5++
		}
		metrics.queries++
	}
	if metrics.queries > 0 {
		metrics.rAt5 /= float64(metrics.queries)
		metrics.hitAt5 /= float64(metrics.queries)
	}
	return metrics
}

func countUnrelatedEmpty(t testing.TB, e *Engine, queries []string) int {
	t.Helper()
	count := 0
	for _, query := range queries {
		set, err := e.Recall(context.Background(), RecallQuery{TaskHint: query, Branch: "master", Limit: 50, TokenBudget: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		if len(set.Results) == 0 {
			count++
		}
	}
	return count
}

func BenchmarkRecallText(b *testing.B) {
	fixture := readRecallFixture(b)
	for _, target := range []int{73, 1000} {
		for _, semantic := range []bool{true, false} {
			name := "semantic-on"
			if !semantic {
				name = "RECARTO_SEMANTIC=0"
			}
			b.Run(fmt.Sprintf("memories-%d/%s", target, name), func(b *testing.B) {
				if semantic {
					b.Setenv("RECARTO_SEMANTIC", "1")
				} else {
					b.Setenv("RECARTO_SEMANTIC", "0")
				}
				st, err := store.Open(filepath.Join(b.TempDir(), "store.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer st.Close()
				e := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_790_000_000_000)))
				importRecallFixture(b, e, fixture.Memories)
				for i := len(fixture.Memories); i < target; i++ {
					text := fmt.Sprintf("item%04d token%04d key%04d value%04d alpha beta gamma", i, i, i, i)
					if _, _, _, err := e.Record(context.Background(), NewMemory{Kind: KindLearning, Title: fmt.Sprintf("Recall benchmark entry %04d", i), Body: text}); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := e.Recall(context.Background(), RecallQuery{TaskHint: "recarto memory recall", Branch: "master"}); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := e.Recall(context.Background(), RecallQuery{TaskHint: "recarto memory recall", Branch: "master"}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
