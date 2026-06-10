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

// TestReferencesDottedInputNarrowsReceiver checks that dotted input (e.g.
// "Cache.Get") constrains the dst_name arm to matching receiver so that
// unrelated same-named calls (x.Get() on an unrelated receiver) are NOT
// returned as edge hits, while a proper Cache.Get() call IS returned.
func TestReferencesDottedInputNarrowsReceiver(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	// a.go: defines Cache and its Get method
	indexInto(t, e, wt, "a.go", "package p\ntype Cache struct{}\nfunc (c Cache) Get() {}\n")
	// b.go: calls Cache.Get() — matches Cache receiver
	indexInto(t, e, wt, "b.go", "package p\nfunc caller() { Cache.Get() }\n")
	// c.go: calls x.Get() on an unrelated receiver
	indexInto(t, e, wt, "c.go", "package p\nfunc other() { x.Get() }\n")

	refs, _, err := e.References(ctx, wt, "Cache.Get", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range refs {
		if r.Kind == "calls" && r.Path == "c.go" {
			t.Errorf("edge hit from c.go included — unrelated receiver should be filtered: %+v", r)
		}
	}
	var bHit bool
	for _, r := range refs {
		if r.Kind == "calls" && r.Path == "b.go" {
			bHit = true
		}
	}
	if !bHit {
		t.Errorf("no calls-edge hit at b.go; refs = %+v", refs)
	}
}

// TestReferencesTruncatedHonestAtExactLimit pins the reviewer's scenario:
// edge hits == limit but an unseen text ref exists → Truncated must be true.
func TestReferencesTruncatedHonestAtExactLimit(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()

	// Two callers → 2 edge refs
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	indexInto(t, e, wt, "b.go", "package p\nfunc Caller1() { Target() }\n")
	indexInto(t, e, wt, "c.go", "package p\nfunc Caller2() { Target() }\n")
	// README has a text ref that the edge query won't find
	writeFile(t, wt, "README.md", "Target is also mentioned here.\n")

	refs, meta, err := e.References(ctx, wt, "Target", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Errorf("len(refs) = %d, want 2", len(refs))
	}
	if !meta.Truncated {
		t.Errorf("Truncated = false, want true (unseen text ref exists beyond the limit)")
	}
}

// TestReferencesWordBoundaryPrecision asserts that TargetFactory on its own
// line does NOT produce a text reference (the word-boundary regex must not
// match TargetFactory when searching for Target).
func TestReferencesWordBoundaryPrecision(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc Target() {}\n")
	// README has Target on one line and TargetFactory on a separate line
	writeFile(t, wt, "README.md", "Target is documented here.\nTargetFactory is not it.\n")

	refs, _, err := e.References(ctx, wt, "Target", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Path == "README.md" && r.Kind == "text" && r.Line == 2 {
			t.Errorf("text hit at README.md:2 (TargetFactory line) — word-boundary regex must not match: %+v", r)
		}
	}
}
