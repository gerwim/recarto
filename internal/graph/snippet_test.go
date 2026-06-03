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
	snip, err := e.Snippet(ctx, wt, "t.go", "Target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snip, "func Target() int") || !strings.Contains(snip, "return 42") {
		t.Errorf("snippet = %q, want the Target function body", snip)
	}
	if strings.Contains(snip, "func Other") {
		t.Errorf("snippet leaked Other; should be just Target")
	}
	saved, _ := e.led.Get(ctx, "lifetime", "graph_tokens_saved")
	if saved <= 0 {
		t.Errorf("graph_tokens_saved = %d, want > 0", saved)
	}
}
