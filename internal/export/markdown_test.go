package export

import (
	"testing"

	"github.com/gerwim/recarto/internal/memory"
)

func TestRenderGolden(t *testing.T) {
	// 1_700_000_000_000 ms = 2023-11-14 UTC.
	recs := []*memory.Record{
		{
			Kind:               memory.KindDoNotRepeat,
			Title:              "no redundant toolchain line",
			Body:               "go.mod must not carry a toolchain directive equal to the go line",
			UpdatedAt:          1_700_000_000_000,
			Branch:             "",
			ReinforcementCount: 2,
			Refs:               []memory.Ref{{Type: memory.RefFile, Value: "go.mod"}},
		},
		{
			Kind:               memory.KindLearning,
			Title:              "WAL needs busy_timeout",
			Body:               "set busy_timeout so writers wait instead of erroring",
			UpdatedAt:          1_700_000_000_000,
			Branch:             "main",
			ReinforcementCount: 0,
		},
	}

	want := "# Recarto Memory — demo\n\n" +
		"_2 active memories_\n\n" +
		"## Do Not Repeat\n\n" +
		"- **no redundant toolchain line**\n" +
		"  go.mod must not carry a toolchain directive equal to the go line\n" +
		"  _updated 2023-11-14 · reinforced ×2 · scope global · refs: go.mod_\n\n" +
		"## Learnings\n\n" +
		"- **WAL needs busy_timeout**\n" +
		"  set busy_timeout so writers wait instead of erroring\n" +
		"  _updated 2023-11-14 · reinforced ×0 · scope main · refs: none_\n"

	got := Render("demo", recs)
	if got != want {
		t.Errorf("Render mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	got := Render("demo", nil)
	want := "# Recarto Memory — demo\n\n_0 active memories_\n"
	if got != want {
		t.Errorf("empty render = %q, want %q", got, want)
	}
}

// TestRenderUntitledAndMultilineBody pins the (untitled) fallback and that a
// multi-line body's continuation lines stay indented inside the list item.
func TestRenderUntitledAndMultilineBody(t *testing.T) {
	recs := []*memory.Record{
		{
			Kind:      memory.KindLearning,
			Title:     "",
			Body:      "line one\nline two",
			UpdatedAt: 1_700_000_000_000,
			Branch:    "dev",
		},
	}
	want := "# Recarto Memory — demo\n\n" +
		"_1 active memories_\n\n" +
		"## Learnings\n\n" +
		"- **(untitled)**\n" +
		"  line one\n" +
		"  line two\n" +
		"  _updated 2023-11-14 · reinforced ×0 · scope dev · refs: none_\n"

	got := Render("demo", recs)
	if got != want {
		t.Errorf("Render mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}
