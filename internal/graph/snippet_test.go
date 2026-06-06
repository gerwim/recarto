package graph

import (
	"context"
	"strings"
	"testing"
)

func TestSnippetReturnsSymbolSource(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	content := "package p\n\nfunc Target() int {\n\treturn 42\n}\n\nfunc Other() {}\n"
	writeFile(t, wt, "t.go", content)
	src, _ := FSSource{Root: wt}.Read("t.go")
	if err := e.IndexFile(ctx, wt, "t.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, err := e.Snippet(ctx, wt, "t.go", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Snippet, "func Target() int") || !strings.Contains(res.Snippet, "return 42") {
		t.Errorf("snippet = %q, want the Target function body", res.Snippet)
	}
	if strings.Contains(res.Snippet, "func Other") {
		t.Errorf("snippet leaked Other; should be just Target")
	}
	saved, _ := e.led.Get(ctx, "lifetime", "graph_tokens_saved")
	if saved <= 0 {
		t.Errorf("graph_tokens_saved = %d, want > 0", saved)
	}
}

func TestSnippetByNameUniqueReturnsSource(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\n\nfunc Uniq() int {\n\treturn 7\n}\n")
	src, _ := FSSource{Root: wt}.Read("a.go")
	if err := e.IndexFile(ctx, wt, "a.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Uniq")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found || !strings.Contains(res.Snippet, "func Uniq() int") {
		t.Errorf("res = %+v, want the Uniq body", res)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %v, want none for a unique name", cands)
	}
}

func TestSnippetByNameAmbiguousReturnsCandidates(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package a\n\nfunc Dup() {}\n")
	writeFile(t, wt, "b.go", "package b\n\nfunc Dup() {}\n")
	for _, f := range []string{"a.go", "b.go"} {
		src, _ := FSSource{Root: wt}.Read(f)
		if err := e.IndexFile(ctx, wt, f, src, 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Dup")
	if err != nil {
		t.Fatal(err)
	}
	if res.Found {
		t.Errorf("ambiguous name should not resolve to a single snippet, got %+v", res)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	for _, c := range cands {
		if c.Path == "" || c.StartLine == 0 {
			t.Errorf("candidate missing locator: %+v", c)
		}
	}
}

func TestSnippetByNameOverloadsReturnAllBodies(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// Two overloads of Add share one qualified name (N.Calc.Add) — and one file.
	// Neither a qualified name nor a file path can disambiguate them, so the old
	// "ambiguous; re-run with a qualified name or the file" advice was unfollowable
	// and forced the agent to read the whole file. The overloads ARE the answer:
	// return every body in one result so the agent stays in recarto.
	code := "namespace N;\nclass Calc {\n" +
		"  public int Add(int a, int b) { return a + b; }\n" +
		"  public string Add(string a, string b) { return a + b; }\n}"
	writeFile(t, wt, "Calc.cs", code)
	src, _ := FSSource{Root: wt}.Read("Calc.cs")
	if err := e.IndexFile(ctx, wt, "Calc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Add")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found {
		t.Fatalf("overloaded Add should resolve to a combined snippet, got Found=false cands=%v", cands)
	}
	if len(cands) != 0 {
		t.Errorf("a resolved overload set must not also return candidates, got %v", cands)
	}
	if !strings.Contains(res.Snippet, "int Add(int a, int b)") || !strings.Contains(res.Snippet, "string Add(string a, string b)") {
		t.Errorf("combined snippet must contain BOTH overloads, got:\n%s", res.Snippet)
	}
	// Each body must be locatable: the per-overload location header carries file:line.
	if !strings.Contains(res.Snippet, "Calc.cs:3") || !strings.Contains(res.Snippet, "Calc.cs:4") {
		t.Errorf("combined snippet must label each overload with its file:line, got:\n%s", res.Snippet)
	}
}

func TestSnippetByNameQualifiedDisambiguates(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	// Two methods share the bare name Run but differ by receiver/qualified name.
	writeFile(t, wt, "svc.go", "package p\n\ntype A struct{}\n\nfunc (A) Run() int {\n\treturn 1\n}\n\ntype B struct{}\n\nfunc (B) Run() int {\n\treturn 2\n}\n")
	src, _ := FSSource{Root: wt}.Read("svc.go")
	if err := e.IndexFile(ctx, wt, "svc.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	// Bare name is ambiguous.
	_, cands, err := e.SnippetByName(ctx, wt, "Run")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) < 2 {
		t.Fatalf("bare Run should be ambiguous, got %d candidates", len(cands))
	}
	// The qualified name picks exactly one.
	qn := cands[0].QualifiedName
	if qn == "" {
		t.Fatal("candidate has no qualified name to disambiguate with")
	}
	res, cands2, err := e.SnippetByName(ctx, wt, qn)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found || len(cands2) != 0 {
		t.Errorf("qualified %q should resolve uniquely, got res=%+v cands=%v", qn, res, cands2)
	}
}

// TestSnippetByNameTypePreferredOverOwnConstructor: in C#/Java a type and its
// constructor share the bare name, so a file-less snippet lookup for the type
// matched both and bounced the agent with a spurious "ambiguous" disambiguation.
// The constructor is owned by (nested in) the type, so it is not a real
// alternative — the lookup resolves directly to the type's source.
func TestSnippetByNameTypePreferredOverOwnConstructor(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "Widget.cs", "namespace App;\n\npublic class Widget\n{\n    public Widget() { }\n\n    public void Run() { }\n}\n")
	src, _ := FSSource{Root: wt}.Read("Widget.cs")
	if err := e.IndexFile(ctx, wt, "Widget.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Widget")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found {
		t.Fatalf("type should resolve directly over its own constructor, got candidates %v", cands)
	}
	if res.QualifiedName != "App.Widget" {
		t.Fatalf("resolved to %q, want the class App.Widget (not its constructor)", res.QualifiedName)
	}
}

// TestSnippetByNameGenuinelyAmbiguousTypesStillBounce guards that the constructor
// collapse does NOT mask real ambiguity: two distinct same-named types remain a
// genuine choice the agent must disambiguate.
func TestSnippetByNameGenuinelyAmbiguousTypesStillBounce(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	a := []byte("namespace A;\n\npublic class Widget\n{\n    public Widget() { }\n}\n")
	b := []byte("namespace B;\n\npublic class Widget\n{\n    public Widget() { }\n}\n")
	if err := e.IndexFile(ctx, wt, "A.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, wt, "B.cs", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Widget")
	if err != nil {
		t.Fatal(err)
	}
	if res.Found {
		t.Fatalf("two distinct Widget types must stay ambiguous, resolved to %q", res.QualifiedName)
	}
	// Exactly the two classes survive — both constructors collapse into their type.
	if len(cands) != 2 {
		t.Fatalf("want 2 type candidates, got %d: %v", len(cands), cands)
	}
	for _, c := range cands {
		if c.Kind != "class" {
			t.Errorf("candidate %q is %s, want the class (constructor should have collapsed)", c.QualifiedName, c.Kind)
		}
	}
}

func TestSnippetByNameMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\n\nfunc Here() {}\n")
	src, _ := FSSource{Root: wt}.Read("a.go")
	if err := e.IndexFile(ctx, wt, "a.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, cands, err := e.SnippetByName(ctx, wt, "Nope")
	if err != nil {
		t.Fatalf("miss should not error: %v", err)
	}
	if res.Found || len(cands) != 0 {
		t.Errorf("miss should be empty, got res=%+v cands=%v", res, cands)
	}
}

func TestSnippetResultFoundAndMiss(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "sample.go", "package p\n\nfunc Greet(name string) string {\n\treturn name\n}\n")
	src, _ := FSSource{Root: wt}.Read("sample.go")
	if err := e.IndexFile(ctx, wt, "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	got, err := e.Snippet(ctx, wt, "sample.go", "Greet")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.StartLine == 0 || got.Snippet == "" {
		t.Fatalf("found symbol: %+v", got)
	}

	miss, err := e.Snippet(ctx, wt, "sample.go", "DoesNotExist")
	if err != nil {
		t.Fatalf("miss should not error, got %v", err)
	}
	if miss.Found {
		t.Error("expected Found=false for unknown symbol")
	}
}
