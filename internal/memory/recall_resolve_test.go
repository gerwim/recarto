package memory

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func recordWithRef(t *testing.T, e *Engine, kind Kind, title string, ref Ref, pinned bool) {
	t.Helper()
	if _, _, err := e.Record(context.Background(), NewMemory{
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

	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
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

	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestRecallNoResolverIsInert(t *testing.T) {
	e := newTestEngine(t, clock.Real{})
	recordWithRef(t, e, KindLearning, "learn-absent", Ref{RefFile, "absent.go"}, false)
	res, err := e.Recall(context.Background(), RecallQuery{
		TaskHint: "body about absent", Worktree: "/wt", TokenBudget: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if len(r.DanglingRefs) != 0 {
			t.Fatalf("no resolver → no dangling refs, got %v", r.DanglingRefs)
		}
	}
}
