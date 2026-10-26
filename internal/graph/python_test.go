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
// class X(Base)` and `class X(m.X)` idioms — and to nothing when that module is
// not in the worktree; super() binds through the class's bases.
func TestPythonModules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	files := map[string]string{
		"pkg/a_decoy.py": "class Store:\n    def save(self): pass\n    def load(self): pass\n\n\nclass Serializer:\n    def serialize(self): pass\n    def end(self): pass\n",
		"pkg/db.py":      "class Store:\n    def save(self): pass\n    def load(self): pass\n    def json(self): pass\n    def list(self): pass\n",
		"pkg/cached.py":  "from pkg.db import Store as DBStore\n\n\nclass Store(DBStore):\n    def save(self):\n        super().save()\n        self.load()\n",
		"pkg/base.py":    "class Serializer:\n    def serialize(self): pass\n    def end(self): pass\n",
		"pkg/json.py":    "from pkg import base\n\n\nclass Serializer(base.Serializer):\n    def serialize(self):\n        super(Serializer, self).serialize()\n        self.end()\n",
		// a base is a class: list is the builtin, not Store.list
		"pkg/mi.py": "class A:\n    pass\n\n\nclass B:\n    def m(self): pass\n\n\nclass C(A, B):\n    def m(self):\n        super().m()\n\n\nclass D(list):\n    pass\n",
		// an external base of the same name: not this Session, so unresolved
		"pkg/wrap.py": "import requests\n\n\nclass Session(requests.Session):\n    pass\n",
		// ... even when another Session subclasses this one, which would make a
		// cycle super() climbs back into close itself
		"pkg/session.py":    "from wsgiref import simple_server\n\n\nclass Handler(simple_server.Handler):\n    def close(self):\n        super().close()\n",
		"tests/test_web.py": "from pkg import session\n\n\nclass Handler(session.Handler):\n    pass\n",
		// imports of an external module, a submodule (whatever else is named mi), a
		// method or the importing file's own symbol bind no worktree symbol
		"pkg/util.py": "from urllib.parse import urlencode as quote\nfrom pkg import mi\nfrom pkg.base import end\nfrom pkg import urlencode\nimport json\n\n\ndef urlencode(q):\n    return quote(q)\n\n\ndef mi(): pass\n",
		// a base a star import brings in names no module: by name, never itself
		"pkg/model.py": "class Model:\n    pass\n\n\ndef mi(): pass\n",
		"pkg/star.py":  "from pkg.model import *\n\n\nclass Model(Model):\n    pass\n",
		// a package star-importing its submodule; Model is not unique (see pkg/)
		"orm/__init__.py": "from orm.base import *\n",
		"orm/base.py":     "class Model:\n    def save(self): pass\n",
		"blog.py":         "from orm import Model\n\n\nclass Post(Model):\n    def publish(self):\n        self.save()\n",
		// an absolute import from under a source root finds its module
		"src/lib/core.py": "class Engine:\n    pass\n",
		"src/main.py":     "from lib.core import Engine\n",
		// a package's own Config, before a submodule's (Alt.py sorts first)
		"cfg/__init__.py": "class Config:\n    pass\n",
		"cfg/Alt.py":      "class Config:\n    pass\n",
		"app.py":          "from cfg import Config\n",
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
		"imports": {
			"<file> -> Config @cfg/__init__.py (imports)", "<file> -> Engine @src/lib/core.py (imports)",
			"<file> -> Model @orm/base.py (imports)", "<file> -> Store @pkg/db.py (imports)",
		},
		"extends": {
			"C -> A @pkg/mi.py (extends)", "C -> B @pkg/mi.py (extends)",
			"Handler -> Handler @pkg/session.py (extends)", "Model -> Model @pkg/model.py (extends)",
			"Post -> Model @orm/base.py (extends)",
			"Serializer -> Serializer @pkg/base.py (extends)", "Store -> Store @pkg/db.py (extends)",
		},
		"calls": {
			"C.m -> B.m @pkg/mi.py (typed)",
			"Post.publish -> Model.save @orm/base.py (typed)",
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

// TestPythonReexports: an import whose module re-exports the name resolves to
// what the module itself imports — through a re-export of a re-export too —
// while a re-exported variable, no symbol, stays unresolved; editing or
// dropping the re-export moves its importers along, incrementally or in a full
// resolve. A base through a re-export resolves to the one module-level class of
// that name (Widget, Gauge), or not at all (Desc): never to a wrong one, nor
// itself.
func TestPythonReexports(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	for path, src := range map[string]string{
		"fields/descriptors.py": "class Desc:\n    def get(self): pass\n",
		"fields/other.py":       "class Desc:\n    def get(self): pass\n\n    class Gauge:\n        pass\n",
		"fields/widgets.py":     "class Widget:\n    def draw(self): pass\n\n\nclass Gauge:\n    pass\n",
		"fields/related.py":     "from fields.descriptors import Desc\nfrom fields.widgets import Widget, Gauge\n",
		"app3.py":               "from fields import related\n\n\nclass Gauge(related.Gauge):\n    pass\n",
		"forms.py":              "from fields.related import Desc\n",
		"app.py":                "from forms import Desc\nfrom fields import related\n\n\nclass MyWidget(related.Widget):\n    def run(self):\n        self.draw()\n\n\nclass MyDesc(related.Desc):\n    pass\n",
		"app2.py":               "from fields import related\n\n\nclass Desc(related.Desc):\n    pass\n",
		"urls/__init__.py":      "from urls.conf import path\n",
		"urls/conf.py":          "path = make_path()\n",
		"views.py":              "from urls import path\n\n\nclass Storage:\n    def path(self): pass\n",
	} {
		if err := e.IndexFile(ctx, wt, path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatal(err)
	}
	check := func(when, module string, importers int) {
		t.Helper()
		for kind, want := range map[string][]string{
			// related.py's own import, forms.py's (one re-export), app.py's (two)
			"imports": append(slices.Repeat([]string{"<file> -> Desc @fields/" + module + ".py (imports)"}, importers),
				"<file> -> Gauge @fields/widgets.py (imports)", "<file> -> Widget @fields/widgets.py (imports)"),
			"extends": {"Gauge -> Gauge @fields/widgets.py (extends)", "MyWidget -> Widget @fields/widgets.py (extends)"},
			"calls":   {"MyWidget.run -> Widget.draw @fields/widgets.py (typed)"},
		} {
			if got := pythonTargets(t, e, kind); !slices.Equal(got, want) {
				t.Errorf("%s, %s:\n got %q\nwant %q", when, kind, got, want)
			}
		}
	}
	check("full resolve", "descriptors", 3)

	edit := func(src string, incremental bool) {
		t.Helper()
		b := []byte(src + "from fields.widgets import Widget, Gauge\n")
		var err error
		if incremental {
			err = e.ReindexFileIncremental(ctx, wt, "fields/related.py", b, 2, int64(len(b)))
		} else if err = e.IndexFile(ctx, wt, "fields/related.py", b, 3, int64(len(b))); err == nil {
			err = e.Resolve(ctx, wt, noopProgress{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	edit("from fields.other import Desc\n", true)
	check("after re-exporting fields.other's Desc", "other", 3)
	edit("from fields.descriptors import Desc\n", false)
	check("full resolve after re-exporting fields.descriptors' Desc again", "descriptors", 3)
	edit("", true)
	check("after dropping the re-export", "", 0)
	edit("from fields.descriptors import Desc\n", true)
	check("after adding it back", "descriptors", 3)
}

// TestPythonModuleCreated: which modules exist decides a base: one naming a
// module that is not in the worktree stays unresolved, and once the module is
// (re-exporting the base with a star import, so no edge or name of its own
// tells), the base is the one class of its name — and unresolved again when
// the module goes. Reconcile picks each change up, inherited calls too.
func TestPythonModuleCreated(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	src := &mutMemSource{files: map[string]string{
		"lib/impl.py": "class Base:\n    def ping(self): pass\n",
		"app.py":      "from lib import api\n\n\nclass App(api.Base):\n    def run(self):\n        self.ping()\n",
	}}
	check := func(when string, want ...string) {
		t.Helper()
		if err := e.Reconcile(ctx, wt, src); err != nil {
			t.Fatal(err)
		}
		got := append(pythonTargets(t, e, "extends"), pythonTargets(t, e, "calls")...)
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", when, got, want)
		}
	}
	check("without lib/api.py", "App.run -> Base.ping @lib/impl.py (name)")
	src.files["lib/api.py"] = "from lib.impl import *\n"
	check("with lib/api.py", "App -> Base @lib/impl.py (extends)", "App.run -> Base.ping @lib/impl.py (typed)")
	delete(src.files, "lib/api.py")
	check("without lib/api.py again", "App.run -> Base.ping @lib/impl.py (name)")
}
