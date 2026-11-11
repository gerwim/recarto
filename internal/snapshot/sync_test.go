package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

func TestSyncImportsOnceThenSkipsUntilChanged(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := memory.New(st, ledger.New(st), clock.Real{})

	dir := t.TempDir()
	path := filepath.Join(dir, ".recarto", "memory.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(`{"id":"x","kind":"learning","title":"T","body":"shared knowledge","confidence":1,"created_at":1}`+"\n"), 0o644)

	ins, err := SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 {
		t.Fatalf("first sync should import 1, got %d", ins)
	}
	ins, err = SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 {
		t.Fatalf("unchanged file should not re-import, got %d", ins)
	}

	// Changing the file (new record) must re-import on the next sync.
	if err := os.WriteFile(path, []byte(
		`{"id":"x","kind":"learning","title":"T","body":"shared knowledge","confidence":1,"created_at":1}`+"\n"+
			`{"id":"y","kind":"learning","title":"T2","body":"more shared knowledge","confidence":1,"created_at":2}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	ins, err = SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 1 {
		t.Fatalf("changed file should import the new record, got %d", ins)
	}
}

// TestSyncOnSessionStartDoesNotResurrectForgottenMemory reproduces the
// round-7 gap end-to-end within this package: a memory is recorded and then
// Forgotten (e.g. via the HTTP sync pull applying a remote tombstone), but a
// stale committed .recarto/memory.jsonl snapshot still lists its id. Syncing
// that snapshot must not bring the memory back.
func TestSyncOnSessionStartDoesNotResurrectForgottenMemory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := memory.New(st, ledger.New(st), clock.Real{})

	id, _, _, err := eng.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "T", Body: "shared knowledge that gets forgotten",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := eng.Forget(ctx, id, "test"); err != nil || !ok {
		t.Fatalf("Forget: ok=%v err=%v", ok, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".recarto", "memory.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(
		`{"id":"`+id+`","kind":"learning","title":"T","body":"shared knowledge that gets forgotten","confidence":1,"created_at":1}`+"\n",
	), 0o644)

	ins, err := SyncOnSessionStart(ctx, eng, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 {
		t.Fatalf("a stale snapshot listing a forgotten id must not be imported, got %d", ins)
	}
	if got, _ := eng.GetByID(ctx, id); got != nil {
		t.Fatalf("forgotten memory must not be resurrected by a stale snapshot: %+v", got)
	}
}

func TestSyncNoFileIsNoop(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := memory.New(st, ledger.New(st), clock.Real{})
	ins, err := SyncOnSessionStart(ctx, eng, st, t.TempDir())
	if err != nil {
		t.Fatalf("missing file must not error, got %v", err)
	}
	if ins != 0 {
		t.Fatalf("missing file → 0 imports, got %d", ins)
	}
}
