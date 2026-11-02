package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// resolveSignature is a canonical, id-independent fingerprint of how every node,
// binding and edge in the worktree resolved: their own attributes plus the
// denormalized binding and the full candidate set, all keyed by stable identity
// (file path + qualified name + offsets) rather than the random node/edge ids that
// differ between two independent indexes. Two graphs with equal signatures
// resolved identically.
func resolveSignature(t *testing.T, e *Engine, wt string) string {
	t.Helper()
	ctx := context.Background()
	var lines []string

	// Every query selects a label first, then the columns joined with "|".
	// Offsets rather than ids keep two independent indexes comparable.
	queries := []string{`
SELECT 'NODE', f.path, n.kind, n.qualified_name, n.parent_name, n.signature,
       n.start_byte, n.end_byte, n.start_line, n.end_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?`, `
SELECT 'BIND', f.path, tb.name, tb.type_name, tb.scope_start_byte, tb.scope_end_byte
  FROM type_bindings tb JOIN files f ON f.id = tb.file_id
 WHERE f.worktree = ?`, `
SELECT 'EDGE', sf.path, COALESCE(sn.qualified_name, ''), COALESCE(sn.start_byte, -1),
       e.kind, e.dst_name, e.dst_recv, e.line,
       COALESCE(df.path, '·'), COALESCE(dn.qualified_name, '·'), COALESCE(dn.start_byte, -1)
  FROM edges e
  JOIN files sf ON sf.id = e.file_id
  LEFT JOIN nodes sn ON sn.id = e.src_node_id
  LEFT JOIN nodes dn ON dn.id = e.dst_node_id
  LEFT JOIN files df ON df.id = dn.file_id
 WHERE sf.worktree = ?`, `
SELECT 'CAND', sf.path, COALESCE(sn.qualified_name, ''), COALESCE(sn.start_byte, -1),
       e.kind, e.dst_name, e.dst_recv, e.line,
       cf.path, cn.qualified_name, cn.start_byte,
       printf('%.4f', ec.confidence), ec.method
  FROM edge_candidates ec
  JOIN edges e   ON e.id = ec.edge_id
  JOIN files sf  ON sf.id = e.file_id
  LEFT JOIN nodes sn ON sn.id = e.src_node_id
  JOIN nodes cn  ON cn.id = ec.dst_node_id
  JOIN files cf  ON cf.id = cn.file_id
 WHERE sf.worktree = ?`,
	}
	for _, q := range queries {
		rows, err := e.st.DB.QueryContext(ctx, q, wt)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			strs := make([]string, len(vals))
			for i, v := range vals {
				switch x := v.(type) {
				case nil:
					strs[i] = "·"
				case []byte:
					strs[i] = string(x)
				default:
					strs[i] = fmt.Sprint(x)
				}
			}
			lines = append(lines, strings.Join(strs, "|"))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
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
// resolution, cross-file references, renames, additions, deletions, and base
// changes, which are re-scoped incrementally.
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

	plainClassPaths := make([]string, 12)
	for i := range plainClassPaths {
		plainClassPaths[i] = fmt.Sprintf("bulk/C%02d.cs", i+1)
	}
	plainClass := func(i int, base string) string {
		return fmt.Sprintf("namespace App;\npublic class C%02d%s { public string Greet() { return \"x\"; } }\n", i+1, base)
	}
	commentedPaths := []string{"core/IGreeter.cs", "core/Greeter.cs", "svc/Service.cs"}

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
			// dispatch in Service must now include it, via the derived-class
			// method scope, and still match from-scratch.
			writeBumped(t, wt, "core/Shouter.cs",
				"namespace App;\npublic class Shouter : IGreeter {\n"+
					"  public string Greet() { return \"HEY\"; }\n}")
		}},
		{"existing-class-drops-interface", func() {
			// Same methods, only the base list changes: no name sequence moves,
			// so only the derived scope can catch Service's _greeter.Greet().
			writeBumped(t, wt, "core/Shouter.cs",
				"namespace App;\npublic class Shouter {\n"+
					"  public string Greet() { return \"HEY\"; }\n}")
		}},
		{"existing-class-regains-interface", func() {
			writeBumped(t, wt, "core/Shouter.cs",
				"namespace App;\npublic class Shouter : IGreeter {\n"+
					"  public string Greet() { return \"HEY\"; }\n}")
		}},
		{"rename-class", func() {
			// Greeter -> Welcomer, with the receiver's type binding updated.
			writeBumped(t, wt, "core/Greeter.cs",
				"namespace App;\npublic class Welcomer : IGreeter {\n"+
					"  public string Greet() { return Render(); }\n"+
					"  public string Render() { return \"hello there\"; }\n}")
			writeBumped(t, wt, "svc/Service.cs",
				"namespace App;\npublic class Service {\n"+
					"  private IGreeter _greeter;\n  private Welcomer _concrete;\n"+
					"  public void Run() {\n"+
					"    _greeter.Greet();\n    Helper.Make();\n    Helper.Reset();\n  }\n}")
		}},
		{"move-file", func() {
			b, err := os.ReadFile(filepath.Join(wt, "core/Shouter.cs"))
			if err != nil {
				t.Fatal(err)
			}
			writeBumped(t, wt, "impl/Shouters/Shouter.cs", string(b))
			if err := os.Remove(filepath.Join(wt, "core/Shouter.cs")); err != nil {
				t.Fatal(err)
			}
		}},
		{"add-twelve-plain-classes", func() {
			for i, rel := range plainClassPaths {
				writeBumped(t, wt, rel, plainClass(i, ""))
			}
		}},
		{"twelve-classes-gain-a-base", func() {
			// One reconcile, twelve new implements edges.
			for i, rel := range plainClassPaths {
				writeBumped(t, wt, rel, plainClass(i, " : IGreeter"))
			}
		}},
		{"comment-three-files", func() {
			for _, rel := range commentedPaths {
				b, err := os.ReadFile(filepath.Join(wt, rel))
				if err != nil {
					t.Fatal(err)
				}
				writeBumped(t, wt, rel, "// note\n"+string(b))
			}
		}},
		{"restore-three-files", func() {
			for _, rel := range commentedPaths {
				b, err := os.ReadFile(filepath.Join(wt, rel))
				if err != nil {
					t.Fatal(err)
				}
				writeBumped(t, wt, rel, strings.TrimPrefix(string(b), "// note\n"))
			}
		}},
		{"delete-file", func() {
			if err := os.Remove(filepath.Join(wt, "util/Helper.cs")); err != nil {
				t.Fatal(err)
			}
		}},
		{"restore-deleted-file", func() {
			writeBumped(t, wt, "util/Helper.cs",
				"namespace App;\npublic static class Helper {\n"+
					"  public static Greeter Make() { return new Greeter(); }\n"+
					"  public static void Reset() { }\n}")
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
	old, err := e.captureDirty(ctx, wt, []string{"A.cs"})
	if err != nil {
		t.Fatal(err)
	}
	// Re-index A.cs only (no resolve), mirroring what Reconcile's write phase does.
	content, _ := os.ReadFile(filepath.Join(wt, "A.cs"))
	if err := e.IndexFile(ctx, wt, "A.cs", content, int64(len(content)+1), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	newSeqs, err := nameSeqs(ctx, e.st.DB, wt, []string{"A.cs"})
	if err != nil {
		t.Fatal(err)
	}
	if ch := changedNames(old.seqs, newSeqs); len(ch) != 0 {
		t.Errorf("body-only edit re-resolves names %v, want none (stable ids)", ch)
	}
	if err := e.ResolveIncremental(ctx, wt, []string{"A.cs"}, old, nil); err != nil {
		t.Fatal(err)
	}
	gotInc := resolveSignature(t, e, wt)

	wantFull := resolveSignature(t, fromScratch(t, wt), wt)
	if gotInc != wantFull {
		t.Fatalf("direct incremental differs from full\n--- incremental ---\n%s\n--- full ---\n%s", gotInc, wantFull)
	}
}

// TestResolveIncrementalWritesOnlyChangedRows: a scoped resolve derives each
// affected edge's candidates afresh but writes only the difference. Adding an
// overload to Y moves the name M into scope; X.M's call re-resolves to the same
// candidate and must cost no write, while the previously unresolved z.M gains
// one.
func TestResolveIncrementalWritesOnlyChangedRows(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	writeBumped(t, wt, "x.cs", "class X { public void M() {} }\n")
	writeBumped(t, wt, "y.cs", "class Y { public void M() {} }\n")
	writeBumped(t, wt, "c.cs", "class C { void Go(X x) { x.M(); } void G2() { z.M(); } }\n")
	e := newTestEngine(t)
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	// A row-write log, so the test sees which rows the next reconcile touches.
	for _, q := range []string{
		`CREATE TABLE write_log(verb TEXT, recv TEXT)`,
		`CREATE TRIGGER cand_ins AFTER INSERT ON edge_candidates BEGIN
		   INSERT INTO write_log SELECT 'ins', COALESCE(dst_recv, '') FROM edges WHERE id = NEW.edge_id;
		 END`,
		`CREATE TRIGGER cand_del AFTER DELETE ON edge_candidates BEGIN
		   INSERT INTO write_log SELECT 'del', COALESCE(dst_recv, '') FROM edges WHERE id = OLD.edge_id;
		 END`,
		`CREATE TRIGGER edge_upd AFTER UPDATE OF dst_node_id ON edges BEGIN
		   INSERT INTO write_log VALUES ('upd', COALESCE(NEW.dst_recv, ''));
		 END`,
	} {
		if _, err := e.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	writeBumped(t, wt, "y.cs", "class Y { public void M() {} public void M(int i) {} }\n")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	rows, err := e.st.DB.Query(`SELECT verb, recv FROM write_log ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var verb, recv string
		if err := rows.Scan(&verb, &recv); err != nil {
			t.Fatal(err)
		}
		got = append(got, verb+" "+recv)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ins z"}; !slices.Equal(got, want) {
		t.Errorf("writes = %q, want %q (x.M's unchanged candidate must not be rewritten)", got, want)
	}
}
