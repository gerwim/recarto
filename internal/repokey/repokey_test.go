package repokey

import "testing"

func TestHashIsDeterministicAndDistinct(t *testing.T) {
	a1 := Hash("commit-a")
	a2 := Hash("commit-a")
	b := Hash("commit-b")
	if a1 != a2 {
		t.Fatalf("Hash must be deterministic: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("distinct root commits must hash to distinct keys")
	}
	if len(a1) != 16 {
		t.Fatalf("Hash length = %d, want 16 (matches keyPattern's expectations)", len(a1))
	}
}
