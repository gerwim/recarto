package parse

import "testing"

func names(syms []Symbol, kind string) []string {
	var out []string
	for _, s := range syms {
		if s.Kind == kind {
			out = append(out, s.Name)
		}
	}
	return out
}

func has(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestParseGo(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte(`package p

import "fmt"

type Greeter struct{}

func (g Greeter) Hello(name string) string {
	return fmt.Sprintf("hi %s", name)
}

func Run() {
	g := Greeter{}
	g.Hello("world")
}
`)
	res, err := r.ParseFile("go", "p/greet.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if !has(names(res.Symbols, "function"), "Run") {
		t.Errorf("missing function Run; functions=%v", names(res.Symbols, "function"))
	}
	if !has(names(res.Symbols, "method"), "Hello") {
		t.Errorf("missing method Hello; methods=%v", names(res.Symbols, "method"))
	}
	if !has(names(res.Symbols, "type"), "Greeter") {
		t.Errorf("missing type Greeter; types=%v", names(res.Symbols, "type"))
	}
	var calls, imports []string
	for _, e := range res.Edges {
		switch e.Kind {
		case "calls":
			calls = append(calls, e.DstName)
		case "imports":
			imports = append(imports, e.DstName)
		}
	}
	if !has(calls, "Sprintf") || !has(calls, "Hello") {
		t.Errorf("calls = %v, want Sprintf and Hello", calls)
	}
	if len(imports) == 0 {
		t.Errorf("expected an import edge, got none")
	}
	for _, s := range res.Symbols {
		if s.EndByte <= s.StartByte {
			t.Errorf("symbol %q has empty byte range", s.Name)
		}
	}
}
