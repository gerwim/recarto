package ids

import (
	"sort"
	"testing"
)

func TestNewFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != 36 {
			t.Fatalf("id %q length = %d, want 36", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = true
	}
}

func TestNewIsTimeOrdered(t *testing.T) {
	const n = 200
	got := make([]string, n)
	for i := range got {
		got[i] = New()
	}
	sorted := make([]string, n)
	copy(sorted, got)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("UUIDv7 not lexicographically time-ordered at %d", i)
		}
	}
}
