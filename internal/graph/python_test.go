package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var pythonShapes = []byte(`from abc import ABC, abstractmethod


class Shape(ABC):
    def __init__(self, name: str):
        self.name = name

    @abstractmethod
    def area(self) -> float: ...

    def describe(self) -> str:
        return self.label() + str(self.area())

    def label(self) -> str:
        return self.name


class Circle(Shape):
    def __init__(self, r: float):
        super().__init__("circle")
        self.r = r

    def area(self) -> float:
        return area(self.r)

    @classmethod
    def unit(cls) -> "Circle":
        return cls.make(1.0)

    @staticmethod
    def make(r: float) -> "Circle":
        return Circle(r)


def area(r: float) -> float:
    return 3.14 * r * r


class Disc(Circle):
    def shade(self) -> float:
        return self.area() + len(self.describe())
`)

var pythonService = []byte(`from typing import Optional

from geo.shapes import Circle, Disc, Shape


class Repo:
    def save(self, s: Shape) -> None: ...


class Service:
    store: Repo

    def __init__(self, repo: Repo, cache: Optional[Repo] = None):
        self.repo = repo
        self.backup = Repo()

    def run(self, s: Shape) -> float:
        self.repo.save(s)
        self.store.save(s)
        self.backup.save(s)
        return s.area()


def main():
    c = Circle.unit()
    svc = Service(Repo())
    svc.run(c)
    total(c)


def total(s: Shape | None) -> str:
    return s.describe()


def show(c: Circle) -> str:
    return c.describe() + str(Disc.unit())
`)

// TestPythonGraph indexes a two-module package and checks what the graph tools
// see: methods qualify as Class.method; self, cls, typed parameters and locals,
// and self.x attributes resolve typed, inherited methods included; base-typed
// receivers dispatch to overrides; super() binds the base class's method; a
// class call binds the class; and a bare call never binds a method.
func TestPythonGraph(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir() // snippets read source back from the worktree
	files := map[string][]byte{"geo/shapes.py": pythonShapes, "app/service.py": pythonService}
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
		"Shape.describe -> Shape.label (typed)",     // self.label()
		"Shape.describe -> Shape.area (typed)",      // self.area() ...
		"Shape.describe -> Circle.area (iface)",     // ... and the override it may dispatch to
		"Circle.__init__ -> Shape.__init__ (typed)", // super().__init__()
		"Circle.area -> area (name)",                // bare area() is the module function
		"Circle.unit -> Circle.make (typed)",        // cls.make()
		"Circle.make -> Circle (ctor)",              // Circle(r)
		"Service.__init__ -> Repo (ctor)",
		"Service.run -> Shape.area (typed)",
		"Service.run -> Circle.area (iface)",
		"main -> Circle.unit (static)",
		"main -> Service (ctor)",
		"main -> Service.run (typed)", // svc = Service(...)
		"main -> total (name)",
		"total -> Shape.describe (typed)", // s: Shape | None
		// inherited: the nearest ancestor declaring the method
		"Disc.shade -> Circle.area (typed)",
		"Disc.shade -> Shape.describe (typed)",
		"show -> Shape.describe (typed)", // c: Circle
		"show -> Circle.unit (typed)",    // Disc.unit()
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing candidate %q", want)
		}
	}
	// self.repo (= the annotated parameter), self.store (a class-level annotation)
	// and self.backup (= Repo()) all type the receiver of their save() call.
	if n := strings.Count(strings.Join(got, "\n"), "Service.run -> Repo.save (typed)"); n != 3 {
		t.Errorf("Service.run -> Repo.save (typed) %d times, want 3 (self.repo, self.store, self.backup)", n)
	}
	for _, c := range got {
		if strings.HasPrefix(c, "Circle.area -> ") && c != "Circle.area -> area (name)" {
			t.Errorf("bare area() bound to %q; a bare Python call never names a method", c)
		}
	}
	if slices.Contains(got, "Disc.shade -> Shape.area (typed)") {
		t.Error("Disc's self.area() bound to Shape.area; Circle.area overrides it")
	}
	if t.Failed() {
		t.Logf("candidates:\n%s", strings.Join(got, "\n"))
	}

	// The incremental resolver runs the same strategies scoped to an edit.
	for path, src := range files {
		if err := e.ReindexFileIncremental(ctx, wt, path, src, 2, int64(len(src))); err != nil {
			t.Fatal(err)
		}
		if again := callCandidates(t, e); !slices.Equal(again, got) {
			t.Errorf("incremental re-resolve of %s diverged:\n got %v\nwant %v", path, again, got)
		}
	}

	impls, err := e.Implementations(ctx, wt, "Shape", "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(impls) != 1 || impls[0].QualifiedName != "Circle" || impls[0].Line != 18 {
		t.Errorf("Implementations(Shape) = %+v, want class Circle at line 18", impls)
	}
	sup, err := e.Implementations(ctx, wt, "Circle", "up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !relNames(sup)["Shape"] {
		t.Errorf("Implementations(Circle, up) = %v, want Shape", keys(relNames(sup)))
	}

	// A decorated method's snippet starts at its decorator; its line is the def's.
	snip, cands, err := e.SnippetByName(ctx, wt, "Circle.unit")
	if err != nil {
		t.Fatal(err)
	}
	if !snip.Found || !strings.HasPrefix(snip.Snippet, "@classmethod\n    def unit(cls)") || snip.StartLine != 27 {
		t.Errorf("SnippetByName(Circle.unit) = %+v, candidates %+v; want the decorated method", snip, cands)
	}

	for _, attr := range []string{"abstractmethod", "@abstractmethod", "abc.abstractmethod"} {
		ann, err := e.Annotated(ctx, wt, attr, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ann) != 1 || ann[0].QualifiedName != "Shape.area" {
			t.Errorf("Annotated(%s) = %+v, want Shape.area", attr, ann)
		}
	}

	res, err := e.Search(ctx, wt, "area", "", "python", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range res {
		found[r.Kind+" "+r.QualifiedName] = true
	}
	for _, want := range []string{"function area", "method Shape.area", "method Circle.area"} {
		if !found[want] {
			t.Errorf("Search(area) misses %s: %+v", want, res)
		}
	}
}

// TestPythonResolutionScoping pins where a Python receiver's class is looked up:
// among Python classes only, and in the class the call's own file declares when
// it declares one of that name — for direct, inherited and override candidates
// alike. And every method's self is its class, whatever block declares it.
func TestPythonResolutionScoping(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        map[string]string
		want, forbid string
	}{
		{
			name: "inherited lookup stays in Python",
			files: map[string]string{
				"app.py":     "from model import Foreign\ndef run(x: Foreign):\n    x.target()\n",
				"model.py":   "class PyBase:\n    def target(self): pass\nclass Foreign(PyBase): pass\n",
				"foreign.cs": "class CsBase { public void target() {} } class Foreign : CsBase {}",
			},
			want:   "run -> PyBase.target (typed)",
			forbid: "run -> CsBase.target (typed)",
		},
		{
			name: "override dispatch stays in Python",
			files: map[string]string{
				"app.py":   "from model import Base\ndef run(x: Base):\n    x.target()\n",
				"model.py": "class Base:\n    def target(self): pass\n",
				"base.cs":  "class Base {} class CsSub : Base { public void target() {} }",
			},
			want:   "run -> Base.target (typed)",
			forbid: "run -> CsSub.target (iface)",
		},
		{
			name: "the local class shadows a same-named one for self",
			files: map[string]string{
				"z_local.py": "from z_base import Base\nclass Local(Base):\n    def run(self):\n        self.target()\n",
				"z_base.py":  "class Base:\n    def target(self): pass\n",
				"a_other.py": "class Local:\n    def target(self): pass\n",
			},
			want:   "Local.run -> Base.target (typed)",
			forbid: "Local.run -> Local.target (typed)",
		},
		{
			name: "the local class shadows a same-named one for a static call",
			files: map[string]string{
				"z_local.py": "from z_base import Base\nclass Local(Base): pass\ndef run():\n    Local.target()\n",
				"z_base.py":  "class Base:\n    def target(self): pass\n",
				"a_other.py": "class Local:\n    def target(self): pass\n",
			},
			want:   "run -> Base.target (typed)",
			forbid: "run -> Local.target (static)",
		},
		{
			name: "the local class shadows a same-named one for overrides",
			files: map[string]string{
				"z_local.py": "class Local:\n    def target(self): pass\n    def run(self):\n        self.target()\n",
				"a_other.py": "class Local:\n    pass\nclass Sub(Local):\n    def target(self): pass\n",
			},
			want:   "Local.run -> Local.target (typed)",
			forbid: "Local.run -> Sub.target (iface)",
		},
		{
			name: "a method under a class-body if binds self",
			files: map[string]string{
				"app.py": "class A:\n    def target(self): pass\nclass C:\n    def target(self): pass\n    if True:\n        def run(self):\n            self.target()\n",
			},
			want:   "C.run -> C.target (typed)",
			forbid: "C.run -> A.target (name)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t)
			ctx := context.Background()
			wt := t.TempDir()
			for path, src := range tc.files {
				if err := e.IndexFile(ctx, wt, path, []byte(src), 1, int64(len(src))); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
				t.Fatal(err)
			}
			check := func(mode string) {
				t.Helper()
				got := callCandidates(t, e)
				if !slices.Contains(got, tc.want) || slices.Contains(got, tc.forbid) {
					t.Errorf("%s resolve: candidates %v, want %q and not %q", mode, got, tc.want, tc.forbid)
				}
			}
			check("full")
			for path, src := range tc.files {
				if err := e.ReindexFileIncremental(ctx, wt, path, []byte(src), 2, int64(len(src))); err != nil {
					t.Fatal(err)
				}
			}
			check("incremental")
		})
	}
}

// pythonTargets lists every candidate of the given edge kind (calls: from
// edge_candidates; others: the resolved target) as "src -> dst @path (method)",
// the path telling same-named classes apart.
func pythonTargets(t *testing.T, e *Engine, kind string) []string {
	t.Helper()
	q := `
SELECT COALESCE(sn.qualified_name, '<file>'), dn.qualified_name, df.path, ec.method
  FROM edges ed
  LEFT JOIN nodes sn ON sn.id = ed.src_node_id
  JOIN edge_candidates ec ON ec.edge_id = ed.id
  JOIN nodes dn ON dn.id = ec.dst_node_id JOIN files df ON df.id = dn.file_id
 WHERE ed.kind = 'calls'`
	if kind != "calls" {
		q = `
SELECT COALESCE(sn.qualified_name, '<file>'), dn.qualified_name, df.path, ed.kind
  FROM edges ed
  LEFT JOIN nodes sn ON sn.id = ed.src_node_id
  JOIN nodes dn ON dn.id = ed.dst_node_id JOIN files df ON df.id = dn.file_id
 WHERE ed.kind = '` + kind + `'`
	}
	rows, err := e.st.DB.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, dst, path, method string
		if err := rows.Scan(&src, &dst, &path, &method); err != nil {
			t.Fatal(err)
		}
		out = append(out, src+" -> "+dst+" @"+path+" ("+method+")")
	}
	slices.Sort(out)
	return out
}

// TestPythonModules: an import or base resolves to the class of the module it
// names, even when another module (a_decoy.py, first in path order) or the
// deriving class itself has the same name — the `from m import X as Base;
// class X(Base)` and `class X(m.X)` idioms — and super() binds through the
// class's bases.
func TestPythonModules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	files := map[string]string{
		"pkg/a_decoy.py": "class Store:\n    def save(self): pass\n    def load(self): pass\n\n\nclass Serializer:\n    def serialize(self): pass\n    def end(self): pass\n",
		"pkg/db.py":      "class Store:\n    def save(self): pass\n    def load(self): pass\n",
		"pkg/cached.py":  "from pkg.db import Store as DBStore\n\n\nclass Store(DBStore):\n    def save(self):\n        super().save()\n        self.load()\n",
		"pkg/base.py":    "class Serializer:\n    def serialize(self): pass\n    def end(self): pass\n",
		"pkg/json.py":    "from pkg import base\n\n\nclass Serializer(base.Serializer):\n    def serialize(self):\n        super(Serializer, self).serialize()\n        self.end()\n",
		"pkg/mi.py":      "class A:\n    pass\n\n\nclass B:\n    def m(self): pass\n\n\nclass C(A, B):\n    def m(self):\n        super().m()\n",
		// an external base of the same name: not this Session, so unresolved
		"pkg/wrap.py": "import requests\n\n\nclass Session(requests.Session):\n    pass\n",
	}
	for path, src := range files {
		if err := e.IndexFile(ctx, wt, path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"imports": {"<file> -> Store @pkg/db.py (imports)"},
		"extends": {
			"C -> A @pkg/mi.py (extends)", "C -> B @pkg/mi.py (extends)",
			"Serializer -> Serializer @pkg/base.py (extends)", "Store -> Store @pkg/db.py (extends)",
		},
		"calls": {
			"C.m -> B.m @pkg/mi.py (typed)",
			"Serializer.serialize -> Serializer.end @pkg/base.py (typed)",
			"Serializer.serialize -> Serializer.serialize @pkg/base.py (typed)",
			"Store.save -> Store.load @pkg/db.py (typed)",
			"Store.save -> Store.save @pkg/db.py (typed)",
		},
	}
	check := func(mode string) {
		t.Helper()
		for _, kind := range []string{"imports", "extends", "calls"} {
			if got := pythonTargets(t, e, kind); !slices.Equal(got, want[kind]) {
				t.Errorf("%s resolve, %s:\n got %q\nwant %q", mode, kind, got, want[kind])
			}
		}
	}
	check("full")
	for path, src := range files {
		if err := e.ReindexFileIncremental(ctx, wt, path, []byte(src), 2, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	check("incremental")

	sup, err := e.Implementations(ctx, wt, "Store", "up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sup) != 1 || sup[0].Path != "pkg/db.py" {
		t.Errorf("Implementations(Store, up) = %+v, want only pkg/db.py's Store", sup)
	}
}

// TestPythonBaseModuleChange: an edit that only changes the module a base comes
// from re-resolves the calls inheriting through it in other files too.
func TestPythonBaseModuleChange(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	root := "class Root:\n    def ping(self): pass\n"
	for path, src := range map[string]string{
		"pkg/m1.py":   root,
		"pkg/m2.py":   root,
		"pkg/leaf.py": "from pkg.m1 import Root\n\n\nclass Leaf(Root):\n    pass\n",
		"pkg/use.py":  "from pkg.leaf import Leaf\n\n\ndef run(s: Leaf):\n    s.ping()\n",
	} {
		if err := e.IndexFile(ctx, wt, path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatal(err)
	}
	if got, want := pythonTargets(t, e, "calls"), []string{"run -> Root.ping @pkg/m1.py (typed)"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	leaf := []byte("from pkg.m2 import Root\n\n\nclass Leaf(Root):\n    pass\n")
	if err := e.ReindexFileIncremental(ctx, wt, "pkg/leaf.py", leaf, 2, int64(len(leaf))); err != nil {
		t.Fatal(err)
	}
	if got, want := pythonTargets(t, e, "calls"), []string{"run -> Root.ping @pkg/m2.py (typed)"}; !slices.Equal(got, want) {
		t.Errorf("after moving Leaf's base to pkg.m2: calls %q, want %q", got, want)
	}
}
