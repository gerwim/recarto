package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/app"
)

func TestGraphToolsRejectCodeLessFolderWithoutRepos(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hello-marker\nhello-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := codelessApp(t, folder)
	calls := []struct {
		name string
		err  error
	}{
		{"search_graph", func() error { _, err := searchGraph(context.Background(), a, SearchGraphInput{Query: "x"}); return err }()},
		{"trace_call_path", func() error {
			_, err := traceCallPath(context.Background(), a, TraceCallPathInput{Symbol: "x"})
			return err
		}()},
		{"get_architecture", func() error { _, err := getArchitecture(context.Background(), a, GetArchitectureInput{}); return err }()},
		{"query_graph", func() error {
			_, err := queryGraph(context.Background(), a, QueryGraphInput{Kind: "function"})
			return err
		}()},
		{"get_code_snippet", func() error {
			_, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbol: "x"})
			return err
		}()},
		{"get_file_outline", func() error {
			_, err := getFileOutline(context.Background(), a, GetFileOutlineInput{File: "x.go"})
			return err
		}()},
		{"find_implementations", func() error {
			_, err := findImplementations(context.Background(), a, FindImplementationsInput{Symbol: "x"})
			return err
		}()},
		{"find_annotated", func() error {
			_, err := findAnnotated(context.Background(), a, FindAnnotatedInput{Attribute: "x"})
			return err
		}()},
		{"find_references", func() error {
			_, err := findReferences(context.Background(), a, FindReferencesInput{Symbol: "x"})
			return err
		}()},
		{"get_graph_delta", func() error { _, err := getGraphDelta(context.Background(), a, GetGraphDeltaInput{}); return err }()},
	}
	for _, call := range calls {
		if !errors.Is(call.err, errNoCode) {
			t.Errorf("%s error = %v, want %v", call.name, call.err, errNoCode)
		}
	}
	out, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "hello-marker"})
	if err != nil || out.Total != 2 {
		t.Fatalf("folder content = %+v, %v", out, err)
	}
}

func TestShareCap(t *testing.T) {
	tests := []struct {
		avail []int
		limit int
		want  []int
	}{
		{[]int{8, 2}, 10, []int{8, 2}},
		{[]int{2, 8}, 10, []int{2, 8}},
		{[]int{6, 7}, 10, []int{5, 5}},
		{[]int{3, 3}, 5, []int{3, 2}},
		{[]int{0, 4}, 3, []int{0, 3}},
		{[]int{9, 1}, 4, []int{3, 1}},
		{[]int{10, 10, 10, 1}, 10, []int{3, 3, 3, 1}},
		{[]int{10, 10, 10, 1}, 9, []int{3, 3, 2, 1}},
		{[]int{5, 5, 5}, 4, []int{2, 1, 1}},
	}
	for _, tc := range tests {
		if got := shareCap(tc.avail, tc.limit); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shareCap(%v, %d) = %v, want %v", tc.avail, tc.limit, got, tc.want)
		}
	}
}

func servedGraphFixture(t *testing.T, several bool) (folder, recarto, second string, a *app.App) {
	t.Helper()
	folder, recarto, second = routeRepos(t)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "notes.md"), []byte("hello-marker\nhello-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "README.md"), []byte("hello-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	served := []string{recarto}
	if several {
		served = append(served, second)
	}
	a = codelessApp(t, folder, served...)
	for _, rc := range a.Repos {
		indexRepo(t, rc)
	}
	return folder, recarto, second, a
}

func TestServedGraphRouting(t *testing.T) {
	_, _, _, a := servedGraphFixture(t, true)
	query := SearchGraphInput{Query: "TryLockFile", CandidateSymbols: []string{"TryLockFile"}, Limit: 10}
	first, err := searchGraph(context.Background(), a, SearchGraphInput{Repo: "recarto", Query: query.Query, CandidateSymbols: query.CandidateSymbols, Limit: query.Limit})
	if err != nil || first.Count == 0 {
		t.Fatalf("recarto inventory = %+v, %v", first, err)
	}
	secondOut, err := searchGraph(context.Background(), a, SearchGraphInput{Repo: "second", Query: query.Query, CandidateSymbols: query.CandidateSymbols, Limit: query.Limit})
	if err != nil || secondOut.Count == 0 {
		t.Fatalf("second inventory = %+v, %v", secondOut, err)
	}
	joined, err := searchGraph(context.Background(), a, query)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Count != first.Count+secondOut.Count {
		t.Fatalf("joined search count = %d, inventories %d + %d", joined.Count, first.Count, secondOut.Count)
	}
	for i, result := range joined.Results {
		want := "recarto"
		if i >= first.Count {
			want = "second"
		}
		if result.Repo != want {
			t.Fatalf("result %d repo = %q, want %q", i, result.Repo, want)
		}
	}

	pathQuery, err := queryGraph(context.Background(), a, QueryGraphInput{Path: "internal/store/filelock.go", Kind: "function"})
	if err != nil || pathQuery.Count != 2 {
		t.Fatalf("two-file query = %+v, %v", pathQuery, err)
	}
	if pathQuery.Nodes[0].Repo != "recarto" || pathQuery.Nodes[1].Repo != "second" {
		t.Fatalf("two-file query labels/order = %+v", pathQuery.Nodes)
	}
	limitedPathQuery, err := queryGraph(context.Background(), a, QueryGraphInput{Path: "internal/store/filelock.go", NameContains: "secondOnlyLock", Limit: 1})
	if err != nil || limitedPathQuery.Count != 1 || !strings.Contains(limitedPathQuery.Hint, "limit is shared") {
		t.Fatalf("limited two-file query = %+v, %v; want shared-cap hint", limitedPathQuery, err)
	}

	outline, err := getFileOutline(context.Background(), a, GetFileOutlineInput{File: "store/filelock.go"})
	if err != nil || outline.Repo != "second" || outline.Count != 1 {
		t.Fatalf("owned outline = %+v, %v", outline, err)
	}
	snippet, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbol: "TryLockFile", File: "internal/store/filelock.go"})
	if err != nil || !snippet.Found || snippet.Repo != "recarto" {
		t.Fatalf("file-settled snippet = %+v, %v", snippet, err)
	}
	batch, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: []string{"TryLockFile", "secondOnlyLock"}, File: "internal/store/filelock.go"})
	if err != nil || len(batch.Snippets) != 2 || batch.Snippets[0].Repo != "recarto" || batch.Snippets[1].Repo != "second" {
		t.Fatalf("file-settled batch = %+v, %v", batch, err)
	}
}

func TestServedJoinedContent(t *testing.T) {
	folder, _, _, a := servedGraphFixture(t, true)
	content, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "hello-marker", Limit: 10})
	if err != nil || content.Total != 3 || len(content.Matches) != 3 {
		t.Fatalf("joined content = %+v, %v", content, err)
	}
	if content.Matches[0].Repo != folder || content.Matches[1].Repo != folder || content.Matches[2].Repo != "second" {
		t.Fatalf("joined content order/labels = %+v", content.Matches)
	}
	page, err := searchContent(context.Background(), a, SearchContentInput{Pattern: "hello-marker", Limit: 2})
	if err != nil || len(page.Matches) != 2 || !page.Truncated {
		t.Fatalf("first content page = %+v, %v", page, err)
	}
	page, err = searchContent(context.Background(), a, SearchContentInput{Pattern: "hello-marker", Offset: 2, Limit: 2})
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Repo != "second" || page.Truncated {
		t.Fatalf("second content page = %+v, %v", page, err)
	}
}

func TestOneServedRepoGraphMatchesExplicit(t *testing.T) {
	_, recarto, _, one := servedGraphFixture(t, false)
	withoutRepo, err := searchGraph(context.Background(), one, SearchGraphInput{Query: "TryLockFile", CandidateSymbols: []string{"TryLockFile"}})
	if err != nil {
		t.Fatal(err)
	}
	withRepo, err := searchGraph(context.Background(), one, SearchGraphInput{Repo: recarto, Query: "TryLockFile", CandidateSymbols: []string{"TryLockFile"}})
	if err != nil || !reflect.DeepEqual(withoutRepo, withRepo) {
		t.Fatalf("one-repo implicit result = %+v, explicit = %+v, err %v", withoutRepo, withRepo, err)
	}
}
