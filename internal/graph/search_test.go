package graph

import (
	"context"
	"testing"
)

func TestSearchByName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Authenticate(){}\nfunc helper(){}\ntype Session struct{}")
	if err := e.IndexFile(ctx, "/wt", "auth.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "Authenticate", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Authenticate" {
		t.Fatalf("Search = %+v, want one result Authenticate", got)
	}
	if got[0].Path != "auth.go" || got[0].Kind != "function" {
		t.Errorf("result = %+v, want path auth.go kind function", got[0])
	}
	only, err := e.Search(ctx, "/wt", "Session", "type", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Kind != "type" {
		t.Errorf("kind-filtered search = %+v", only)
	}
	none, _ := e.Search(ctx, "/other", "Authenticate", "", "", 10)
	if len(none) != 0 {
		t.Errorf("cross-worktree search leaked %d results", len(none))
	}
}
