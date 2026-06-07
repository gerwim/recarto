package mcp

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestReportRecartoIssueWritesToFeedbackStore(t *testing.T) {
	a := newTestApp(t, "main") // sets RECARTO_CACHE_DIR; resolves a.Git via fakeGit
	ctx := context.Background()

	out, err := reportRecartoIssue(ctx, a, ReportRecartoIssueInput{
		SignalType: "error",
		Title:      "search_content rejected query param",
		Detail:     "Called search_content with `query`; it errored with unexpected property.",
		Tool:       "search_content",
		Severity:   "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ID == "" {
		t.Fatal("expected an id")
	}

	// It must NOT land in the per-repo store.
	perRepo, err := a.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(perRepo) != 0 {
		t.Fatalf("feedback leaked into the per-repo store: %d rows", len(perRepo))
	}

	// It MUST land in the global feedback store.
	b, err := feedback.Open(a.Clock)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	recs, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("feedback store rows = %d, want 1", len(recs))
	}
	if st, _ := recs[0].Extra["signal_type"].(string); st != "error" {
		t.Fatalf("signal_type = %q, want error", st)
	}

	// A near-identical second report reinforces rather than duplicating.
	out2, err := reportRecartoIssue(ctx, a, ReportRecartoIssueInput{
		SignalType: "error",
		Title:      "search_content rejected query param",
		Detail:     "Called search_content with `query`; it errored with unexpected property.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Reinforced || out2.ID != out.ID {
		t.Fatalf("second report = (id=%s reinforced=%v), want reinforce of %s", out2.ID, out2.Reinforced, out.ID)
	}
}

func TestReportRecartoIssueRequiresFields(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	cases := []ReportRecartoIssueInput{
		{Title: "t", Detail: "d"},                  // missing signal_type
		{SignalType: "error", Detail: "d"},          // missing title
		{SignalType: "error", Title: "t"},           // missing detail
	}
	for i, in := range cases {
		if _, err := reportRecartoIssue(ctx, a, in); err == nil {
			t.Errorf("case %d: expected error for missing required field, got nil", i)
		}
	}
}
