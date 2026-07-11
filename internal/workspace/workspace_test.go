package workspace

import (
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestStoreID(t *testing.T) {
	id := StoreID("acme")
	if !strings.HasPrefix(id, StorePrefix) {
		t.Errorf("StoreID = %q, want %q prefix", id, StorePrefix)
	}
	if id != StoreID("acme") {
		t.Error("StoreID must be deterministic")
	}
	if id == StoreID("other") {
		t.Error("different names must derive different store ids")
	}
}

func TestOpenDisabledWithoutEnv(t *testing.T) {
	t.Setenv(EnvVar, "")
	b, err := Open(nil)
	if err != nil || b != nil {
		t.Fatalf("Open with no %s = (%v, %v), want (nil, nil)", EnvVar, b, err)
	}
}

func TestOpenRecordsAndRecalls(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv(EnvVar, "acme")
	// The host running the tests may itself use Recarto with sync configured
	// (e.g. a ForgeBoard session); this test asserts the sync-LESS shape.
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")
	b, err := Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("Open returned nil bundle with env set")
	}
	defer func() { _ = b.Close() }()
	if b.Name != "acme" {
		t.Errorf("Name = %q, want acme", b.Name)
	}
	if b.Sync != nil {
		t.Error("Sync must be nil without RECARTO_SYNC_URL/KEY")
	}

	ctx := context.Background()
	id, _, _, err := b.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindPreference, Title: "CliWrap", Body: "always use CliWrap for process execution",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := b.Engine.GetByID(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("GetByID = (%v, %v), want the recorded memory", rec, err)
	}
}
