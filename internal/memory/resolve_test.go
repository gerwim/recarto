package memory

import (
	"context"
	"testing"
)

// fakeResolver returns the refs whose Value is in the dangling set.
type fakeResolver struct{ dangling map[string]bool }

func (f fakeResolver) Dangling(_ context.Context, _ string, refs []Ref) ([]Ref, error) {
	var out []Ref
	for _, r := range refs {
		if f.dangling[r.Value] {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestCodeRefsOfFiltersTags(t *testing.T) {
	refs := []Ref{
		{Type: RefFile, Value: "a.go"},
		{Type: RefTag, Value: "perf"},
		{Type: RefSymbol, Value: "Foo"},
	}
	got := codeRefsOf(refs)
	if len(got) != 2 {
		t.Fatalf("want 2 code refs, got %d: %v", len(got), got)
	}
}

func TestCaveatFormat(t *testing.T) {
	r := RecallResult{
		Record:       &Record{Branch: "feat/x"},
		DanglingRefs: []Ref{{Type: RefFile, Value: "internal/foo.go"}},
	}
	c := r.Caveat()
	if c == "" || !contains(c, "feat/x") || !contains(c, "internal/foo.go") {
		t.Fatalf("unexpected caveat: %q", c)
	}
	empty := RecallResult{Record: &Record{}}
	if empty.Caveat() != "" {
		t.Fatalf("want empty caveat for no dangling refs, got %q", empty.Caveat())
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
