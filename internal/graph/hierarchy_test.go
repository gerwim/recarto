package graph

import (
	"context"
	"slices"
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
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
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

func TestImplementationsFollowsTypeScriptImplements(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte(`export interface Source { list(): string[] }
export class IndexQueue implements Source { list(): string[] { return []; } }
export class Base {}
export interface I1 {}
export interface I2 {}
export class C extends Base implements I1, I2 {}
`)
	if err := e.IndexFile(ctx, "/wt", "a.ts", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt", noopProgress{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		symbol, direction string
		want              []string
	}{
		{"Source", "down", []string{"IndexQueue implements"}},
		{"IndexQueue", "up", []string{"Source implements"}},
		{"C", "up", []string{"Base extends", "I1 implements", "I2 implements"}},
		{"I1", "down", []string{"C implements"}},
		{"Base", "down", []string{"C implements"}},
	} {
		got, err := e.Implementations(ctx, "/wt", tc.symbol, tc.direction, 10)
		if err != nil {
			t.Fatalf("Implementations(%s, %s): %v", tc.symbol, tc.direction, err)
		}
		gotNames := make([]string, len(got))
		for i, r := range got {
			gotNames[i] = r.Name + " " + r.Relation
		}
		if tc.symbol == "IndexQueue" && tc.direction == "up" && len(got) != 1 {
			t.Errorf("Implementations(IndexQueue, up) returned %d results, want 1", len(got))
			continue
		}
		if !slices.Equal(gotNames, tc.want) {
			t.Errorf("Implementations(%s, %s) = %v, want %v", tc.symbol, tc.direction, gotNames, tc.want)
		}
		if tc.symbol == "IndexQueue" && tc.direction == "up" {
			if got[0].Path != "a.ts" || got[0].Line != 1 {
				t.Errorf("Implementations(IndexQueue, up) target = %s:%d, want a.ts:1", got[0].Path, got[0].Line)
			}
		}
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
