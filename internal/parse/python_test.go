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
		"imports os  1", "imports os.path  1", "imports Thing  2", "imports other  2",
		"imports Shape  3", "imports Square  4",
		"annotates dataclass  7", "annotates register  8", "calls register registry 8",
		"extends Shape  9", "extends Named  9", "extends Generic  9", // metaclass= is no base
		"calls Store  11",
		"calls super  14", "calls __init__ Shape 14", // super() is the first base
		"calls Logger  17",
		"annotates staticmethod  19", "calls Circle  21",
		"annotates property  23", "calls load self.repo 26", "calls helper  27", "calls inner  27",
		"calls super  34", "calls m super() 34", // Config has no base to name
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

// TestParsePythonSuper: a super() receiver is the first positional base of the
// innermost enclosing class, never an outer class's.
func TestParsePythonSuper(t *testing.T) {
	parsePy := pythonParser(t)
	for _, c := range []struct{ src, recv string }{
		{"class A(B):\n    def f(self):\n        super().f()", "B"},
		{"class A(B):\n    def f(self):\n        super(A, self).f()", "B"},
		{"class A(m.B[T], C):\n    def f(self):\n        super().f()", "B"},
		{"class A:\n    def f(self):\n        super().f()", "super()"},
		{"class A(metaclass=M):\n    def f(self):\n        super().f()", "super()"},
		{"class A(B):\n    class C:\n        def f(self):\n            super().f()", "super()"},
		// f is called on what super() returns, not on super itself
		{"class A(B):\n    def g(self):\n        super().x.f()", "super().x"},
		{"class A(B):\n    def g(self):\n        super().g().f()", "super().g()"},
		// C is A's last statement, so both classes end on the same byte
		{"class A(B):\n    def f(self):\n        super().f()\n    class C(D):\n        pass", "B"},
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
// binds Type.f like Type.f() does.
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
}
