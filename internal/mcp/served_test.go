package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
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
			_, err := queryGraph(context.Background(), a, QueryGraphInput{})
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

func TestTakeInTurn(t *testing.T) {
	parts := [][]GraphSymbol{
		{{QualifiedName: "a1"}, {QualifiedName: "a2"}, {QualifiedName: "a3"}},
		nil,
		{{QualifiedName: "c1"}, {QualifiedName: "c2"}},
	}
	got := searchGraphQualifiedNames(takeInTurn(parts))
	want := []string{"a1", "c1", "a2", "c2", "a3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("takeInTurn names = %v, want %v", got, want)
	}
	if got := takeInTurn(nil); len(got) != 0 {
		t.Fatalf("takeInTurn(nil) = %v, want no entries", got)
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

func searchOrderRepos(t *testing.T) (folder, alpha, beta string) {
	t.Helper()
	base := t.TempDir()
	folder = filepath.Join(base, "_scratch")
	alpha = filepath.Join(base, "alpha")
	beta = filepath.Join(base, "beta")
	gitRepoWith(t, alpha, map[string]string{
		"run/helper.go":      "package run\nfunc Helper() {}\nfunc HelperPool() {}\n",
		"run/helper_test.go": "package run\nfunc TestHelper() {}\n",
	})
	gitRepoWith(t, beta, map[string]string{
		"jobs/pool.go": "package jobs\ntype Pool struct{}\nfunc (Pool) Drain() {}\nfunc TestHelper() {}\nfunc helperQueue() {}\n",
	})
	return folder, alpha, beta
}

func searchGraphQualifiedNames(results []GraphSymbol) []string {
	names := make([]string, len(results))
	for i, result := range results {
		names[i] = result.QualifiedName
	}
	return names
}

func searchGraphLabeledNames(results []GraphSymbol) []string {
	names := make([]string, len(results))
	for i, result := range results {
		names[i] = result.Repo + ":" + result.QualifiedName
	}
	return names
}

func assertJoinedSearchMatchesRepos(t *testing.T, joined SearchGraphOutput, repos map[string]SearchGraphOutput) {
	t.Helper()
	expected := make(map[string]GraphSymbol)
	expectedCount := 0
	for repo, out := range repos {
		expectedCount += out.Count
		for _, result := range out.Results {
			result.Repo = repo
			key := result.Repo + "\x00" + result.QualifiedName + "\x00" + result.Path
			if _, exists := expected[key]; exists {
				t.Fatalf("repo result key %q is not unique", key)
			}
			expected[key] = result
		}
	}
	if joined.Count != expectedCount || len(joined.Results) != expectedCount {
		t.Fatalf("joined count/results = %d/%d, repo counts sum to %d", joined.Count, len(joined.Results), expectedCount)
	}
	seen := make(map[string]int, len(expected))
	for _, result := range joined.Results {
		key := result.Repo + "\x00" + result.QualifiedName + "\x00" + result.Path
		want, ok := expected[key]
		if !ok {
			t.Fatalf("joined result has no matching repo result: %+v", result)
		}
		if !reflect.DeepEqual(result, want) {
			t.Errorf("joined result = %+v, repo result = %+v", result, want)
		}
		seen[key]++
	}
	for key := range expected {
		if seen[key] != 1 {
			t.Errorf("repo result %q occurs %d times in joined results, want once", key, seen[key])
		}
	}
}

func TestServedSearchGraphListsExactMatchesFirst(t *testing.T) {
	folder, alpha, beta := searchOrderRepos(t)
	a := codelessApp(t, folder, alpha, beta)
	for _, rc := range a.Repos {
		indexRepo(t, rc)
	}

	tests := []struct {
		name        string
		input       SearchGraphInput
		alphaNames  []string
		betaNames   []string
		joinedNames []string
	}{
		{
			name:       "query exact names",
			input:      SearchGraphInput{Query: "TestHelper", Limit: 20},
			alphaNames: []string{"Helper", "HelperPool", "TestHelper"},
			betaNames:  []string{"TestHelper", "helperQueue"},
			joinedNames: []string{
				"alpha:TestHelper", "beta:TestHelper", "alpha:Helper", "beta:helperQueue", "alpha:HelperPool",
			},
		},
		{
			name:       "qualified candidate and case-sensitive bare candidate",
			input:      SearchGraphInput{Query: "drain the pool", CandidateSymbols: []string{"Pool.Drain", "helper"}, Limit: 20},
			alphaNames: []string{"Helper", "HelperPool", "TestHelper"},
			betaNames:  []string{"Pool.Drain", "Pool", "TestHelper", "helperQueue"},
			joinedNames: []string{
				"beta:Pool.Drain", "alpha:Helper", "beta:Pool", "alpha:HelperPool", "beta:TestHelper", "alpha:TestHelper", "beta:helperQueue",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoOutputs := make(map[string]SearchGraphOutput, 2)
			for _, repo := range []struct {
				label string
				want  []string
			}{
				{label: "alpha", want: tc.alphaNames},
				{label: "beta", want: tc.betaNames},
			} {
				input := tc.input
				input.Repo = repo.label
				got, err := searchGraph(context.Background(), a, input)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(searchGraphQualifiedNames(got.Results), repo.want) {
					t.Errorf("repo %s search names = %v, want %v", repo.label, searchGraphQualifiedNames(got.Results), repo.want)
				}
				repoOutputs[repo.label] = got
			}

			joined, err := searchGraph(context.Background(), a, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(searchGraphLabeledNames(joined.Results), tc.joinedNames) {
				t.Errorf("joined search names = %v, want %v", searchGraphLabeledNames(joined.Results), tc.joinedNames)
			}
			assertJoinedSearchMatchesRepos(t, joined, repoOutputs)
		})
	}

	cInput := SearchGraphInput{
		Query: "drain the pool", CandidateSymbols: []string{"Helper", "HelperPool", "Drain", "Pool"}, Limit: 20,
	}
	repoOutputs := make(map[string]SearchGraphOutput, 2)
	repoExactNames := make(map[string][]string, 2)
	for _, repo := range []struct {
		label      string
		exactNames []string
	}{
		{label: "alpha", exactNames: []string{"Helper", "HelperPool"}},
		{label: "beta", exactNames: []string{"Pool.Drain", "Pool"}},
	} {
		input := cInput
		input.Repo = repo.label
		got, err := searchGraph(context.Background(), a, input)
		if err != nil {
			t.Fatal(err)
		}
		var gotExact []string
		for _, result := range got.Results {
			for _, name := range repo.exactNames {
				if result.QualifiedName == name {
					gotExact = append(gotExact, result.QualifiedName)
					break
				}
			}
		}
		gotExactSorted := append([]string(nil), gotExact...)
		wantExactSorted := append([]string(nil), repo.exactNames...)
		sort.Strings(gotExactSorted)
		sort.Strings(wantExactSorted)
		if !reflect.DeepEqual(gotExactSorted, wantExactSorted) {
			t.Fatalf("repo %s exact names = %v, want %v", repo.label, gotExact, repo.exactNames)
		}
		repoExactNames[repo.label] = gotExact
		repoOutputs[repo.label] = got
	}
	joined, err := searchGraph(context.Background(), a, cInput)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"alpha:" + repoExactNames["alpha"][0], "beta:" + repoExactNames["beta"][0],
		"alpha:" + repoExactNames["alpha"][1], "beta:" + repoExactNames["beta"][1],
	}
	joinedNames := searchGraphLabeledNames(joined.Results)
	if len(joinedNames) < len(wantPrefix) || !reflect.DeepEqual(joinedNames[:len(wantPrefix)], wantPrefix) {
		t.Errorf("joined exact prefix = %v, want %v", joinedNames, wantPrefix)
	}
	assertJoinedSearchMatchesRepos(t, joined, repoOutputs)
}

func TestOneServedRepoSearchKeepsRepoOrder(t *testing.T) {
	folder, alpha, _ := searchOrderRepos(t)
	a := codelessApp(t, folder, alpha)
	for _, rc := range a.Repos {
		indexRepo(t, rc)
	}

	input := SearchGraphInput{Query: "TestHelper"}
	implicit, err := searchGraph(context.Background(), a, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Repo = "alpha"
	explicit, err := searchGraph(context.Background(), a, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(implicit, explicit) {
		t.Fatalf("implicit result = %+v, explicit = %+v", implicit, explicit)
	}
	want := []string{"Helper", "HelperPool", "TestHelper"}
	if got := searchGraphQualifiedNames(implicit.Results); !reflect.DeepEqual(got, want) {
		t.Fatalf("one-repo search names = %v, want %v", got, want)
	}
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

func TestQueryGraphResolvesRepoBeforeFilters(t *testing.T) {
	_, _, _, a := servedGraphFixture(t, true)
	_, err := queryGraph(context.Background(), a, QueryGraphInput{Repo: "missing"})
	if err == nil || !strings.HasPrefix(err.Error(), `unknown repo "missing"`) || errors.Is(err, graph.ErrEmptyQuery) {
		t.Fatalf("unknown repo with empty query = %v", err)
	}
	_, err = queryGraph(context.Background(), a, QueryGraphInput{})
	if !errors.Is(err, graph.ErrEmptyQuery) {
		t.Fatalf("served empty query = %v, want %v", err, graph.ErrEmptyQuery)
	}
}

func TestServedSnippetBatchRoutedByFileKeepsSkipped(t *testing.T) {
	_, _, _, a := servedGraphFixture(t, true)
	symbols := []string{"TryLockFile", "Missing1", "Missing2", "Missing3", "Missing4", "Missing5", "Missing6", "Missing7", "Missing8", "Missing9", "Missing10"}
	explicit, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Repo: "second", File: "store/filelock.go", Symbols: symbols})
	if err != nil {
		t.Fatal(err)
	}
	routed, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{File: "store/filelock.go", Symbols: symbols})
	if err != nil {
		t.Fatal(err)
	}
	want := explicit
	for i := range want.Snippets {
		want.Snippets[i].Repo = "second"
	}
	want.Repo = "second"
	if !reflect.DeepEqual(routed, want) || !reflect.DeepEqual(routed.Skipped, []string{"Missing10"}) {
		t.Fatalf("routed batch = %+v, want %+v with skipped %q", routed, want, "Missing10")
	}
}

func TestServedSnippetBatchHintsOnce(t *testing.T) {
	_, _, _, a := servedGraphFixture(t, true)
	out, err := getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: []string{"SessionStart", "TryLockFile", "NoSuchA", "NoSuchB"}})
	if err != nil {
		t.Fatal(err)
	}
	wantRepos := []string{"recarto", "second"}
	if len(out.Snippets) != 4 || !reflect.DeepEqual(out.Snippets[0].Repos, wantRepos) || !reflect.DeepEqual(out.Snippets[1].Repos, wantRepos) {
		t.Fatalf("ambiguous batch entries = %+v", out.Snippets)
	}
	if strings.Count(out.Hint, "call again with one of them as repo") != 1 || !strings.Contains(out.Hint, noSymbolHint("NoSuchA or NoSuchB", "recarto, second")) {
		t.Fatalf("batch hint = %q", out.Hint)
	}
	out, err = getCodeSnippet(context.Background(), a, GetCodeSnippetInput{Symbols: []string{"TryLockFile", "Warm"}, File: "no/such/file.go"})
	if err != nil || out.Hint != noFileHint("no/such/file.go", "recarto, second") {
		t.Fatalf("missing-file batch hint = %q, err %v", out.Hint, err)
	}
}

func TestServedTextReferencesReportOmittedHits(t *testing.T) {
	_, recarto, _, a := servedGraphFixture(t, true)
	content := []byte("RECARTO_QUICKCHECK_INTERVAL\nRECARTO_QUICKCHECK_INTERVAL\nRECARTO_QUICKCHECK_INTERVAL\n")
	if err := os.WriteFile(filepath.Join(recarto, "NOTES.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := findReferences(context.Background(), a, FindReferencesInput{Symbol: "RECARTO_QUICKCHECK_INTERVAL", Limit: 2})
	if err != nil || out.Count != 2 || !out.Truncated || out.Resolved || !strings.Contains(out.Hint, "limit is shared") {
		t.Fatalf("limited text references = %+v, err %v", out, err)
	}
	out, err = findReferences(context.Background(), a, FindReferencesInput{Symbol: "RECARTO_QUICKCHECK_INTERVAL", Limit: 3})
	if err != nil || out.Count != 3 || out.Truncated || out.Resolved || out.Hint != "" {
		t.Fatalf("complete text references = %+v, err %v", out, err)
	}
}
