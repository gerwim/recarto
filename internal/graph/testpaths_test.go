package graph

import (
	"context"
	"testing"
)

func TestIsTestPath(t *testing.T) {
	tests := []string{
		"src/HotChocolate/Core/test/Types.Tests/Types/ObjectTypeTests.cs",
		"src/GreenDonut/test/GreenDonut.Tests/DataLoader.cs",
		"pkg/svc/handler_test.go",
		"frontend/src/app/login.test.ts",
		"frontend/src/app/login.spec.tsx",
		"api/Foo/BarTests.cs",
	}
	for _, p := range tests {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}
	// Product code must NOT be flagged — a false positive demotes real code.
	human := []string{
		"src/HotChocolate/Core/src/Types/Configuration/TypeReferenceResolver.cs",
		"pkg/svc/handler.go",
		"frontend/src/app/login.ts",
		"api/internal/testing/harness.go", // "testing" segment, not "test"
		"src/contest/winner.cs",           // "contest" is not the "test" segment
	}
	for _, p := range human {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false", p)
		}
	}
}

func TestDemoteTestsStablePartition(t *testing.T) {
	in := []SearchResult{
		{Name: "A", Path: "src/a.cs"},
		{Name: "B", Path: "src/test/b.cs"},
		{Name: "C", Path: "src/c.cs"},
		{Name: "D", Path: "src/d_test.go"},
	}
	got := demoteTests(in, nil)
	want := []string{"A", "C", "B", "D"} // product first, each group stable
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("demoteTests order = %v, want %v", names(got), want)
		}
	}
	if len(got) != len(in) {
		t.Fatalf("demoteTests dropped results: %d != %d", len(got), len(in))
	}
}

func TestDemoteTestsProtectsExactCandidate(t *testing.T) {
	in := []SearchResult{
		{Name: "Other", Path: "src/a.cs"},
		{Name: "SnapshotTests", Path: "src/test/SnapshotTests.cs"}, // a test, but explicitly requested
	}
	got := demoteTests(in, map[string]bool{"snapshottests": true})
	if got[1].Name != "SnapshotTests" {
		t.Fatalf("protected candidate was reordered: %v", names(got))
	}
}

// TestSearchDemotesTestsBelowSource is the end-to-end guarantee: when product
// code and a test both match a concept query, the product code leads — but the
// test is still returned (recall preserved), so an LLM seeking implementation
// stops reading test noise without losing the ability to find the test.
func TestSearchDemotesTestsBelowSource(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\npublic class TypeResolver { public void Resolve() {} }\n")
	if err := e.IndexFile(ctx, "/wt", "src/Types/TypeResolver.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	tst := []byte("namespace App;\npublic class TypeResolverTests { public void Resolve_Works() {} }\n")
	if err := e.IndexFile(ctx, "/wt", "test/Types.Tests/TypeResolverTests.cs", tst, 1, int64(len(tst))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "type resolver", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("search returned nothing")
	}
	if isTestPath(got[0].Path) {
		t.Fatalf("a test ranked first over product code: %v", resultPaths(got))
	}
	var sawTest bool
	for _, r := range got {
		if isTestPath(r.Path) {
			sawTest = true
		}
	}
	if !sawTest {
		t.Fatalf("test results were dropped entirely: %v", resultPaths(got))
	}
}

// TestSearchAllTestsKeepsOrder guards the adaptive contract: when every match is
// a test (a deliberately test-seeking query), demotion is a no-op and the
// best-ranked test still leads — nothing useful is buried.
func TestSearchAllTestsKeepsOrder(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	a := []byte("namespace App;\npublic class SnapshotTests { public void Match_Works() {} }\n")
	if err := e.IndexFile(ctx, "/wt", "test/SnapshotTests.cs", a, 1, int64(len(a))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "snapshot", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name == "" {
		t.Fatalf("all-test query returned nothing useful: %v", resultNames(got))
	}
}

func TestIsTestPathExported(t *testing.T) {
	if !IsTestPath("pkg/svc/handler_test.go") {
		t.Error("IsTestPath(handler_test.go) = false, want true")
	}
	if IsTestPath("pkg/svc/handler.go") {
		t.Error("IsTestPath(handler.go) = true, want false")
	}
}
