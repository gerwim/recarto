package graph

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// TestRequiredLiteral exercises the conservative required-literal extractor that
// powers the grep pre-filter. A non-empty result must be a substring of EVERY
// possible match of the pattern; "" means "cannot prove one, scan everything".
func TestRequiredLiteral(t *testing.T) {
	cases := []struct {
		expr, want string
	}{
		{`IRequestExecutor`, "IRequestExecutor"},     // plain literal
		{`\bIRequestExecutor\b`, "IRequestExecutor"}, // zero-width anchors keep siblings
		{`^name = `, "name = "},                      // line anchor
		{`foo|bar`, ""},                              // alternation: nothing required
		{`(?i)foo`, ""},                              // case folding: literal not exact
		{`a.+bcd`, "bcd"},                            // longest required literal wins
		{`x(yz)+`, "yz"},                             // plus: child required at least once
		{`Get(Foo|Bar)`, "Get"},                      // required prefix, optional tail
		{`[Tt]oken`, "oken"},                         // class breaks the head, not the tail
		{`colou?r`, "colo"},                          // optional char ends the run
		{`a*`, ""},                                   // possibly-empty match
		{`TODO(:.*)?`, "TODO"},                       // optional group ignored
		{`(?s)first.*last`, "first"},                 // dotall still concatenates
		{`\d+`, ""},                                  // no literal at all
		{fmt.Sprintf(`%c`, 'é') + `tat`, "état"},     // multibyte literals survive
	}
	for _, c := range cases {
		if got := requiredLiteral(c.expr); got != c.want {
			t.Errorf("requiredLiteral(%q) = %q, want %q", c.expr, got, c.want)
		}
	}
}

// TestSearchContentLineAnchorSemantics pins per-LINE regex semantics: ^ anchors
// to each line, exactly as if every line were matched on its own — not to the
// start of the file. The fast path must not change this.
func TestSearchContentLineAnchorSemantics(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "f.txt", "foo bar\nbar foo\nbarista\n")

	got, err := e.SearchContent(ctx, wt, `^bar`, ContentSearchOpts{Regex: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want the two ^bar lines, got %d: %+v", len(got), got)
	}
	if got[0].Line != 2 || got[1].Line != 3 {
		t.Fatalf("want lines 2 and 3, got %d and %d", got[0].Line, got[1].Line)
	}
}

// TestSearchContentCountsLineOncePerLine pins meta.Total as a count of matching
// LINES, not occurrences: three hits on one line are one match.
func TestSearchContentCountsLineOncePerLine(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "f.txt", "aa aa aa\nnothing\naa aa\n")

	_, meta, err := e.SearchContentWithMeta(ctx, wt, "aa", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Total != 2 {
		t.Fatalf("want Total=2 (two matching lines), got %d", meta.Total)
	}
}

// TestSearchContentDeterministicOrderAcrossManyFiles pins the result order the
// parallel scan must preserve: hand-written files first, then generated, each
// bucket in sorted-path order — identical run to run.
func TestSearchContentDeterministicOrderAcrossManyFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	var hand []string
	for i := range 24 {
		p := fmt.Sprintf("src/d%02d/f%02d.txt", i%6, i)
		writeFile(t, wt, p, "needle here\n")
		hand = append(hand, p)
	}
	writeFile(t, wt, "gen/api.generated.cs", "needle here\n")
	sort.Strings(hand)

	for run := range 3 {
		got, err := e.SearchContent(ctx, wt, "needle", ContentSearchOpts{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(hand)+1 {
			t.Fatalf("run %d: want %d matches, got %d", run, len(hand)+1, len(got))
		}
		for i, p := range hand {
			if got[i].Path != p {
				t.Fatalf("run %d: position %d: want %s, got %s", run, i, p, got[i].Path)
			}
		}
		if last := got[len(got)-1].Path; last != "gen/api.generated.cs" {
			t.Fatalf("run %d: generated file must sort last, got %s", run, last)
		}
	}
}
