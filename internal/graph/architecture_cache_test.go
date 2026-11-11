package graph

import (
	"context"
	"testing"

	"github.com/gerwim/recarto/internal/store"
)

// TestArchitectureCachedByGraphToken proves Architecture serves the stored
// result while the worktree's graph token is unchanged, and recomputes when the
// token moves. The out-of-band node wipe below is the observable: a cache hit
// still reports the old modules; a token bump must not.
func TestArchitectureCachedByGraphToken(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	// Simulate the git-token bookkeeping ReconcileIfStale does for git repos.
	setToken := func(tok string) {
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "graph_token:"+wt, tok)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setToken("tok1")

	symbols := func(a Architecture) int {
		n := 0
		for _, m := range a.Modules {
			n += m.Symbols
		}
		return n
	}
	first, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Modules) == 0 || symbols(first) == 0 {
		t.Fatalf("fixture produced no modules/symbols: %+v", first.Modules)
	}

	// Wipe the graph out-of-band (token untouched): a cache hit still sees the
	// old shape — by contract the cache is exactly as fresh as the token.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM nodes`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cached, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	if symbols(cached) != symbols(first) {
		t.Fatalf("unchanged token must serve the cached architecture: got %d symbols, want %d",
			symbols(cached), symbols(first))
	}

	// Token moved ⇒ recompute against the (now symbol-less) graph.
	setToken("tok2")
	fresh, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	if symbols(fresh) != 0 {
		t.Fatalf("token change must invalidate the cache; stale symbol count %d", symbols(fresh))
	}
}

// TestArchitectureIgnoresFileScopeReferences pins a subtlety of the dependency
// aggregation: an edge with no source symbol (a file-scope reference — an
// import, or a call outside any definition) is not a module dependency. The
// old SQL excluded them via its inner join on the source node; the Go-side
// aggregation must too.
func TestArchitectureIgnoresFileScopeReferences(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "alpha/a.go", "package alpha\nfunc A(){}")
	writeFile(t, wt, "beta/b.go", "package beta\nfunc B(){}")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Synthesize a file-scope (NULL src) resolved reference alpha → beta.
	var aFile, bNode string
	if err := e.st.DB.QueryRow(`SELECT id FROM files WHERE worktree=? AND path='alpha/a.go'`, wt).Scan(&aFile); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DB.QueryRow(`SELECT n.id FROM nodes n JOIN files f ON f.id=n.file_id
		WHERE f.worktree=? AND n.name='B'`, wt).Scan(&bNode); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `INSERT INTO edges(id,src_node_id,dst_node_id,dst_name,kind,file_id,line)
		 VALUES ('synthetic-edge', NULL, ?, 'B', 'imports', ?, 1)`, bNode, aFile)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	arch, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range arch.Deps {
		if d.From == "alpha" && d.To == "beta" {
			t.Fatalf("file-scope reference must not count as a module dependency: %+v", d)
		}
	}
}
