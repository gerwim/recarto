package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var rustShapes = []byte(`pub trait Area {
    fn area(&self) -> f64;
}

pub struct Circle { pub r: f64 }

impl Circle {
    pub fn new(r: f64) -> Self { Self::make(r) }
    fn make(r: f64) -> Self { Circle { r } }
    fn len(&self) -> usize { len(self.r) }
}

impl Area for Circle {
    fn area(&self) -> f64 { self.r * self.r }
}

pub mod geo {
    pub struct Square;
    impl Square { pub fn side(&self) -> f64 { 1.0 } }
}

fn len(x: f64) -> usize { x as usize }

pub trait Measure<T> {
    fn measure(&self) -> T;
}

impl Measure<f64> for Circle {
    fn measure(&self) -> f64 { self.r }
}
`)

var rustMain = []byte(`use crate::shapes::{Area, Circle};

fn total(s: &dyn Area) -> f64 { s.area() }

fn main() {
    let c = Circle::new(2.0);
    println!("{}", c.area());
    total(&c);
}

fn measured(input: &dyn Measure<f64>) -> f64 {
    let m: &dyn Measure<f64> = input;
    m.measure() + crate::helper::<u32>()
}

fn helper<T>() -> f64 { 0.0 }

#[cfg(test)]
mod tests {
    #[test]
    fn checks() { assert_eq!(super::total(&Circle::new(1.0)), 1.0); }
}
`)

// callCandidates lists every resolved call as "caller -> target (method)".
func callCandidates(t *testing.T, e *Engine) []string {
	t.Helper()
	rows, err := e.st.DB.Query(`
SELECT sn.qualified_name, dn.qualified_name, ec.method
  FROM edges ed
  JOIN nodes sn ON sn.id = ed.src_node_id
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes dn ON dn.id = ec.dst_node_id
 WHERE ed.kind = 'calls'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, dst, method string
		if err := rows.Scan(&src, &dst, &method); err != nil {
			t.Fatal(err)
		}
		out = append(out, src+" -> "+dst+" ("+method+")")
	}
	slices.Sort(out)
	return out
}

// TestRustGraph indexes a two-file crate and checks what the graph tools see:
// impl methods qualify as Type.method, self/Self/let receivers resolve typed,
// trait-typed receivers dispatch to impls, calls inside macros resolve, a bare
// call never binds a same-named method, and impl blocks stay out of the way of
// their type in snippet lookups.
func TestRustGraph(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir() // snippets read source back from the worktree
	files := map[string][]byte{
		"src/shapes.rs": rustShapes, "src/main.rs": rustMain,
		// more impl blocks of Circle, each in its own file
		"src/fmt.rs": []byte("impl std::fmt::Display for Circle {}\n"),
		"src/eq.rs":  []byte("impl PartialEq for Circle {}\n"),
	}
	for path, src := range files {
		full := filepath.Join(wt, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := e.IndexFile(ctx, wt, path, src, 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatal(err)
	}

	got := callCandidates(t, e)
	for _, want := range []string{
		"Circle.new -> Circle.make (typed)",   // Self::make via the impl's Self binding
		"Circle.len -> len (name)",            // bare len(...) is the free function
		"total -> Area.area (typed)",          // s: &dyn Area
		"total -> Circle.area (iface)",        // ... dispatched to the trait impl
		"main -> Circle.new (static)",         // Circle::new
		"main -> Circle.area (typed)",         // c.area() inside println!, c = Circle::new(..)
		"main -> total (name)",                //
		"tests.checks -> total (name)",        // inside assert_eq!
		"tests.checks -> Circle.new (static)", // Circle::new inside assert_eq!
		"measured -> Measure.measure (typed)", // let m: &dyn Measure<f64>
		"measured -> Circle.measure (iface)",  // ... dispatched to the trait impl
		"measured -> helper (name)",           // crate::helper::<u32>()
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing candidate %q", want)
		}
	}
	for _, method := range []string{"self", "name"} {
		if slices.Contains(got, "Circle.len -> Circle.len ("+method+")") {
			t.Errorf("bare len(...) bound to the method Circle.len (%s); a bare Rust call never names a method", method)
		}
	}
	if t.Failed() {
		t.Logf("candidates:\n%s", strings.Join(got, "\n"))
	}

	// The incremental resolver runs the same strategies scoped to an edit.
	if err := e.ReindexFileIncremental(ctx, wt, "src/main.rs", rustMain, 2, int64(len(rustMain))); err != nil {
		t.Fatal(err)
	}
	if again := callCandidates(t, e); !slices.Equal(again, got) {
		t.Errorf("incremental re-resolve diverged:\n got %v\nwant %v", again, got)
	}

	impls, err := e.Implementations(ctx, wt, "Area", "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(impls) != 1 || impls[0].QualifiedName != "Circle" || impls[0].Kind != "impl" || impls[0].Line != 13 {
		t.Errorf("Implementations(Area) = %+v, want the impl Circle block at line 13", impls)
	}
	sup, err := e.Implementations(ctx, wt, "Circle", "up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !relNames(sup)["Area"] {
		t.Errorf("Implementations(Circle, up) = %v, want Area", keys(relNames(sup)))
	}

	// struct Circle + four impl Circle blocks share one qualified name; the
	// lookup returns the struct, not an ambiguity prompt.
	snip, cands, err := e.SnippetByName(ctx, wt, "Circle")
	if err != nil {
		t.Fatal(err)
	}
	if !snip.Found || !strings.HasPrefix(snip.Snippet, "pub struct Circle") {
		t.Errorf("SnippetByName(Circle) = %+v, candidates %+v; want the struct", snip, cands)
	}
	if snip, _, _ := e.SnippetByName(ctx, wt, "geo.Square.side"); !snip.Found {
		t.Error("SnippetByName(geo.Square.side) not found; methods in inline modules qualify as mod.Type.method")
	}

	res, err := e.Search(ctx, wt, "Circle", "", "rust", 10)
	if err != nil {
		t.Fatal(err)
	}
	impl := 0
	for _, r := range res {
		if r.Kind == "impl" {
			impl++
		}
	}
	if len(res) == 0 || res[0].Kind != "struct" || impl != 1 || res[len(res)-1].Kind != "impl" {
		t.Errorf("Search(Circle): want the struct first and the impl blocks folded to one last row, got %+v", res)
	}

	ann, err := e.Annotated(ctx, wt, "test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ann) != 1 || ann[0].QualifiedName != "tests.checks" {
		t.Errorf("Annotated(test) = %+v, want tests.checks", ann)
	}
}

// TestRustPathSymbols: Rust writes item paths with "::" (Body::let_go) while
// qualified names are dotted (Body.let_go); every symbol lookup accepts both.
func TestRustPathSymbols(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	src := []byte(`pub struct Body;

impl Body {
    pub fn let_go(&self) {}
}

pub mod geo {
    pub struct Square;
    impl Clone for Square { fn clone(&self) -> Self { Square } }
}

fn run(b: &Body) { b.let_go(); }
`)
	if err := os.MkdirAll(filepath.Join(wt, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "src", "lib.rs"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.IndexFile(ctx, wt, "src/lib.rs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatal(err)
	}

	if snip, cands, err := e.SnippetByName(ctx, wt, "Body::let_go"); err != nil || !snip.Found || snip.QualifiedName != "Body.let_go" {
		t.Errorf("SnippetByName(Body::let_go) = %+v, candidates %+v, err %v; want Body.let_go", snip, cands, err)
	}
	if snip, err := e.Snippet(ctx, wt, "src/lib.rs", "Body::let_go"); err != nil || !snip.Found || snip.QualifiedName != "Body.let_go" {
		t.Errorf("Snippet(src/lib.rs, Body::let_go) = %+v, err %v; want Body.let_go", snip, err)
	}
	chains, sugg, err := e.TraceCallPathFiltered(ctx, wt, "Body::let_go", "callers", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	for _, ch := range chains {
		if len(ch) >= 2 && ch[1].Name == "run" {
			called = true
		}
	}
	if !called {
		t.Errorf("callers(Body::let_go) = %+v, suggestions %v; want run", chains, sugg)
	}
	refs, meta, err := e.References(ctx, wt, "Body::let_go", 10)
	if err != nil {
		t.Fatal(err)
	}
	fromRun := false
	for _, r := range refs {
		if r.Kind == "calls" && r.From == "run" {
			fromRun = true
		}
	}
	if !meta.Resolved || !fromRun {
		t.Errorf("References(Body::let_go) = %+v (resolved %v); want the calls edge from run", refs, meta.Resolved)
	}
	rels, err := e.Implementations(ctx, wt, "geo::Square", "up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !relNames(rels)["Clone"] {
		t.Errorf("Implementations(geo::Square, up) = %+v; want Clone", rels)
	}
	nodes, err := e.Query(ctx, wt, QuerySpec{NameLike: "Body::let_go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].QualifiedName != "Body.let_go" {
		t.Errorf("Query(name Body::let_go) = %+v; want Body.let_go", nodes)
	}
}
