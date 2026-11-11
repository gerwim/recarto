package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

func newEngine(t *testing.T) *memory.Engine {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return memory.New(st, ledger.New(st), clock.Real{})
}

func TestExportThenImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t)
	if _, _, _, err := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "L", Body: "learned about retries and backoff",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "retry.go"}},
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := Export(ctx, src, &buf)
	if err != nil || n != 1 {
		t.Fatalf("export: n=%d err=%v", n, err)
	}
	if strings.Contains(buf.String(), "last_used_epoch") || strings.Contains(buf.String(), "recall_count") ||
		strings.Contains(buf.String(), "archived_epoch") {
		t.Fatalf("snapshot must not contain local activity fields:\n%s", buf.String())
	}

	dst := newEngine(t)
	ins, rein, skip, err := Import(ctx, dst, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 || rein != 0 || skip != 0 {
		t.Fatalf("import counts: ins=%d rein=%d skip=%d", ins, rein, skip)
	}
	got, _ := dst.List(ctx, memory.ListFilter{})
	if len(got) != 1 || got[0].Title != "L" || len(got[0].Refs) != 1 {
		t.Fatalf("imported record wrong: %+v", got)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t)
	src.Record(ctx, memory.NewMemory{Kind: memory.KindDecision, Title: "D", Body: "chose stdlib flag over cobra"})
	var buf bytes.Buffer
	Export(ctx, src, &buf)
	data := buf.Bytes()

	dst := newEngine(t)
	if _, _, _, err := Import(ctx, dst, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	_, _, skip, err := Import(ctx, dst, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if skip != 1 {
		t.Fatalf("second import should skip the already-present record, got skip=%d", skip)
	}
}

// TestSnapshotRoundTripsActiveTodoNotDone pins the spec's assumption that the team
// snapshot needs no todo-specific handling: an active todo round-trips like any
// memory, while a done todo is excluded because Export only emits active rows.
func TestSnapshotRoundTripsActiveTodoNotDone(t *testing.T) {
	ctx := context.Background()
	src := newEngine(t)

	openID, _, _, err := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "open work", Body: "open work body", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	doneID, _, _, _ := src.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "finished work", Body: "finished work body", Branch: "main", Source: "cli",
	})
	if _, err := src.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Export(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), doneID) {
		t.Errorf("export should not include a done todo (only active rows export):\n%s", buf.String())
	}

	dst := newEngine(t)
	if _, _, _, err := Import(ctx, dst, &buf); err != nil {
		t.Fatal(err)
	}
	got, err := dst.ActiveTodos(ctx, "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "open work" {
		t.Fatalf("expected the open todo to round-trip; got %d rows", len(got))
	}
	_ = openID
}
