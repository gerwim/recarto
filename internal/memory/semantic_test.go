package memory

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/embedding"
	"github.com/gerwim/recarto/internal/store"
)

type countingEmbedder struct {
	mu    sync.Mutex
	id    string
	calls []string
	vec   []float32
}

func (f *countingEmbedder) ID() string { return f.id }
func (f *countingEmbedder) Embed(text string) []float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	return append([]float32(nil), f.vec...)
}
func (f *countingEmbedder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestSemanticBackfillPersistsAndReuses(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	fake := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(fake)
	ctx := context.Background()
	for _, memory := range []NewMemory{
		{Kind: KindLearning, Title: "wal writer alpha", Body: "sqlite concurrency alpha"},
		{Kind: KindLearning, Title: "wal writer beta", Body: "wal connection beta"},
		{Kind: KindLearning, Title: "wal writer gamma", Body: "database writer gamma"},
	} {
		if _, _, _, err := e.Record(ctx, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memory_vectors`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("memory_vectors rows = %d, want 3", count)
	}
	var mismatches int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memories m JOIN memory_vectors v ON v.memory_id=m.id WHERE v.model<>? OR v.source_at<>m.updated_at`, fake.ID()).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("stale persisted vectors = %d, want 0", mismatches)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	if got := fake.count(); got != 5 {
		t.Fatalf("Embed calls after warm recall = %d, want 5 (three docs + two queries)", got)
	}
}

type emptyDocumentEmbedder struct{ calls int }

func (*emptyDocumentEmbedder) ID() string { return "empty-doc-v1" }
func (e *emptyDocumentEmbedder) Embed(text string) []float32 {
	e.calls++
	if strings.Contains(text, "\n") {
		return nil
	}
	return []float32{1, 0}
}

func TestSemanticEmptyDocumentVectorIsNotRetried(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	fake := &emptyDocumentEmbedder{}
	e.SetEmbedder(fake)
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "zero vector doc", Body: "empty embedding"}); err != nil {
		t.Fatal(err)
	}
	query := RecallQuery{TaskHint: "zero vector doc"}
	if _, err := e.Recall(ctx, query); err != nil {
		t.Fatal(err)
	}
	var storageType string
	var size int
	if err := e.st.DB.QueryRow(`SELECT typeof(vec), length(vec) FROM memory_vectors`).Scan(&storageType, &size); err != nil {
		t.Fatal(err)
	}
	if storageType != "blob" || size != 0 {
		t.Fatalf("empty vector storage = (%s, %d), want (blob, 0)", storageType, size)
	}
	if _, err := e.Recall(ctx, query); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 3 {
		t.Fatalf("Embed calls = %d, want query + one empty doc + query", fake.calls)
	}
}

func TestSemanticReembedsChangedMemory(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	e := newTestEngine(t, clk)
	fake := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(fake)
	ctx := context.Background()
	nm := NewMemory{Kind: KindLearning, Title: "wal writer changed", Body: "sqlite concurrency work"}
	id, _, _, err := e.Record(ctx, nm)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "wal writer unchanged", Body: "query pool model embeddings"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	if got := fake.count(); got != 3 {
		t.Fatalf("initial Embed calls = %d, want 3", got)
	}
	clk.Advance(time.Second)
	if got, reinforced, _, err := e.Record(ctx, nm); err != nil || !reinforced || got != id {
		t.Fatalf("Record reinforce = (%q, %v, %v), want id %q reinforced", got, reinforced, err, id)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	if got := fake.count(); got != 5 {
		t.Fatalf("Embed calls after one changed memory = %d, want 5 (one doc + one query)", got)
	}
}

func TestSemanticModelChangeReembeds(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	ctx := context.Background()
	for _, memory := range []NewMemory{
		{Kind: KindLearning, Title: "wal writer one", Body: "sqlite concurrency one"},
		{Kind: KindLearning, Title: "wal writer two", Body: "wal connection two"},
	} {
		if _, _, _, err := e.Record(ctx, memory); err != nil {
			t.Fatal(err)
		}
	}
	first := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(first)
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	second := &countingEmbedder{id: "fake-v2", vec: []float32{0, 1}}
	e.SetEmbedder(second)
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "wal writer sqlite"}); err != nil {
		t.Fatal(err)
	}
	if got := second.count(); got != 3 {
		t.Fatalf("Embed calls after model change = %d, want two docs + query", got)
	}
	var wrong int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memory_vectors WHERE model<>?`, second.ID()).Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if wrong != 0 {
		t.Fatalf("vectors with old model = %d, want 0", wrong)
	}
}

func TestVectorsCascadeOnForget(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	e.SetEmbedder(&countingEmbedder{id: "fake-v1", vec: []float32{1, 0}})
	ctx := context.Background()
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "vector forget alpha", Body: "alpha beta gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "vector forget alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Forget(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	assertNoVector(t, e, id)
	id, _, _, err = e.Record(ctx, NewMemory{Kind: KindLearning, Title: "vector gc delta", Body: "delta epsilon zeta"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "vector gc delta"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error { return e.deleteMemoryRows(ctx, c, []string{id}) }); err != nil {
		t.Fatal(err)
	}
	assertNoVector(t, e, id)
}

func TestSemanticFindsZeroOverlapParaphrase(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	e.floor = false
	e.SetEmbedder(embedding.Default())
	ctx := context.Background()
	memories := []NewMemory{
		{Kind: KindLearning, Title: "WAL writers collide", Body: "sqlite returns SQLITE_BUSY when two programs write simultaneously"},
		{Kind: KindLearning, Title: "Session start ranks modules", Body: "the map ranks modules by file count and demotes tests"},
		{Kind: KindLearning, Title: "Claude hooks pin PowerShell", Body: "hooks pin the PowerShell shell on Windows installs"},
		{Kind: KindLearning, Title: "Generated files demoted", Body: "search results demote generated files such as protobuf output"},
	}
	var expected string
	for i, nm := range memories {
		id, _, _, err := e.Record(ctx, nm)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			expected = id
		}
	}
	set, err := e.Recall(ctx, RecallQuery{TaskHint: "concurrent processes fail with a database lock error", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Results) == 0 || set.Results[0].Record.ID != expected {
		t.Fatalf("top semantic result = %v, want WAL memory %s", set.Results, expected)
	}
	if !strings.Contains(strings.Join(set.Results[0].MatchedOn(), ","), "semantic") {
		t.Fatalf("MatchedOn = %v, want semantic", set.Results[0].MatchedOn())
	}
	// With the balanced floor enabled this low cosine and zero lexical coverage
	// intentionally fail the gate; the owner accepted this trade-off.
}

func TestSessionStartBaselineNeverEmbeds(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	fake := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(fake)
	ctx := context.Background()
	for _, memory := range []NewMemory{
		{Kind: KindLearning, Title: "baseline alpha", Body: "branch alpha"},
		{Kind: KindLearning, Title: "baseline beta", Body: "branch beta"},
		{Kind: KindLearning, Title: "baseline gamma", Body: "branch gamma"},
	} {
		if _, _, _, err := e.Record(ctx, memory); err != nil {
			t.Fatal(err)
		}
	}
	withEmbedder, err := e.Recall(ctx, RecallQuery{Baseline: true, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if fake.count() != 0 {
		t.Fatalf("baseline Embed calls = %d, want 0", fake.count())
	}
	e.SetEmbedder(nil)
	withoutEmbedder, err := e.Recall(ctx, RecallQuery{Baseline: true, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameResultIDs(withEmbedder.Results, withoutEmbedder.Results) {
		t.Fatalf("baseline result order changed: with=%v without=%v", resultIDs(withEmbedder.Results), resultIDs(withoutEmbedder.Results))
	}
}

func TestLexicalOnlyNeverEmbeds(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	fake := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(fake)
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "augment lexical bug", Body: "database lock while compiling"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "database lock", Kind: KindBug, LexicalOnly: true}); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 0 {
		t.Fatalf("LexicalOnly Embed calls = %d, want 0", fake.count())
	}
}

func TestSemanticDisabledByEnv(t *testing.T) {
	t.Setenv("RECARTO_SEMANTIC", "0")
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if e.emb != nil {
		t.Fatalf("engine embedder = %T, want nil", e.emb)
	}
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "disabled semantic test", Body: "lexical only"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Recall(ctx, RecallQuery{TaskHint: "disabled semantic test"}); err != nil {
		t.Fatal(err)
	}
}

func TestKindFilterScopesVectorScan(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	fake := &countingEmbedder{id: "fake-v1", vec: []float32{1, 0}}
	e.SetEmbedder(fake)
	ctx := context.Background()
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "shared keyword learning", Body: "shared vector entry"}); err != nil {
		t.Fatal(err)
	}
	bugID, _, _, err := e.Record(ctx, NewMemory{Kind: KindBug, Title: "shared keyword bug", Body: "shared vector entry"})
	if err != nil {
		t.Fatal(err)
	}
	set, err := e.Recall(ctx, RecallQuery{TaskHint: "shared keyword", Kind: KindBug})
	if err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 {
		t.Fatalf("Embed calls = %d, want query + one bug document", fake.count())
	}
	for _, result := range set.Results {
		if result.Record.Kind != KindBug {
			t.Errorf("result kind = %s, want bug", result.Record.Kind)
		}
	}
	var vectors int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memory_vectors v JOIN memories m ON m.id=v.memory_id WHERE m.kind=?`, KindBug).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 1 || len(set.Results) == 0 || set.Results[0].Record.ID != bugID {
		t.Fatalf("bug vectors=%d results=%v, want only bug %s", vectors, resultIDsFromSet(set), bugID)
	}
}

func assertNoVector(t *testing.T, e *Engine, id string) {
	t.Helper()
	var count int
	if err := e.st.DB.QueryRow(`SELECT count(*) FROM memory_vectors WHERE memory_id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("memory vector rows for %s = %d, want 0", id, count)
	}
}

func sameResultIDs(a, b []RecallResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Record.ID != b[i].Record.ID {
			return false
		}
	}
	return true
}

func resultIDs(results []RecallResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Record.ID)
	}
	return ids
}

func resultIDsFromSet(set RecallResultSet) []string { return resultIDs(set.Results) }
