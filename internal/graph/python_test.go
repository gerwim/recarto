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
		"Shape.describe -> Shape.label (typed)",      // self.label()
		"Shape.describe -> Shape.area (typed)",       // self.area() ...
		"Shape.describe -> Circle.area (iface)",      // ... and the override it may dispatch to
		"Circle.__init__ -> Shape.__init__ (static)", // super().__init__()
		"Circle.area -> area (name)",                 // bare area() is the module function
		"Circle.unit -> Circle.make (typed)",         // cls.make()
		"Circle.make -> Circle (ctor)",               // Circle(r)
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
