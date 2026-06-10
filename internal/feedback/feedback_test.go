package feedback

import (
	"context"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestOpenIsRepoIndependentAndPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", dir)
	ctx := context.Background()

	b, err := Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	// The store lives at the reserved id under the cache dir, regardless of cwd.
	want := filepath.Join(dir, StoreID, "store.db")
	if b.Store.Path() != want {
		t.Fatalf("store path = %q, want %q", b.Store.Path(), want)
	}
	if _, _, _, err := b.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindFeedback, Title: "t", Body: "b", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the report persists in the same global store.
	b2, err := Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	recs, err := b2.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("feedback records = %d, want 1", len(recs))
	}
}
