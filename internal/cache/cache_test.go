package cache

import (
	"path/filepath"
	"testing"
)

func TestStorePathHonorsEnvOverride(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", "/custom/cache")
	got, err := StorePath("abc123def456789a")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/cache", "abc123def456789a", "store.db")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestStorePathDefaultUsesOSCache(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", "")
	got, err := StorePath("repo01")
	if err != nil {
		t.Fatal(err)
	}
	// Must end with recarto/<repo-id>/store.db regardless of OS cache root.
	wantSuffix := filepath.Join("recarto", "repo01", "store.db")
	if filepath.Base(got) != "store.db" || !hasSuffix(got, wantSuffix) {
		t.Fatalf("StorePath = %q, want suffix %q", got, wantSuffix)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
