package graph

import (
	"context"
	"testing"
)

func TestSearchByCamelComponent(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Modern .NET identifiers are PascalCase with no separators; an agent
	// searching a conceptual component word ("Renderer", "Backup") must still
	// find the symbol, not only its exact full name.
	src := []byte("namespace App;\nclass ActivityMessageRenderer {}\nclass BackupService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ query, want string }{
		{"Renderer", "ActivityMessageRenderer"},
		{"Message", "ActivityMessageRenderer"},
		{"Backup", "BackupService"},
		{"ActivityMessageRenderer", "ActivityMessageRenderer"}, // exact still works
	}
	for _, c := range cases {
		got, err := e.Search(ctx, "/wt", c.query, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range got {
			if r.Name == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("Search(%q) did not find %q; got %d results %v",
				c.query, c.want, len(got), resultNames(got))
		}
	}
}

func resultNames(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

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
