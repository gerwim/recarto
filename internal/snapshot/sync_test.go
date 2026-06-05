package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
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
