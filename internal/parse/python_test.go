package parse

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const pythonSrc = `import os, os.path as osp
from ..pkg.mod import Thing as T, other
from .shapes import (Shape,
    Square)


@dataclass
@registry.register("circle")
class Circle(Shape, mixins.Named, Generic[T], metaclass=Meta):
    radius: float = 0.0
    store = Store()

    def __init__(self, repo: Repo, cache: Optional["Cache"] = None):
        super().__init__(repo)
        self.repo = repo
        self.cache = cache
        self.log = Logger()

    @staticmethod
    def make(x: int) -> "Circle":
        return Circle()

    @property
    async def area(self):
        def inner():
            return self.repo.load()
        return helper(self.radius) + inner()

    if TYPE_CHECKING:
        def typed(cls) -> None: ...

    class Config:
        def m(self):
            super().m()


type Alias = list[int]


def helper(c: Circle | None, s: shapes.Square):
    c.area()
    s.side()
    t: Shape = Circle.make(1)
    t.area()
`

// pythonParser returns a parse func over one registry, closed when the test ends.
func pythonParser(t *testing.T) func(src string) Result {
	t.Helper()
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return func(src string) Result {
		t.Helper()
		res, err := r.ParseFile("python", "pkg/mod.py", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
}

// TestParsePython pins the Python extraction end to end on one file: a def is a
// method exactly when its nearest enclosing def/class is a class, decorated
// spans start at the first decorator, every edge kind, and the bindings —
// including self.x attributes scoped to the class they belong to.
func TestParsePython(t *testing.T) {
	res := pythonParser(t)(pythonSrc)

	var syms []string
	for _, s := range res.Symbols {
		syms = append(syms, fmt.Sprintf("%s %s %d-%d@%d", s.Kind, s.Name, s.StartLine, s.EndLine, s.NameLine))
	}
	wantSyms := []string{
		"class Circle 7-34@9", // span widened over both decorators
		"method __init__ 13-17@13",
		"method make 19-21@20",
		"method area 23-27@24",
		"function inner 25-26@25", // nested in a method: a function
		"method typed 30-30@30",   // under an if in the class body: still a method
		"class Config 32-34@32",
		"method m 33-34@33",
		"type Alias 37-37@37",
		"function helper 40-44@40",
	}
	if !slices.Equal(syms, wantSyms) {
		t.Errorf("symbols:\n got %s\nwant %s", strings.Join(syms, ", "), strings.Join(wantSyms, ", "))
	}

	var edges []string
	for _, e := range res.Edges {
		edges = append(edges, fmt.Sprintf("%s %s %s %d", e.Kind, e.DstName, e.Recv, e.Line))
	}
	wantEdges := []string{
		// a from-import carries its module, relative ones resolved against pkg/
		"imports os  1", "imports os.path  1", "imports Thing pkg.mod 2", "imports other pkg.mod 2",
		"imports Shape pkg.shapes 3", "imports Square pkg.shapes 4",
		"annotates dataclass  7", "annotates register  8", "calls register registry 8",
		// an imported base carries its module too; metaclass= is no base
		"extends Shape pkg.shapes 9", "extends Named  9", "extends Generic  9",
		"calls Store  11",
		"calls super  14", "calls __init__ super() 14",
		"calls Logger  17",
		"annotates staticmethod  19", "calls Circle  21",
		"annotates property  23", "calls load self.repo 26", "calls helper  27", "calls inner  27",
		"calls super  34", "calls m super() 34",
		"calls area c 41", "calls side s 42", "calls make Circle 43", "calls area t 44",
	}
	slices.Sort(edges)
	slices.Sort(wantEdges)
	if !slices.Equal(edges, wantEdges) {
		t.Errorf("edges:\n got %s\nwant %s", strings.Join(edges, ", "), strings.Join(wantEdges, ", "))
	}

	circle := res.Symbols[0]
	var binds []string
	for _, b := range res.Bindings {
		bind := b.Name + ":" + b.TypeName
		if strings.HasPrefix(b.Name, "self.") && b.StartByte != circle.NameByte {
			bind += " (not anchored at Circle)"
		}
		binds = append(binds, bind)
	}
	wantBinds := []string{
		"self:Circle", "repo:Repo", "cache:Cache", "x:int", "self:Circle", "self:Config",
		"cls:Circle", // typed(cls), declared under the class body's if
		"c:Circle", "s:Square", "t:Shape",
		// class-level fields, self.x = T(), and self.x = param via its annotation
		"self.radius:float", "self.store:Store", "self.repo:Repo", "self.cache:Cache", "self.log:Logger",
	}
	slices.Sort(binds)
	slices.Sort(wantBinds)
	if !slices.Equal(binds, wantBinds) {
		t.Errorf("bindings:\n got %s\nwant %s", strings.Join(binds, ", "), strings.Join(wantBinds, ", "))
	}
}

// TestParsePythonBindingShapes pins x -> Area for every annotation shape, as a
// parameter, a defaulted parameter, a local and a self attribute: one query
// pattern covers all four, so they cannot drift apart.
func TestParsePythonBindingShapes(t *testing.T) {
	parsePy := pythonParser(t)
	for _, ty := range []string{
		"Area", "geo.Area", `"Area"`, "Area[int]", "geo.Area[int]", "Optional[Area]",
		"Optional[geo.Area]", `Optional["Area"]`, "typing.Optional[Area]", "typing.Optional[geo.Area]",
		`typing.Optional["Area"]`, "Area | None", "geo.Area | None",
	} {
		for _, c := range []struct{ src, want string }{
			{"def f(x: " + ty + "): pass", "x:Area"},
			{"def f(x: " + ty + " = None): pass", "x:Area"},
			{"def f():\n    x: " + ty + " = g()", "x:Area"},
			{"class C:\n    def f(self):\n        self.x: " + ty + " = g()", "self.x:Area"},
			{"class C:\n    x: " + ty, "self.x:Area"},
			{"class C:\n    if TYPE_CHECKING:\n        x: " + ty, "self.x:Area"},
		} {
			var got []string
			for _, b := range parsePy(c.src).Bindings {
				if b.Name != "self" {
					got = append(got, b.Name+":"+b.TypeName)
				}
			}
			if !slices.Equal(got, []string{c.want}) {
				t.Errorf("%q: bindings %v, want [%s]", c.src, got, c.want)
			}
		}
	}
	// Neither a lowercase callee (a function, by PEP 8) nor a bare Optional, nor a
	// list of T, says what x is.
	for _, src := range []string{"x = make()", "x = Area.create()", "def f(x: Optional): pass", "def f(x: Optional[A, B]): pass", "def f(*x: Area): pass"} {
		if b := parsePy(src).Bindings; len(b) != 0 {
			t.Errorf("%q: bindings %v, want none", src, b)
		}
	}
}

// TestParsePythonSuper: a call on super itself has the receiver super(), which
// the resolver binds through the enclosing class's bases; a call on what super()
// returns does not.
func TestParsePythonSuper(t *testing.T) {
	parsePy := pythonParser(t)
	for _, c := range []struct{ src, recv string }{
		{"class A(B):\n    def f(self):\n        super().f()", "super()"},
		{"class A(B):\n    def f(self):\n        super(A, self).f()", "super()"},
		{"class A(B):\n    def g(self):\n        super().x.f()", "super().x"},
		{"class A(B):\n    def g(self):\n        super().g().f()", "super().g()"},
	} {
		var got []string
		for _, e := range parsePy(c.src).Edges {
			if e.Kind == "calls" && e.DstName == "f" {
				got = append(got, e.Recv)
			}
		}
		if !slices.Equal(got, []string{c.recv}) {
			t.Errorf("%q: f receivers %v, want [%s]", c.src, got, c.recv)
		}
	}
}

// TestParsePythonReceivers: a call's receiver is its object expression, except
// that a CapWords last segment (a class, by PEP 8) stands alone so m.Type.f()
// binds Type.f like Type.f() does, and a call on a literal (a builtin's method)
// gets no edge at all.
func TestParsePythonReceivers(t *testing.T) {
	parsePy := pythonParser(t)
	for _, c := range []struct{ call, recv string }{
		{"f()", ""},
		{"x.f()", "x"},
		{"self.repo.f()", "self.repo"},
		{"os.path.f()", "os.path"},
		{"Point.f()", "Point"},
		{"geo.Point.f()", "Point"},
		{"self.Config.f()", "Config"},
		{"items[0].f()", "items[0]"},
		{"g().f()", "g()"},
		{"(1).f()", "(1)"},
	} {
		var got []string
		for _, e := range parsePy("def caller():\n    " + c.call).Edges {
			if e.Kind == "calls" && e.DstName == "f" {
				got = append(got, e.Recv)
			}
		}
		if !slices.Equal(got, []string{c.recv}) {
			t.Errorf("%s: f receivers %q, want [%q]", c.call, got, c.recv)
		}
	}
	for _, call := range []string{`", ".f(xs)`, `f"{a}".f()`, `b"".f()`, `"a" "b".f()`, "[1].f()", "{}.f()", "{1}.f()", "(1, 2).f()", "[x for x in y].f()", "1.0.f()"} {
		for _, e := range parsePy("def caller():\n    " + call).Edges {
			if e.Kind == "calls" && e.DstName == "f" {
				t.Errorf("%s: call edge to f (receiver %q); a literal's method is a builtin's", call, e.Recv)
			}
		}
	}
}

// TestParsePythonSelfAttrs: self.x = name takes the type that name has in the
// def doing the assignment — not a same-named parameter of another def.
func TestParsePythonSelfAttrs(t *testing.T) {
	parsePy := pythonParser(t)
	for _, c := range []struct{ src, want string }{
		{"class C:\n    def m(self, p: A):\n        def inner(p: B):\n            pass\n        self.x = p", "self.x:A"},
		{"class C:\n    def m(self, p: A):\n        def inner(p: B):\n            self.x = p", "self.x:B"},
		{"class C:\n    def m(self):\n        x = Repo()\n        self.x = x", "self.x:Repo"},
		{"class C:\n    def m(self, p):\n        self.x = p", ""},
		{"def f(self, p: A):\n    self.x = p", ""}, // no class: no attribute to type
	} {
		var got []string
		for _, b := range parsePy(c.src).Bindings {
			if strings.HasPrefix(b.Name, "self.") {
				got = append(got, b.Name+":"+b.TypeName)
			}
		}
		if want := strings.Fields(c.want); !slices.Equal(got, want) {
			t.Errorf("%q: self bindings %v, want %v", c.src, got, want)
		}
	}
}

// TestParsePythonMethodReceivers: a method's self/cls is bound to its class by
// the rule that makes the def a method, whatever block of the class body
// declares it; a function's first parameter is bound to nothing.
func TestParsePythonMethodReceivers(t *testing.T) {
	parsePy := pythonParser(t)
	for _, c := range []struct{ src, want string }{
		{"class C:\n    def m(self): pass", "self:C"},
		{"class C:\n    @classmethod\n    def m(cls): pass", "cls:C"},
		{"class C:\n    if X:\n        def m(self): pass", "self:C"},
		{"class C:\n    try:\n        @property\n        def m(self): pass\n    except E:\n        pass", "self:C"},
		{"class C:\n    class D:\n        def m(self): pass", "self:D"},
		{"class C:\n    def m(other): pass", ""},
		{"def f(self): pass", ""},
		{"class C:\n    def m(this):\n        def inner(self): pass", ""}, // inner is a function
	} {
		var got []string
		for _, b := range parsePy(c.src).Bindings {
			got = append(got, b.Name+":"+b.TypeName)
		}
		if want := strings.Fields(c.want); !slices.Equal(got, want) {
			t.Errorf("%q: bindings %v, want %v", c.src, got, want)
		}
	}
}

// TestParsePythonModules: a from-import edge carries the module it reads,
// relative ones resolved against the importing file's package, and a base names
// the class it denotes (an imported alias's real name) with its module.
func TestParsePythonModules(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range []struct{ path, src, want string }{
		{"a/b/c.py", "from x.y import Z", "imports Z x.y"},
		{"a/b/c.py", "from . import Z", "imports Z a.b"},
		{"a/b/c.py", "from .m import Z as W", "imports Z a.b.m"},
		{"a/b/c.py", "from ..m import Z", "imports Z a.m"},
		{"c.py", "from .m import Z", "imports Z m"},
		{"a/b/c.py", "import x.y as z", "imports x.y "},
		{"a/b/c.py", "from db import Store as DBStore\nclass Store(DBStore): pass", "imports Store db; extends Store db"},
		{"a/b/c.py", "from x import Base\nclass C(Base[int]): pass", "imports Base x; extends Base x"},
		{"a/b/c.py", "from django.core.serializers import base\nclass Serializer(base.Serializer): pass", "imports base django.core.serializers; extends Serializer django.core.serializers.base"},
		{"a/b/c.py", "import a.b\nclass C(a.b.Base): pass", "imports a.b ; extends Base a.b"},
		{"a/b/c.py", "import x.y as m\nclass C(m.Base): pass", "imports x.y ; extends Base x.y"},
		{"a/b/c.py", "from . import sib\nclass C(sib.Base): pass", "imports sib a.b; extends Base a.b.sib"},
		{"a/b/c.py", "class Base: pass\nclass C(Base, other.Mixin): pass", "extends Base ; extends Mixin "},
	} {
		res, err := r.ParseFile("python", c.path, []byte(c.src))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range res.Edges {
			if e.Kind == "imports" || e.Kind == "extends" {
				got = append(got, e.Kind+" "+e.DstName+" "+e.Recv)
			}
		}
		if want := strings.Split(c.want, "; "); !slices.Equal(got, want) {
			t.Errorf("%s %q: %q, want %q", c.path, c.src, got, want)
		}
	}
}
