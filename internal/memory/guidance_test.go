package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

// GuidanceForCode surfaces the high-signal "ship better code" kinds anchored to a
// file/symbol — do_not_repeat (guardrail) and decision (rationale) — and must NOT
// leak the lower-signal kinds (bug/todo have their own tiers; learning/preference
// belong to recall), so the just-in-time read/edit tier stays high-precision.
func TestGuidanceForCodeReturnsOnlyGuardrailKinds(t *testing.T) {
	eng := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	rel := "internal/agents/handle.go"

	rec := func(k Kind, title string) {
		t.Helper()
		if _, _, err := eng.Record(ctx, NewMemory{
			Kind: k, Title: title, Body: title + " body", Source: "cli", Branch: "main",
			Refs: []Ref{{Type: RefFile, Value: rel}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec(KindDoNotRepeat, "do not hand-roll the lexer")
	rec(KindDecision, "we chose modernc sqlite")
	rec(KindBug, "off-by-one in cursor")
	rec(KindLearning, "resolve is the bottleneck")
	rec(KindTodo, "split this file")

	got, err := eng.GuidanceForCode(ctx, "main", []string{rel}, nil)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, r := range got {
		titles[r.Title] = true
	}
	if !titles["do not hand-roll the lexer"] {
		t.Errorf("do_not_repeat anchored to the file should surface; got %v", titles)
	}
	if !titles["we chose modernc sqlite"] {
		t.Errorf("decision anchored to the file should surface; got %v", titles)
	}
	if titles["off-by-one in cursor"] || titles["resolve is the bottleneck"] || titles["split this file"] {
		t.Errorf("bug/learning/todo must not leak into the guidance tier; got %v", titles)
	}
}

// A symbol-anchored guardrail must surface when that symbol is touched, mirroring
// BugsForCode's symbol path.
func TestGuidanceForCodeMatchesBySymbol(t *testing.T) {
	eng := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()
	if _, _, err := eng.Record(ctx, NewMemory{
		Kind: KindDoNotRepeat, Title: "ExecuteAsync must stay allocation-free",
		Body: "hot path", Source: "cli", Branch: "main",
		Refs: []Ref{{Type: RefSymbol, Value: "DefaultRequestExecutor.ExecuteAsync"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := eng.GuidanceForCode(ctx, "main", nil, []string{"DefaultRequestExecutor.ExecuteAsync"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "ExecuteAsync must stay allocation-free" {
		t.Fatalf("symbol-anchored guardrail not surfaced; got %v", got)
	}
}
