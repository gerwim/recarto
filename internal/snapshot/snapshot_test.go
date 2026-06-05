package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
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
	if _, _, err := src.Record(ctx, memory.NewMemory{
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
