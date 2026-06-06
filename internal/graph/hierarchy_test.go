package graph

import (
	"context"
	"testing"
)

// "Find the implementations of interface X" / "what subclasses Y" is one of the
// most common reasons an agent abandons symbol search and greps a large OO
// codebase. The `extends` edges already capture it; Implementations exposes them.
func TestImplementationsFindsSubtypes(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\n" +
		"interface IGreeter { string Greet(); }\n" +
		"class EnglishGreeter : IGreeter { public string Greet() => \"hi\"; }\n" +
		"class FrenchGreeter : IGreeter { public string Greet() => \"salut\"; }\n" +
		"class Base { }\n" +
		"class Derived : Base { }\n")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}

	// Down: implementers/subtypes of the named type.
	impls, err := e.Implementations(ctx, "/wt", "IGreeter", "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := relNames(impls)
	if !got["EnglishGreeter"] || !got["FrenchGreeter"] {
		t.Errorf("Implementations(IGreeter, down) = %v, want EnglishGreeter & FrenchGreeter", keys(got))
	}
	if got["Base"] || got["Derived"] {
		t.Errorf("Implementations(IGreeter, down) leaked unrelated types: %v", keys(got))
	}

	// Works for plain class inheritance too.
	subs, err := e.Implementations(ctx, "/wt", "Base", "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !relNames(subs)["Derived"] {
		t.Errorf("Implementations(Base, down) = %v, want Derived", keys(relNames(subs)))
	}

	// Up: the supertypes a named type extends.
	sup, err := e.Implementations(ctx, "/wt", "Derived", "up", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !relNames(sup)["Base"] {
		t.Errorf("Implementations(Derived, up) = %v, want Base", keys(relNames(sup)))
	}

	// Worktree scoping: a miss is empty, not an error or a cross-repo leak.
	none, err := e.Implementations(ctx, "/other", "IGreeter", "down", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("cross-worktree Implementations leaked %d rows", len(none))
	}
}

func relNames(rs []TypeRelation) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		out[r.Name] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
