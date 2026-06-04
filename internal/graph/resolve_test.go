package graph

import (
	"context"
	"testing"
)

func TestResolveLinksCallEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Two files in one worktree: a.go's A() calls B() defined in b.go.
	if err := e.IndexFile(ctx, "/wt", "a.go", []byte("package p\nfunc A(){ B() }"), 1, 24); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "b.go", []byte("package p\nfunc B(){}"), 1, 19); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var resolved int
	if err := e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges WHERE kind='calls' AND dst_name='B' AND dst_node_id IS NOT NULL`).
		Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Errorf("resolved call edges to B = %d, want 1", resolved)
	}
}

func TestResolveDoesNotCrossLanguageFamily(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A C# method calls Select() (a LINQ method, external to the graph). A TSX
	// function named Select exists in the frontend. Resolution must NOT bind the
	// C# call to the TS Select — they are different language families.
	cs := []byte("namespace N;\nclass Svc {\n  void M() { x.Select(); }\n}")
	if err := e.IndexFile(ctx, "/wt", "api/Svc.cs", cs, 1, int64(len(cs))); err != nil {
		t.Fatal(err)
	}
	ts := []byte("export function Select() {}")
	if err := e.IndexFile(ctx, "/wt", "web/select.tsx", ts, 1, int64(len(ts))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var crossLang int
	if err := e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges ed
		  JOIN files ef ON ef.id = ed.file_id
		  JOIN nodes n  ON n.id  = ed.dst_node_id
		  JOIN files df ON df.id = n.file_id
		 WHERE ef.language='csharp' AND df.language IN ('typescript','tsx','javascript')`).Scan(&crossLang); err != nil {
		t.Fatal(err)
	}
	if crossLang != 0 {
		t.Errorf("cross-language resolutions = %d, want 0", crossLang)
	}
}

func TestResolvePrefersSameFile(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Both files define Helper(); a.go's A() calls Helper(). Resolution must prefer
	// the same-file Helper over the other file's identically-named one.
	a := []byte("package p\nfunc A(){ Helper() }\nfunc Helper(){}")
	if err := e.IndexFile(ctx, "/wt", "a.go", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	b := []byte("package p\nfunc Helper(){}")
	if err := e.IndexFile(ctx, "/wt", "b.go", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := e.st.DB.QueryRow(`
		SELECT f.path FROM edges ed
		  JOIN nodes n ON n.id = ed.dst_node_id
		  JOIN files f ON f.id = n.file_id
		 WHERE ed.kind='calls' AND ed.dst_name='Helper'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "a.go" {
		t.Errorf("Helper call resolved to %q, want a.go (same-file preference)", path)
	}
}

func TestResolvePrefersSameTopLevelDir(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Foo is defined in both auth/ and other/; the call from auth/a.go must prefer
	// the same-top-level-directory definition over the equally-named other one.
	if err := e.IndexFile(ctx, "/wt", "auth/a.go", []byte("package p\nfunc A(){ Foo() }"), 1, 30); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "auth/b.go", []byte("package p\nfunc Foo(){}"), 1, 21); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, "/wt", "other/c.go", []byte("package p\nfunc Foo(){}"), 1, 21); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := e.st.DB.QueryRow(`
		SELECT f.path FROM edges ed
		  JOIN nodes n ON n.id = ed.dst_node_id
		  JOIN files f ON f.id = n.file_id
		 WHERE ed.kind='calls' AND ed.dst_name='Foo'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "auth/b.go" {
		t.Errorf("Foo resolved to %q, want auth/b.go (same top-level dir preference)", path)
	}
}

func TestResolveWithinWebFamily(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A .tsx component calls a helper defined in a .ts file: typescript and tsx are
	// the same language family, so the call must resolve across the dialect.
	tsx := []byte("export function Comp(){ helper(); }")
	if err := e.IndexFile(ctx, "/wt", "web/comp.tsx", tsx, 1, int64(len(tsx))); err != nil {
		t.Fatal(err)
	}
	ts := []byte("export function helper(){}")
	if err := e.IndexFile(ctx, "/wt", "web/util.ts", ts, 1, int64(len(ts))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges WHERE kind='calls' AND dst_name='helper' AND dst_node_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("web-family resolved helper calls = %d, want 1", n)
	}
}
