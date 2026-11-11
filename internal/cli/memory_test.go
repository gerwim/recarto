package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/memory"
)

func TestMemoryShowRendersMarkdown(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindDecision,
		Title:  "sqlite is source of truth",
		Body:   "markdown is render-only so it cannot drift",
		Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Memory(ctx, a, &buf, []string{"show"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "# Recarto Memory — repo") {
		t.Errorf("missing export header:\n%s", out)
	}
	if !strings.Contains(out, "## Decisions") || !strings.Contains(out, "sqlite is source of truth") {
		t.Errorf("decision not rendered:\n%s", out)
	}
}

func TestMemoryRequiresShow(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Memory(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error without a subcommand")
	}
}

// TestMemoryShowBranchFilter pins the --branch flag → ListFilter.Branch wiring at
// the CLI level: a branch-scoped show excludes other branches' memories.
func TestMemoryShowBranchFilter(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	rec := func(branch, title, body string) {
		t.Helper()
		if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
			Kind: memory.KindDecision, Title: title, Body: body, Branch: branch}); err != nil {
			t.Fatal(err)
		}
	}
	rec("main", "main only decision", "decision recorded on the main branch")
	rec("feature", "feature only decision", "decision recorded on the feature branch")

	var buf bytes.Buffer
	if err := Memory(ctx, a, &buf, []string{"show", "--branch", "feature"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "feature only decision") {
		t.Errorf("--branch feature should include the feature record:\n%s", out)
	}
	if strings.Contains(out, "main only decision") {
		t.Errorf("--branch feature should exclude the main record:\n%s", out)
	}
}
