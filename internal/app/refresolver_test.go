package app

import (
	"context"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func seedFile(t *testing.T, a *App, worktree, path string) {
	t.Helper()
	_, err := a.Store.DB.ExecContext(context.Background(),
		`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		path, worktree, path, "go", "h", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func seedNode(t *testing.T, a *App, fileID, name string) {
	t.Helper()
	_, err := a.Store.DB.ExecContext(context.Background(),
		`INSERT INTO nodes(id,file_id,kind,name,qualified_name,signature,start_byte,end_byte,start_line,end_line)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		name, fileID, "func", name, name, "", 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGraphResolverDangling(t *testing.T) {
	a := newTestApp(t, "main")
	wt := a.Git.Worktree
	seedFile(t, a, wt, "present.go")
	seedNode(t, a, "present.go", "Foo")
	r := graphRefResolver{st: a.Store}

	refs := []memory.Ref{
		{Type: memory.RefFile, Value: "present.go"},
		{Type: memory.RefFile, Value: "absent.go"},
		{Type: memory.RefSymbol, Value: "Foo"},
		{Type: memory.RefSymbol, Value: "Bar"},
	}
	d, err := r.Dangling(context.Background(), wt, refs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range d {
		got[x.Value] = true
	}
	if !got["absent.go"] || !got["Bar"] || got["present.go"] || got["Foo"] {
		t.Fatalf("unexpected dangling set: %v", got)
	}
}

func TestGraphResolverDegradesWhenNoGraph(t *testing.T) {
	a := newTestApp(t, "main")
	r := graphRefResolver{st: a.Store}
	d, err := r.Dangling(context.Background(), "/empty-worktree", []memory.Ref{
		{Type: memory.RefFile, Value: "absent.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 0 {
		t.Fatalf("no graph rows → nothing dangles, got %v", d)
	}
}
