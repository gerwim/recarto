package memory

import (
	"context"
	"testing"

	"github.com/gerwim/recarto/internal/clock"
)

func recordWithRef(t *testing.T, e *Engine, kind Kind, title string, ref Ref, pinned bool) {
	t.Helper()
	if _, _, _, err := e.Record(context.Background(), NewMemory{
		Kind: kind, Title: title, Body: title + " body about " + ref.Value,
		Refs: []Ref{ref}, Pinned: pinned,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecallDemotesAllDanglingNonPinned(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindLearning, "learn-present", Ref{RefFile, "present.go"}, false)
	recordWithRef(t, e, KindLearning, "learn-absent", Ref{RefFile, "absent.go"}, false)
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true}})

	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := set.Results
	var present, absent RecallResult
	for _, r := range res {
		switch r.Record.Title {
		case "learn-present":
			present = r
		case "learn-absent":
			absent = r
		}
	}
	if present.Record == nil || absent.Record == nil {
		t.Fatalf("both memories should surface; got %d results", len(res))
	}
	if len(absent.DanglingRefs) != 1 {
		t.Fatalf("absent should have 1 dangling ref, got %d", len(absent.DanglingRefs))
	}
	if len(present.DanglingRefs) != 0 {
		t.Fatalf("present should have no dangling refs")
	}
	if absent.Score >= present.Score {
		t.Fatalf("absent (all refs dangling) should be demoted below present: %.4f vs %.4f", absent.Score, present.Score)
	}
}

func TestRecallCaveatsButDoesNotDemotePinnedOrBug(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindDoNotRepeat, "dnr-absent", Ref{RefFile, "absent.go"}, true)
	recordWithRef(t, e, KindBug, "bug-absent", Ref{RefFile, "absent.go"}, false)
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true}})

	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := set.Results
	seen := map[string]RecallResult{}
	for _, r := range res {
		seen[r.Record.Title] = r
	}
	for _, title := range []string{"dnr-absent", "bug-absent"} {
		r, ok := seen[title]
		if !ok {
			t.Fatalf("%s should still surface", title)
		}
		if len(r.DanglingRefs) != 1 {
			t.Fatalf("%s should carry the dangling-ref caveat", title)
		}
	}
}

func TestRecallPrivilegedNotNumericallyDemoted(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	// Same body/ref so base scores match; only kind differs.
	recordWithRef(t, e, KindLearning, "plain", Ref{RefFile, "absent.go"}, false)
	recordWithRef(t, e, KindBug, "privileged", Ref{RefFile, "absent.go"}, false)
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true}})

	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := set.Results
	var plain, privileged RecallResult
	for _, r := range res {
		switch r.Record.Title {
		case "plain":
			plain = r
		case "privileged":
			privileged = r
		}
	}
	if plain.Record == nil || privileged.Record == nil {
		t.Fatalf("both should surface; got %d", len(res))
	}
	// Both carry the caveat...
	if len(plain.DanglingRefs) != 1 || len(privileged.DanglingRefs) != 1 {
		t.Fatalf("both should have 1 dangling ref")
	}
	// ...but only the non-privileged one is demoted, so the bug must outrank it.
	if privileged.Score <= plain.Score {
		t.Fatalf("privileged (not demoted) should outrank demoted plain: %.4f vs %.4f", privileged.Score, plain.Score)
	}
}

func TestRecallPartialDanglingNotDemoted(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	mk := func(title string, refs ...Ref) {
		body := title + " body about partial"
		if _, _, _, err := e.Record(context.Background(), NewMemory{
			Kind: KindLearning, Title: title, Body: body, Refs: refs,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("partial", Ref{RefFile, "present.go"}, Ref{RefFile, "absent.go"})
	mk("full", Ref{RefFile, "absent.go"}, Ref{RefFile, "gone.go"})
	e.SetRefResolver(fakeResolver{dangling: map[string]bool{"absent.go": true, "gone.go": true}})

	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about partial", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := set.Results
	var partial, full RecallResult
	for _, r := range res {
		switch r.Record.Title {
		case "partial":
			partial = r
		case "full":
			full = r
		}
	}
	if partial.Record == nil || full.Record == nil {
		t.Fatalf("both should surface; got %d", len(res))
	}
	if len(partial.DanglingRefs) != 1 {
		t.Fatalf("partial should have exactly 1 dangling ref, got %d", len(partial.DanglingRefs))
	}
	if len(full.DanglingRefs) != 2 {
		t.Fatalf("full should have 2 dangling refs, got %d", len(full.DanglingRefs))
	}
	// partial is NOT demoted (not all refs dangle); full IS demoted.
	if partial.Score <= full.Score {
		t.Fatalf("partial (not demoted) should outrank full (demoted): %.4f vs %.4f", partial.Score, full.Score)
	}
}

func TestRecallNoResolverIsInert(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindLearning, "learn-absent", Ref{RefFile, "absent.go"}, false)
	set, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := set.Results
	for _, r := range res {
		if len(r.DanglingRefs) != 0 {
			t.Fatalf("no resolver → no dangling refs, got %v", r.DanglingRefs)
		}
	}
}
