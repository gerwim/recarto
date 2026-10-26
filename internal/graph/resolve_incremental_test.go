package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// resolveSignature is a canonical, id-independent fingerprint of how every edge in
// the worktree resolved: the denormalized binding plus the full candidate set, all
// keyed by stable identity (file path + qualified name) rather than the random
// node/edge ids that differ between two independent indexes. Two graphs with equal
// signatures resolved identically.
func resolveSignature(t *testing.T, e *Engine, wt string) string {
	t.Helper()
	ctx := context.Background()
	var lines []string

	rows, err := e.st.DB.QueryContext(ctx, `
SELECT sf.path, COALESCE(sn.qualified_name, ''), e.kind, e.dst_name, COALESCE(e.dst_recv, ''),
       COALESCE(df.path, '·'), COALESCE(dn.qualified_name, '·')
  FROM edges e
  JOIN files sf ON sf.id = e.file_id
  LEFT JOIN nodes sn ON sn.id = e.src_node_id
  LEFT JOIN nodes dn ON dn.id = e.dst_node_id
  LEFT JOIN files df ON df.id = dn.file_id
 WHERE sf.worktree = ?`, wt)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sp, sq, kind, dn, dr, dfp, dqn string
		if err := rows.Scan(&sp, &sq, &kind, &dn, &dr, &dfp, &dqn); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		lines = append(lines, strings.Join([]string{"EDGE", sp, sq, kind, dn, dr, "->", dfp, dqn}, "|"))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rows, err = e.st.DB.QueryContext(ctx, `
SELECT sf.path, COALESCE(sn.qualified_name, ''), e.dst_name, COALESCE(e.dst_recv, ''),
       cf.path, cn.qualified_name, ec.confidence, ec.method
  FROM edge_candidates ec
  JOIN edges e   ON e.id = ec.edge_id
  JOIN files sf  ON sf.id = e.file_id
  LEFT JOIN nodes sn ON sn.id = e.src_node_id
  JOIN nodes cn  ON cn.id = ec.dst_node_id
  JOIN files cf  ON cf.id = cn.file_id
 WHERE sf.worktree = ?`, wt)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sp, sq, dn, dr, cfp, cqn, method string
		var conf float64
		if err := rows.Scan(&sp, &sq, &dn, &dr, &cfp, &cqn, &conf, &method); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		lines = append(lines, strings.Join([]string{
			"CAND", sp, sq, dn, dr, cfp, cqn, method, fmt.Sprintf("%.4f", conf),
		}, "|"))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// fromScratch builds a brand-new engine (fresh store) and fully indexes wt — the
// gold-standard resolution the incremental path must reproduce exactly.
func fromScratch(t *testing.T, wt string) *Engine {
	t.Helper()
	e := newTestEngine(t)
	if err := e.Reconcile(context.Background(), wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	return e
}

// writeBumped writes content and forces a strictly-increasing mtime, so the
// reconcile mtime/size pre-check always sees the edit even when a rewrite keeps the
// byte length identical (a same-length rename) within one filesystem mtime tick.
var bumpClock = time.Unix(1_700_000_000, 0)

func writeBumped(t *testing.T, dir, rel, content string) {
	t.Helper()
	writeFile(t, dir, rel, content)
	bumpClock = bumpClock.Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, rel), bumpClock, bumpClock); err != nil {
		t.Fatal(err)
	}
}

// TestResolveIncrementalMatchesFullResolve drives a worktree through a sequence of
// realistic edits, reconciling after each, and asserts the incrementally-resolved
// graph is byte-for-byte equivalent (by stable identity) to a from-scratch full
// index of the same files — across self/typed/static/interface/name-fallback
// resolution, cross-file references, renames, additions, deletions, and a
// structural change that must trigger the full-resolve fallback.
func TestResolveIncrementalMatchesFullResolve(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()

	writeBumped(t, wt, "core/IGreeter.cs",
		"namespace App;\npublic interface IGreeter { string Greet(); }")
	writeBumped(t, wt, "core/Greeter.cs",
		"namespace App;\npublic class Greeter : IGreeter {\n"+
			"  public string Greet() { return Format(); }\n"+
			"  public string Format() { return \"hi\"; }\n}")
	writeBumped(t, wt, "util/Helper.cs",
		"namespace App;\npublic static class Helper {\n"+
			"  public static Greeter Make() { return new Greeter(); }\n}")
	writeBumped(t, wt, "svc/Service.cs",
		"namespace App;\npublic class Service {\n"+
			"  private IGreeter _greeter;\n  private Greeter _concrete;\n"+
			"  public void Run() {\n"+
			"    _greeter.Greet();\n    _concrete.Format();\n    Helper.Make();\n    Orphan();\n  }\n}")

	e := newTestEngine(t)
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name string
		mut  func()
	}{
		{"edit-method-body", func() {
			writeBumped(t, wt, "core/Greeter.cs",
				"namespace App;\npublic class Greeter : IGreeter {\n"+
					"  public string Greet() { return Format(); }\n"+
					"  public string Format() { return \"hello there\"; }\n}")
		}},
		{"rename-called-method", func() {
			// Format -> Render, updating the self-call but NOT Service._concrete.Format()
			// (which must become unresolved — exactly as a from-scratch index sees it).
			writeBumped(t, wt, "core/Greeter.cs",
				"namespace App;\npublic class Greeter : IGreeter {\n"+
					"  public string Greet() { return Render(); }\n"+
					"  public string Render() { return \"hello there\"; }\n}")
		}},
		{"add-method-and-caller", func() {
			writeBumped(t, wt, "util/Helper.cs",
				"namespace App;\npublic static class Helper {\n"+
					"  public static Greeter Make() { return new Greeter(); }\n"+
					"  public static void Reset() { }\n}")
			writeBumped(t, wt, "svc/Service.cs",
				"namespace App;\npublic class Service {\n"+
					"  private IGreeter _greeter;\n  private Greeter _concrete;\n"+
					"  public void Run() {\n"+
					"    _greeter.Greet();\n    Helper.Make();\n    Helper.Reset();\n  }\n}")
		}},
		{"add-second-implementer-structural", func() {
			// A new IGreeter implementer changes `extends` structure: interface
			// dispatch in Service must now include it. Must hit the full-resolve
			// fallback and still match from-scratch.
			writeBumped(t, wt, "core/Shouter.cs",
				"namespace App;\npublic class Shouter : IGreeter {\n"+
					"  public string Greet() { return \"HEY\"; }\n}")
		}},
		{"delete-file", func() {
			if err := os.Remove(filepath.Join(wt, "util/Helper.cs")); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, step := range steps {
		step.mut()
		if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
			t.Fatalf("%s: reconcile: %v", step.name, err)
		}
		got := resolveSignature(t, e, wt)
		want := resolveSignature(t, fromScratch(t, wt), wt)
		if got != want {
			t.Fatalf("%s: incremental resolution differs from full reindex\n--- incremental ---\n%s\n--- from-scratch ---\n%s",
				step.name, got, want)
		}
	}
}

// TestResolveIncrementalDirectVsFull pits ResolveIncremental against a full Resolve
// on the very same store and end-state, isolating the scoped SQL from the
// affected-set bookkeeping: after editing one file, resolving incrementally must
// leave the store identical to resolving it fully.
func TestResolveIncrementalDirectVsFull(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	writeBumped(t, wt, "A.cs",
		"namespace P;\npublic class A {\n  public void Go(){ Help(); }\n  public void Help(){}\n}")
	writeBumped(t, wt, "B.cs",
		"namespace P;\npublic class B {\n  private A _a;\n  public void Run(){ _a.Help(); }\n}")

	e := newTestEngine(t)
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// Edit A so its rows (and ids) are replaced, leaving B's cross-file edge to
	// A.Help needing rebind — the textbook incremental case.
	writeBumped(t, wt, "A.cs",
		"namespace P;\npublic class A {\n  public void Go(){ Help(); }\n  public void Help(){ /* edited */ }\n}")
	oldNames, oldImports, err := e.namesForPaths(ctx, wt, []string{"A.cs"})
	if err != nil {
		t.Fatal(err)
	}
	// Re-index A.cs only (no resolve), mirroring what Reconcile's write phase does.
	content, _ := os.ReadFile(filepath.Join(wt, "A.cs"))
	if err := e.IndexFile(ctx, wt, "A.cs", content, int64(len(content)+1), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := e.ResolveIncremental(ctx, wt, []string{"A.cs"}, oldNames, oldImports); err != nil {
		t.Fatal(err)
	}
	gotInc := resolveSignature(t, e, wt)

	wantFull := resolveSignature(t, fromScratch(t, wt), wt)
	if gotInc != wantFull {
		t.Fatalf("direct incremental differs from full\n--- incremental ---\n%s\n--- full ---\n%s", gotInc, wantFull)
	}
}
