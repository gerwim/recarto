package graph

import (
	"context"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, p string
		want    bool
	}{
		// Bare filename glob matches the basename at any depth.
		{"*.cs", "src/Foo/Handlers/DeployCompleted.cs", true},
		{"*Handler*.cs", "src/Foo/DeployCompletedHandler.cs", true},
		{"*.go", "src/Foo/Bar.cs", false},
		// Doublestar matches across path separators (the false-negative cases).
		{"**/*.cs", "src/Foo/Handlers/DeployCompleted.cs", true},
		{"src/**", "src/Foo/Handlers/DeployCompleted.cs", true},
		{"src/**/*.cs", "src/Foo/Handlers/DeployCompleted.cs", true},
		{"**/Handlers/*.cs", "src/Foo/Handlers/DeployCompleted.cs", true},
		// A single star stays within one path segment.
		{"src/*", "src/Foo/Handlers/DeployCompleted.cs", false},
		{"src/*.cs", "src/Bar.cs", true},
		// Doublestar under a subtree does not leak to siblings.
		{"src/**/*.cs", "other/Foo/Bar.cs", false},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.p); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.glob, c.p, got, c.want)
		}
	}
}

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

// TestSearchContentSearchesUnindexedTextFiles proves grep reaches files the code
// graph never parsed — config, build, docs, data (.csproj, .json, .md, .yaml).
// Without this, an agent grepping a dependency name in a .csproj gets a false
// "(no matches)" and falls back to shell grep, defeating the point of recarto.
func TestSearchContentSearchesUnindexedTextFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// One indexed code file and several text files the indexer skips.
	indexInto(t, e, wt, "code.go", "package p\nfunc Use() { _ = \"DependencyMarker\" }\n")
	writeFile(t, wt, "proj.csproj", "<Project>\n  <PackageReference Include=\"DependencyMarker\" />\n</Project>\n")
	writeFile(t, wt, "appsettings.json", "{\n  \"key\": \"DependencyMarker\"\n}\n")
	writeFile(t, wt, "README.md", "# Title\nUses DependencyMarker for things.\n")

	res, err := e.SearchContent(ctx, wt, "DependencyMarker", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range res {
		got[m.Path] = true
	}
	for _, want := range []string{"code.go", "proj.csproj", "appsettings.json", "README.md"} {
		if !got[want] {
			t.Errorf("missing match in %s; got paths %v", want, got)
		}
	}
	// The indexed file still carries its enclosing symbol; an unindexed file has none.
	for _, m := range res {
		if m.Path == "code.go" && !strings.Contains(m.Symbol, "Use") {
			t.Errorf("indexed file lost its enclosing symbol: %+v", m)
		}
		if m.Path == "proj.csproj" && m.Symbol != "" {
			t.Errorf("unindexed file should have empty symbol, got %q", m.Symbol)
		}
	}
}

// TestSearchContentSkipsBinaryFiles proves grep does not emit lines from binary
// blobs (images, compiled output): they would be garbage tokens, and ripgrep
// skips them too. Detection is by NUL byte and by known-binary extension.
func TestSearchContentSkipsBinaryFiles(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "code.go", "package p\nvar marker = \"BinNeedle\"\n")
	// A binary blob whose bytes happen to contain the search text around a NUL.
	writeFile(t, wt, "blob.dat", "prefix\x00BinNeedle\x00suffix")
	// A PNG by extension (skipped before read).
	writeFile(t, wt, "img.png", "BinNeedle not really a png")

	res, err := e.SearchContent(ctx, wt, "BinNeedle", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res {
		if m.Path == "blob.dat" || m.Path == "img.png" {
			t.Errorf("binary file %s should be skipped, got match: %+v", m.Path, m)
		}
	}
	if len(res) != 1 || res[0].Path != "code.go" {
		t.Fatalf("want only the text match in code.go, got %+v", res)
	}
}

// TestSearchContentMetaReportsTruncation proves the meta channel reports the true
// total match count and that both the global limit and the per-file cap are
// surfaced — so an LLM treating grep as exhaustive learns the page is partial
// instead of acting on a false-complete answer.
func TestSearchContentMetaReportsTruncation(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// a.go has 4 matching lines, b.go has 3 → 7 total matching lines.
	indexInto(t, e, wt, "a.go", "package p\n// marker 1\n// marker 2\n// marker 3\n// marker 4\nfunc A() {}\n")
	indexInto(t, e, wt, "b.go", "package p\n// marker 1\n// marker 2\n// marker 3\nfunc B() {}\n")

	res, meta, err := e.SearchContentWithMeta(ctx, wt, "marker", ContentSearchOpts{MaxPerFile: 2, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Total != 7 {
		t.Errorf("Total = %d, want 7 (every matching line, both caps ignored)", meta.Total)
	}
	if len(res) != 3 || meta.Shown != 3 {
		t.Errorf("Shown = %d (len %d), want 3 (the global limit)", meta.Shown, len(res))
	}
	if !meta.Truncated {
		t.Error("Truncated = false, want true (7 matches, only 3 shown)")
	}
	if !meta.PerFileCapped {
		t.Error("PerFileCapped = false, want true (a.go had 4 > MaxPerFile 2)")
	}
}

func TestSearchContentOffsetPaging(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "t.go", "package p\n// needle one\n// needle two\n// needle three\nfunc F() {}\n")

	page1, m1, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || !m1.Truncated || m1.Total != 3 {
		t.Fatalf("page1: got %d matches, truncated=%v, total=%d; want 2/true/3", len(page1), m1.Truncated, m1.Total)
	}

	page2, m2, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].Line != 4 {
		t.Fatalf("page2: got %+v, want exactly the third match (line 4)", page2)
	}
	if m2.Truncated {
		t.Error("page2 truncated = true, want false (offset+shown covers total)")
	}

	empty, _, err := e.SearchContentWithMeta(ctx, wt, "needle", ContentSearchOpts{Limit: 2, MaxPerFile: 10, Offset: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("overshoot offset: got %+v, want empty (not an error)", empty)
	}
}

// TestSearchContentMetaNoTruncation proves a complete result set is reported as
// such — no false truncation warning when every match is shown.
func TestSearchContentMetaNoTruncation(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	indexInto(t, e, wt, "a.go", "package p\nfunc A() { /* unique_token */ }\n")

	res, meta, err := e.SearchContentWithMeta(ctx, wt, "unique_token", ContentSearchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || meta.Total != 1 || meta.Shown != 1 {
		t.Errorf("meta = %+v, len = %d, want exactly one complete match", meta, len(res))
	}
	if meta.Truncated || meta.PerFileCapped {
		t.Errorf("unexpected truncation flags: %+v", meta)
	}
}
