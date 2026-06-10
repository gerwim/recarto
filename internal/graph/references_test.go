package graph

import (
	"context"
	"testing"
)

// indexInto does not run cross-file resolve, so the call edge below stays
// unresolved (dst_node_id NULL, dst_name "Target") — exactly the case the
// dst_name fallback must still find, mirroring Implementations.
func TestReferencesFindsUnresolvedCallEdgesByName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "b.go", "package p\nfunc Caller() { Target() }\n")

	refs, meta, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Resolved {
		t.Error("Resolved = false, want true (Target is an indexed node)")
	}
	var edgeHit bool
	for _, r := range refs {
		if r.Path == "b.go" && r.Line == 2 && r.Kind == "calls" {
			edgeHit = true
			if r.From != "Caller" {
				t.Errorf("From = %q, want Caller", r.From)
			}
		}
	}
	if !edgeHit {
		t.Fatalf("no calls-edge reference at b.go:2 in %+v", refs)
	}
}

func TestReferencesTextFallbackAndDedup(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "b.go", "package p\nfunc Caller() { Target() }\n")
	writeFile(t, wt, "README.md", "Target is documented here. TargetFactory is not it.\n")

	refs, _, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	var textHit bool
	siteCount := 0
	for _, r := range refs {
		if r.Path == "README.md" && r.Kind == "text" {
			textHit = true
		}
		if r.Path == "b.go" && r.Line == 2 {
			siteCount++ // the calls edge must suppress the duplicate text match
		}
	}
	if !textHit {
		t.Errorf("no text reference from README.md in %+v", refs)
	}
	if siteCount != 1 {
		t.Errorf("b.go:2 appears %d times, want 1 (edge hit dedupes the text match)", siteCount)
	}
}

func TestReferencesFlagsTestFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "a_test.go", "package p\nfunc helper() { Target() }\n")

	refs, _, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Path == "a_test.go" && !r.IsTest {
			t.Errorf("reference in a_test.go not flagged IsTest: %+v", r)
		}
	}
}
