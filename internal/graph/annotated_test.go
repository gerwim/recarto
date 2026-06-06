package graph

import (
	"context"
	"testing"
)

func annNames(rs []AnnotatedSymbol) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		out[r.Name] = true
	}
	return out
}

// "Find every class decorated with [ExtendObjectType]" is a top grep-fallback in
// C#/TS: symbol search returns the attribute's definition, not its use sites.
// Annotated exposes the `annotates` edges, and crucially attributes each edge to
// the *decorated* symbol (innermost enclosing declaration), not its namespace.
func TestAnnotatedFindsDecoratedSymbols(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\n" +
		"[ExtendObjectType(\"Query\")]\n" +
		"public class QueryExtensions {\n" +
		"  [GraphQLName(\"foo\")]\n" +
		"  public string Foo() => \"x\";\n" +
		"}\n" +
		"[ExtendObjectType<Street>]\n" +
		"public class StreetExtensions { }\n" +
		"public class Plain { }\n")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Class-level attribute → the two decorated classes, not the plain one.
	classes, err := e.Annotated(ctx, "/wt", "ExtendObjectType", 50)
	if err != nil {
		t.Fatal(err)
	}
	got := annNames(classes)
	if !got["QueryExtensions"] || !got["StreetExtensions"] {
		t.Errorf("Annotated(ExtendObjectType) = %v, want QueryExtensions & StreetExtensions", keys(got))
	}
	if got["Plain"] {
		t.Errorf("Annotated(ExtendObjectType) leaked undecorated class Plain")
	}

	// Member-level attribute → attributed to the method, not the enclosing class.
	methods, err := e.Annotated(ctx, "/wt", "GraphQLName", 50)
	if err != nil {
		t.Fatal(err)
	}
	gm := annNames(methods)
	if !gm["Foo"] {
		t.Errorf("Annotated(GraphQLName) = %v, want method Foo", keys(gm))
	}
	if gm["QueryExtensions"] {
		t.Errorf("Annotated(GraphQLName) wrongly attributed to enclosing class, not the method")
	}

	// Worktree scoping: a miss is empty, not a cross-repo leak.
	none, err := e.Annotated(ctx, "/other", "ExtendObjectType", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("cross-worktree Annotated leaked %d rows", len(none))
	}
}

// C# allows [Obsolete] as shorthand for the ObsoleteAttribute class; a query for
// either spelling must find uses written the other way.
func TestAnnotatedSuffixTolerant(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\n" +
		"[ObsoleteAttribute]\n" +
		"public class Old { }\n" +
		"[Serializable]\n" +
		"public class Data { }\n")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	// Written long form, queried short.
	short, err := e.Annotated(ctx, "/wt", "Obsolete", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !annNames(short)["Old"] {
		t.Errorf("Annotated(Obsolete) = %v, want Old (written [ObsoleteAttribute])", keys(annNames(short)))
	}
	// Written short form, queried long.
	long, err := e.Annotated(ctx, "/wt", "SerializableAttribute", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !annNames(long)["Data"] {
		t.Errorf("Annotated(SerializableAttribute) = %v, want Data (written [Serializable])", keys(annNames(long)))
	}
}
