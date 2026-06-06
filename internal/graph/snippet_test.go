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
