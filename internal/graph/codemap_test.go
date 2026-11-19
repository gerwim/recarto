package graph

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/store"
)

func moduleNamesOf(a Architecture) []string {
	var names []string
	for _, m := range a.Modules {
		names = append(names, m.Name)
	}
	return names
}

func TestCodeMapStatus(t *testing.T) {
	ctx := context.Background()
	reconcile := func(t *testing.T, e *Engine, wt string) {
		if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(token string) func(*testing.T, *Engine, string) {
		return func(t *testing.T, e *Engine, wt string) {
			if err := e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, token); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		nonGit  bool
		noFiles bool
		setup   func(t *testing.T, e *Engine, wt string)
		want    MapStatus
	}{
		{"nothing indexed", false, false, func(*testing.T, *Engine, string) {}, MapStatus{}},
		{"finished index", false, false, finish("tok"), MapStatus{Done: true, Built: true}},
		{"cut short after a finish", false, false, func(t *testing.T, e *Engine, wt string) {
			finish("tok")(t, e, wt)
			if err := e.setResolvePending(ctx, wt); err != nil {
				t.Fatal(err)
			}
		}, MapStatus{Built: true}},
		{"outside git, never finished", true, false, func(t *testing.T, e *Engine, wt string) {
			reconcile(t, e, wt)
			if err := e.setResolvePending(ctx, wt); err != nil {
				t.Fatal(err)
			}
		}, MapStatus{}},
		{"outside git, finished by an earlier version", true, false, reconcile, MapStatus{Done: true, Built: true}},
		{"outside git, finished with no files", true, true, finish(""), MapStatus{Done: true, Built: true}},
		{"pruned", false, false, func(t *testing.T, e *Engine, wt string) {
			finish("tok")(t, e, wt)
			if ok, err := e.PruneWorktree(ctx, wt, e.clk.Now().Add(time.Second)); err != nil || !ok {
				t.Fatalf("PruneWorktree = %v, %v", ok, err)
			}
		}, MapStatus{}},
		{"outside git, pruned", true, false, func(t *testing.T, e *Engine, wt string) {
			finish("")(t, e, wt)
			if ok, err := e.PruneWorktree(ctx, wt, e.clk.Now().Add(time.Second)); err != nil || !ok {
				t.Fatalf("PruneWorktree = %v, %v", ok, err)
			}
		}, MapStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			wt := t.TempDir()
			if !tc.noFiles {
				writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
			}
			tc.setup(t, e, wt)
			arch, st, err := e.CodeMap(ctx, wt, tc.nonGit)
			if err != nil {
				t.Fatal(err)
			}
			if st != tc.want {
				t.Fatalf("status = %+v, want %+v", st, tc.want)
			}
			want := ""
			if st.Done && !tc.noFiles {
				want = "api"
			}
			if got := strings.Join(moduleNamesOf(arch), ","); got != want {
				t.Errorf("modules = %q, want %q", got, want)
			}
		})
	}
}

// An edit the post-edit hook indexes outside git shows in the next map:
// outside git the map is never cached.
func TestCodeMapOutsideGitCountsAPostEdit(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wt := t.TempDir()
	writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
	if err := e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, ""); err != nil {
		t.Fatal(err)
	}
	symbols := func() int {
		arch, st, err := e.CodeMap(ctx, wt, true)
		if err != nil || !st.Done || len(arch.Modules) != 1 {
			t.Fatalf("CodeMap = %+v, %+v, %v", arch, st, err)
		}
		return arch.Modules[0].Symbols
	}
	before := symbols()
	src := []byte("package api\nfunc A(){}\nfunc B(){}")
	writeFile(t, wt, "api/a.go", string(src))
	if err := e.ReindexFileIncremental(ctx, wt, "api/a.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if got := symbols(); got != before+1 {
		t.Errorf("symbols = %d, want %d", got, before+1)
	}
}

func TestCodeMapReadsOneSnapshot(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
	writeFile(t, wt, "lib/b.go", "package lib\nfunc B(){}")
	if err := e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, "tok"); err != nil {
		t.Fatal(err)
	}
	fired := false
	archHook = func() {
		if fired {
			return
		}
		fired = true
		// What a reconcile starting in another process commits.
		if err := e.setResolvePending(ctx, wt); err != nil {
			t.Error(err)
		}
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx, `DELETE FROM files WHERE worktree=? AND path='lib/b.go'`, wt)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { archHook = nil })

	arch, st, err := e.CodeMap(ctx, wt, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(moduleNamesOf(arch), ","); !st.Done || got != "api,lib" {
		t.Errorf("CodeMap = %q, %+v; want api,lib from the snapshot the check saw", got, st)
	}
	archHook = nil
	if _, st, err := e.CodeMap(ctx, wt, false); err != nil || st.Done {
		t.Errorf("after the commit: %+v, %v; want not done (pending marker)", st, err)
	}
}

func TestArchitectureNotCachedFromUnfinishedIndex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
	if err := e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, "tok1"); err != nil {
		t.Fatal(err)
	}
	if err := e.setResolvePending(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Architecture(ctx, wt, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.clearResolvePending(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM nodes`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	arch, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range arch.Modules {
		if m.Symbols != 0 {
			t.Errorf("module %+v: a map of the unfinished index was cached", m)
		}
	}
}

func TestArchitectureQueriesReadOnlyTheWorktree(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.Reconcile(ctx, "/wt", crossFileCallSource()); err != nil {
		t.Fatal(err)
	}
	// The reconcile's ANALYZE costs a two-file store, where a scan is cheapest.
	// Plan against the statistics of a store holding many worktrees instead.
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, row := range [][2]string{
			{"files", "idx_files_worktree"}, {"nodes", "idx_nodes_file"}, {"edges", "idx_edges_file"},
		} {
			if _, err := c.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE tbl=?`, row[0]); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`,
				row[0], row[1], "1000000 20"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A new handle loads the statistics written above.
	st, err := store.Open(e.st.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, q := range []string{archNodesQuery, archEdgesQuery} {
		for _, step := range explainPlan(t, st.DB, q, "/wt") {
			if strings.HasPrefix(step, "SCAN") {
				t.Errorf("plan of %q has a full scan: %q", q, step)
			}
		}
	}
}

func TestArchitectureIgnoresOtherWorktrees(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	index := func(wt string, files map[string]string) {
		for p, src := range files {
			if err := e.IndexFile(ctx, wt, p, []byte(src), 1, int64(len(src))); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
			t.Fatal(err)
		}
	}
	index("/wtA", map[string]string{
		"auth/login.go":  "package auth\nfunc Login(){}",
		"api/handler.go": "package api\nfunc Handle(){ Login() }",
	})
	index("/wtB", map[string]string{
		"store/db.go":  "package store\nfunc Open(){}",
		"web/serve.go": "package web\nfunc Serve(){ Open() }",
	})
	arch, err := e.Architecture(ctx, "/wtA", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(moduleNamesOf(arch), ","); got != "api,auth" {
		t.Errorf("modules = %q, want api,auth", got)
	}
	if len(arch.Deps) != 1 || arch.Deps[0].From != "api" || arch.Deps[0].To != "auth" {
		t.Errorf("deps = %+v, want only api → auth", arch.Deps)
	}
}

func TestCodeMapDuringARefresh(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		nonGit bool
		first  func(e *Engine, wt string) error
		token  string
		opts   []ReconcileOption
	}{
		{"outside git, finished by an earlier version", true,
			func(e *Engine, wt string) error { return e.Reconcile(ctx, wt, FSSource{Root: wt}) }, "", nil},
		{"forced refresh of a changed git worktree", false,
			func(e *Engine, wt string) error { return e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, "tok") }, "tok",
			[]ReconcileOption{WithForce()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			wt := t.TempDir()
			writeFile(t, wt, "api/a.go", "package api\nfunc A(){}")
			if err := tc.first(e, wt); err != nil {
				t.Fatal(err)
			}
			writeFile(t, wt, "api/a.go", "package api\n\nfunc A() {}\n\nfunc B() {}\n")
			held := newHeldReadSource(wt)
			done := make(chan error, 1)
			go func() { done <- e.ReconcileIfStale(ctx, wt, held, tc.token, tc.opts...) }()
			held.awaitRead(t)

			arch, st, err := e.CodeMap(ctx, wt, tc.nonGit)
			if err != nil {
				t.Fatal(err)
			}
			if st != (MapStatus{Built: true}) || len(arch.Modules) != 0 {
				t.Errorf("during the refresh: status %+v, modules %v; want built, not done, no map", st, moduleNamesOf(arch))
			}

			held.release <- struct{}{}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(hangTimeout):
				t.Fatal("the refresh hung")
			}
			arch, st, err = e.CodeMap(ctx, wt, tc.nonGit)
			if err != nil {
				t.Fatal(err)
			}
			if st != (MapStatus{Done: true, Built: true}) || strings.Join(moduleNamesOf(arch), ",") != "api" {
				t.Errorf("after the refresh: status %+v, modules %v; want done with api", st, moduleNamesOf(arch))
			}
		})
	}
}
