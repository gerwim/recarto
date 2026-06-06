package graph

import (
	"context"
	"strings"
	"testing"
)

// indexInto writes a file to wt and indexes it, returning the source length.
func indexInto(t *testing.T, e *Engine, wt, rel, content string) {
	t.Helper()
	writeFile(t, wt, rel, content)
	src, _ := FSSource{Root: wt}.Read(rel)
	if err := e.IndexFile(context.Background(), wt, rel, src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
}

func TestSearchContentFindsLiteralWithEnclosingSymbol(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	content := "package p\n\nfunc Target() string {\n\treturn \"needle in haystack\"\n}\n\nfunc Other() {}\n"
	indexInto(t, e, wt, "t.go", content)

	res, err := e.SearchContent(ctx, wt, "needle in haystack", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(res), res)
	}
	m := res[0]
	if m.Path != "t.go" || m.Line != 4 {
		t.Errorf("location = %s:%d, want t.go:4", m.Path, m.Line)
	}
	if !strings.Contains(m.Symbol, "Target") {
		t.Errorf("enclosing symbol = %q, want to contain Target", m.Symbol)
	}
	if !strings.Contains(m.Text, "needle in haystack") {
		t.Errorf("text = %q, want the matching line", m.Text)
	}
}

func TestSearchContentRegexAndIgnoreCase(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "t.go", "package p\n\nfunc F() {\n\tlog(\"ErrCode=429\")\n}\n")

	// Regex: digit class should match the numeric code.
	res, err := e.SearchContent(ctx, wt, `ErrCode=\d+`, ContentSearchOpts{Regex: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !strings.Contains(res[0].Text, "ErrCode=429") {
		t.Fatalf("regex: got %+v, want one match on ErrCode=429", res)
	}

	// A literal-only search for the same pattern must NOT match (the regex
	// metacharacters are taken verbatim).
	lit, err := e.SearchContent(ctx, wt, `ErrCode=\d+`, ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lit) != 0 {
		t.Fatalf("literal: got %+v, want no match for regex text taken literally", lit)
	}

	// Ignore case.
	ci, err := e.SearchContent(ctx, wt, "errcode", ContentSearchOpts{IgnoreCase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ci) != 1 {
		t.Fatalf("ignorecase: got %d matches, want 1", len(ci))
	}
}

func TestSearchContentDemotesGeneratedFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// Same token "Widget" appears in a generated file and a hand-written one.
	indexInto(t, e, wt, "api.generated.cs", "namespace G;\nclass GenWidget { void M() { var x = \"Widget\"; } }\n")
	indexInto(t, e, wt, "hand.cs", "namespace H;\nclass HandWidget { void M() { var x = \"Widget\"; } }\n")

	res, err := e.SearchContent(ctx, wt, "Widget", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 2 {
		t.Fatalf("got %d matches, want >=2", len(res))
	}
	// Hand-written file must rank above the generated one.
	if res[0].Path != "hand.cs" {
		t.Errorf("first result = %s, want hand.cs (generated must be demoted)", res[0].Path)
	}
}

func TestSearchContentFilters(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nvar marker = 1\n")
	indexInto(t, e, wt, "b.cs", "namespace n;\nclass C { int marker = 1; }\n")

	// Language filter.
	goOnly, err := e.SearchContent(ctx, wt, "marker", ContentSearchOpts{Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(goOnly) != 1 || goOnly[0].Path != "a.go" {
		t.Fatalf("language filter: got %+v, want only a.go", goOnly)
	}

	// Path glob filter.
	csOnly, err := e.SearchContent(ctx, wt, "marker", ContentSearchOpts{PathGlob: "*.cs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(csOnly) != 1 || csOnly[0].Path != "b.cs" {
		t.Fatalf("path glob: got %+v, want only b.cs", csOnly)
	}
}
