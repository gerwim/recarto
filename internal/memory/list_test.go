package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestListFiltersByKind(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(5_000_000)))
	ctx := context.Background()

	if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning,
		Body: "use wal mode and busy_timeout for cross process writers"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Record(ctx, NewMemory{Kind: KindBug,
		Body: "auth token refresh leaks a goroutine on error return"}); err != nil {
		t.Fatal(err)
	}

	all, err := e.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List() all active = %d, want 2", len(all))
	}

	bugs, err := e.List(ctx, ListFilter{Kind: KindBug})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Kind != KindBug {
		t.Fatalf("List(kind=bug) = %+v, want exactly 1 bug", bugs)
	}
	if bugs[0].ID == "" {
		t.Error("listed record missing id")
	}
}

// TestListBranchFilter pins the `branch IS NULL OR branch = ?` semantics: a
// branch-filtered list must include repo-global (NULL) memories and the matching
// branch, but exclude other branches.
func TestListBranchFilter(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(6_000_000)))
	ctx := context.Background()

	mustRecord := func(branch, body string) {
		t.Helper()
		if _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Branch: branch, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	mustRecord("", "repo global note about wal journaling mode")
	mustRecord("main", "main branch note about busy timeout tuning")
	mustRecord("feature", "feature branch note about goroutine leak handling")

	got, err := e.List(ctx, ListFilter{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Repo-global (NULL) + main, but never feature.
	if len(got) != 2 {
		t.Fatalf("List(branch=main) returned %d records, want 2 (global + main)", len(got))
	}
	for _, r := range got {
		if r.Branch == "feature" {
			t.Errorf("branch filter leaked a feature-branch record: %+v", r)
		}
	}
}
