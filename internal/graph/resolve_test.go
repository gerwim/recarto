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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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

// TestResolveS6ImplicitCtorBindsType: `new Foo()` on a class with no explicit
// constructor emits a calls edge named after the type that no callable-binding
// strategy (S1–S5) can resolve; S6 must bind it to the type declaration itself.
func TestResolveS6ImplicitCtorBindsType(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\nclass Foo { }\nclass Svc { void M() { var x = new Foo(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Foo", "")
	if got["N.Foo"] != "ctor" {
		t.Errorf("new Foo() should bind ctor to the class N.Foo; got %v", got)
	}
	var qn, method string
	if err := e.st.DB.QueryRow(`
SELECT n.qualified_name, ec.method FROM edges ed
  JOIN nodes n ON n.id = ed.dst_node_id
  JOIN edge_candidates ec ON ec.edge_id = ed.id AND ec.dst_node_id = ed.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name='Foo'`).Scan(&qn, &method); err != nil {
		t.Fatal(err)
	}
	if qn != "N.Foo" || method != "ctor" {
		t.Errorf("denormalized new Foo() = %q via %q, want N.Foo via ctor", qn, method)
	}
}

// TestResolveS6SkipsCSharpMemberCall: C# constructs only via `new`, so
// items.Add(x) is never a constructor call of a class named Add.
func TestResolveS6SkipsCSharpMemberCall(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\nclass Add { }\nclass Svc { void M() { items.Add(1); var a = new Add(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	if got := candidateTargets(t, e, "Add", "items"); len(got) != 0 {
		t.Errorf("items.Add(1) should not bind to class Add; got %v", got)
	}
	if got := candidateTargets(t, e, "Add", ""); got["N.Add"] != "ctor" {
		t.Errorf("new Add() should still bind ctor to N.Add; got %v", got)
	}
}

// TestResolveS6SuppressedByEarlierCandidate: S6's NOT EXISTS keeps it strictly a
// fallback — an edge that already earned a candidate (here an S5 name match to a
// same-named method) must NOT also gain a 'ctor' candidate to the same-named type.
func TestResolveS6SuppressedByEarlierCandidate(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// class Widget (an S6 target) and method Widget() (an S5 target) share the name.
	// unknownVar.Widget() has no resolvable receiver type, so S2–S4 miss and S5 binds
	// it by name to Helper.Widget; S6 must then be suppressed.
	src := []byte("namespace N;\n" +
		"class Widget { }\n" +
		"class Helper { public void Widget() {} }\n" +
		"class Svc { void M() { unknownVar.Widget(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Widget", "unknownVar")
	if got["N.Helper.Widget"] != "name" {
		t.Errorf("unknownVar.Widget() should bind name to N.Helper.Widget; got %v", got)
	}
	for qn, method := range got {
		if method == "ctor" {
			t.Errorf("S6 must be suppressed when a candidate already exists; got ctor -> %q", qn)
		}
	}
}

// TestResolveS6StrictLanguageEquality: S6 (unlike S2/S3/S5) has no TS/JS family
// widening — it binds only when the call site and the type share an EXACT language.
// A C# `new Widget()` must not bind to a TypeScript class Widget.
func TestResolveS6StrictLanguageEquality(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	cs := []byte("namespace N;\nclass Svc { void M() { var x = new Widget(); } }")
	if err := e.IndexFile(ctx, "/wt", "api/Svc.cs", cs, 1, int64(len(cs))); err != nil {
		t.Fatal(err)
	}
	ts := []byte("export class Widget {}")
	if err := e.IndexFile(ctx, "/wt", "web/widget.tsx", ts, 1, int64(len(ts))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	if got := candidateTargets(t, e, "Widget", ""); len(got) != 0 {
		t.Errorf("C# new Widget() must not bind to a cross-language type; got %v", got)
	}
	var dst string
	if err := e.st.DB.QueryRow(`
SELECT COALESCE(dst_node_id,'') FROM edges WHERE kind='calls' AND dst_name='Widget'`).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if dst != "" {
		t.Errorf("cross-language new Widget() resolved to %q, want unresolved", dst)
	}
}

// TestResolveS6MultipleTypesKeepAllCandidates: two same-name types in the same
// language both stand in for their implicit constructor, so a `new Widget()` with
// no resolvable single target keeps BOTH ctor candidate rows (the denormalize then
// picks one winner by locality).
func TestResolveS6MultipleTypesKeepAllCandidates(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	a := []byte("namespace A;\nclass Widget { }")
	if err := e.IndexFile(ctx, "/wt", "a/A.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	b := []byte("namespace B;\nclass Widget { }")
	if err := e.IndexFile(ctx, "/wt", "b/B.cs", b, 1, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	svc := []byte("namespace S;\nclass Svc { void M() { var x = new Widget(); } }")
	if err := e.IndexFile(ctx, "/wt", "s/Svc.cs", svc, 1, int64(len(svc))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	got := candidateTargets(t, e, "Widget", "")
	if got["A.Widget"] != "ctor" || got["B.Widget"] != "ctor" {
		t.Errorf("new Widget() should keep both A.Widget and B.Widget ctor candidates; got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want exactly 2 ctor candidates, got %d: %v", len(got), got)
	}
}

// TestResolveDenormalizeFoldMatchesTierOrder is the differential guard for the
// fold-based denormalize: with candidates spanning several tiers on different
// edges, edges.dst_node_id must land on the documented winner —
// (confidence DESC, same-file, same-top-dir, path, start, id).
func TestResolveDenormalizeFoldMatchesTierOrder(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()

	// Edge 1 (multi-tier, S1 self 0.95 beats S3 static 0.9): inside Svc, a bare
	// Run() binds S1 to the sibling Svc.Run; Svc.Run() (static, same-name method on
	// a same-name type reference) is a lower-confidence competitor. The winner must
	// be the S1 self candidate.
	multi := []byte("namespace N;\n" +
		"class Svc { void Run() {} void M() { Run(); Svc.Run(); } }")
	if err := e.IndexFile(ctx, "/wt", "svc.cs", multi, 1, int64(len(multi))); err != nil {
		t.Fatal(err)
	}
	// Edge 2 (S5-only, same-file beats cross-dir): two funcs named Pick; the caller
	// shares a file with one of them, so locality must pick the same-file target.
	sameFile := []byte("package p\nfunc Caller(){ Pick() }\nfunc Pick(){}\n")
	if err := e.IndexFile(ctx, "/wt", "pkg/local.go", sameFile, 1, int64(len(sameFile))); err != nil {
		t.Fatal(err)
	}
	crossDir := []byte("package p\nfunc Pick(){}\n")
	if err := e.IndexFile(ctx, "/wt", "other/remote.go", crossDir, 1, int64(len(crossDir))); err != nil {
		t.Fatal(err)
	}
	// Edge 3 (S6-only): new Gadget() with no explicit ctor binds to the class.
	ctor := []byte("namespace G;\nclass Gadget { }\nclass Use { void M() { var x = new Gadget(); } }")
	if err := e.IndexFile(ctx, "/wt", "gadget.cs", ctor, 1, int64(len(ctor))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}

	winnerOf := func(dstName, recv string) (qn, method, path string) {
		t.Helper()
		if err := e.st.DB.QueryRow(`
SELECT n.qualified_name, COALESCE(ec.method,''), f.path
  FROM edges ed
  JOIN nodes n ON n.id = ed.dst_node_id
  JOIN files f ON f.id = n.file_id
  LEFT JOIN edge_candidates ec ON ec.edge_id = ed.id AND ec.dst_node_id = ed.dst_node_id
 WHERE ed.kind='calls' AND ed.dst_name=? AND ed.dst_recv=?`, dstName, recv).Scan(&qn, &method, &path); err != nil {
			t.Fatalf("winner %s.%s: %v", recv, dstName, err)
		}
		return
	}

	if qn, method, _ := winnerOf("Run", ""); qn != "N.Svc.Run" || method != "self" {
		t.Errorf("edge1 winner = %q via %q, want N.Svc.Run via self (highest confidence)", qn, method)
	}
	if _, method, path := winnerOf("Pick", ""); path != "pkg/local.go" || method != "name" {
		t.Errorf("edge2 winner path = %q via %q, want pkg/local.go via name (same-file locality)", path, method)
	}
	if qn, method, _ := winnerOf("Gadget", ""); qn != "G.Gadget" || method != "ctor" {
		t.Errorf("edge3 winner = %q via %q, want G.Gadget via ctor", qn, method)
	}
}

// TestResolveLeavesNoForeignKeyViolations is the safety net for WriteTxBulk running
// the resolve transaction with foreign keys OFF: after a full resolve, an integrity
// check of edge_candidates must report zero orphaned rows.
func TestResolveLeavesNoForeignKeyViolations(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace N;\n" +
		"class Dep { public void Run() {} }\n" +
		"class Svc { private readonly Dep _dep; void M() { _dep.Run(); var x = new Dep(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}
	rows, err := e.st.DB.Query(`PRAGMA foreign_key_check(edge_candidates)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("PRAGMA foreign_key_check(edge_candidates) reported orphaned rows after resolve")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
