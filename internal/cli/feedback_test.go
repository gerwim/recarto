package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func seedFeedback(t *testing.T) string {
	t.Helper()
	b, err := feedback.Open(clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id, _, _, err := b.Engine.Record(context.Background(), memory.NewMemory{
		Kind:  memory.KindFeedback,
		Title: "search_content rejected query param",
		Body:  "search_content rejected query param\n\ndetail",
		Refs:  []memory.Ref{{Type: memory.RefTag, Value: "repo:ordia"}},
		Extra: map[string]any{"signal_type": "error", "severity": "medium"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFeedbackNoArgsDefaultsToList(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	ctx := context.Background()
	seedFeedback(t)

	// No subcommand at all must behave like "list", not panic on args[1:].
	var out bytes.Buffer
	if err := Feedback(ctx, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search_content rejected") {
		t.Fatalf("no-args feedback did not default to list:\n%s", out.String())
	}
}

func TestFeedbackListShowResolve(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	ctx := context.Background()
	id := seedFeedback(t)

	// list (default = open)
	var out bytes.Buffer
	if err := Feedback(ctx, &out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search_content rejected") ||
		!strings.Contains(out.String(), "error") ||
		!strings.Contains(out.String(), "ordia") {
		t.Fatalf("list output missing fields:\n%s", out.String())
	}

	// show
	out.Reset()
	if err := Feedback(ctx, &out, []string{"show", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "severity") {
		t.Fatalf("show output missing fields:\n%s", out.String())
	}

	// resolve
	out.Reset()
	if err := Feedback(ctx, &out, []string{"resolve", id, "--note", "fixed"}); err != nil {
		t.Fatal(err)
	}

	// show after resolve must surface the resolution note (proves --note was parsed).
	out.Reset()
	if err := Feedback(ctx, &out, []string{"show", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fixed") {
		t.Fatalf("resolution note not persisted/shown:\n%s", out.String())
	}

	// list is now empty; list --all shows the resolved one.
	out.Reset()
	if err := Feedback(ctx, &out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "search_content rejected") {
		t.Fatalf("resolved report still in open list:\n%s", out.String())
	}
	out.Reset()
	if err := Feedback(ctx, &out, []string{"list", "--all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search_content rejected") {
		t.Fatalf("--all did not show the resolved report:\n%s", out.String())
	}
}
