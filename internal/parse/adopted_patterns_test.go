package parse

import (
	"slices"
	"testing"
)

// These tests pin the query patterns adopted from the grammars' upstream
// tags.scm files (see coverage_test.go): each construct upstream tags as a
// definition/reference must actually extract here, not merely appear in the
// query text.

func edgeNames(res Result, kind string) []string {
	var out []string
	for _, e := range res.Edges {
		if e.Kind == kind {
			out = append(out, e.DstName)
		}
	}
	return out
}

func TestParseGoParenthesizedCalls(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`package p

func Run(f func(), g struct{ M func() }) {
	(f)()
	(work.Do)()
}
`)
	res, err := r.ParseFile("go", "p/p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	calls := edgeNames(res, "calls")
	for _, want := range []string{"f", "Do"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing parenthesized call %q; calls=%v", want, calls)
		}
	}
}

func TestParseTypeScriptAdopted(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`namespace Geo {
  export interface Shape { area(): number; }
}

module Legacy {
  export const v = 1;
}

declare function parseRoute(s: string): string;

abstract class Base {
  abstract render(): void;
}

const handlers = {
  onClick: () => 1,
};

handlers.onClick = function () { return 2; };

class Widget extends Base {
  render(): void {}
}

function* pump() { yield 1; }

export function main() {
  const w = new Widget();
  const g = new Geo.Registry();
  w.render();
}
`)
	res, err := r.ParseFile("typescript", "p/a.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string][]string{
		"namespace": {"Geo", "Legacy"},
		"function":  {"parseRoute", "pump", "main"},
		"method":    {"area", "render", "onClick"},
		"class":     {"Base", "Widget"},
	} {
		for _, n := range want {
			if !slices.Contains(names(res.Symbols, kind), n) {
				t.Errorf("missing %s %q; got %v", kind, n, names(res.Symbols, kind))
			}
		}
	}
	calls := edgeNames(res, "calls")
	for _, want := range []string{"Widget", "Registry"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing constructor call %q (new_expression); calls=%v", want, calls)
		}
	}
}

func TestParseJavaScriptAdopted(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`const Renderer = class NamedRenderer {};

const api = {
  fetchAll: async () => [],
};

api.refresh = function () {};

function* feed() { yield 1; }

const drain = function* () { yield 2; };

function main() {
  const r = new Renderer();
  const s = new lib.Socket();
}
`)
	res, err := r.ParseFile("javascript", "p/a.js", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(res.Symbols, "class"), "NamedRenderer") {
		t.Errorf("missing class expression NamedRenderer; got %v", names(res.Symbols, "class"))
	}
	for _, n := range []string{"feed", "drain", "refresh"} {
		if !slices.Contains(names(res.Symbols, "function"), n) {
			t.Errorf("missing function %q; got %v", n, names(res.Symbols, "function"))
		}
	}
	if !slices.Contains(names(res.Symbols, "method"), "fetchAll") {
		t.Errorf("missing object-literal method fetchAll; got %v", names(res.Symbols, "method"))
	}
	calls := edgeNames(res, "calls")
	for _, want := range []string{"Renderer", "Socket"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing constructor call %q (new_expression); calls=%v", want, calls)
		}
	}
}

func TestParseCSharpAdopted(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`namespace App;

public record Point(int X, int Y);

public record Wide(int X, int Y) : Point(X, Y), System.IDisposable
{
    public void Dispose() { }
}

public class Maker
{
    public Point Make()
    {
        Run<int>();
        this.Run<string>();
        return new Point(1, 2);
    }

    public void Run<T>() { }
}
`)
	res, err := r.ParseFile("csharp", "p/a.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	calls := edgeNames(res, "calls")
	want := map[string]int{"Run": 2, "Point": 1}
	for n, c := range want {
		got := 0
		for _, e := range calls {
			if e == n {
				got++
			}
		}
		if got < c {
			t.Errorf("calls to %q = %d, want ≥%d (generic invocation / object creation); calls=%v", n, got, c, calls)
		}
	}
	extends := edgeNames(res, "extends")
	for _, n := range []string{"Point", "IDisposable"} {
		if !slices.Contains(extends, n) {
			t.Errorf("missing extends edge to %q (primary-constructor / qualified base); extends=%v", n, extends)
		}
	}
}
