package graph

import (
	"path"
	"slices"
	"strings"
)

// testFileSuffixes are unambiguous test-file naming conventions across ecosystems
// (Go, xUnit/NUnit C#, Jest/Vitest TS/JS). Like the generated-file list these are
// deliberately narrow: a false positive demotes hand-written product code, which
// would be a quality regression.
var testFileSuffixes = []string{
	"_test.go",
	"tests.cs", "test.cs",
	".test.ts", ".test.tsx", ".test.js", ".test.jsx",
	".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
}

// isTestPath reports whether a repo-relative path looks like test code.
//
// Detection is purely path-based (no content) so it is cheap per result at query
// time and needs no schema change. It matches a whole "test"/"tests" path segment
// (the near-universal test-directory convention) or a well-known test-file suffix.
// It is conservative — "testing", "contest", "testdata" segments do NOT match,
// since the segment test is exact — because demoting real code is worse than
// leaving a test slightly high.
func isTestPath(p string) bool {
	p = strings.ToLower(p)
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "test" || seg == "tests" {
			return true
		}
	}
	base := path.Base(p)
	return slices.ContainsFunc(testFileSuffixes, func(suf string) bool {
		return strings.HasSuffix(base, suf)
	})
}

// isTestModuleName reports whether a module name denotes test code. Module names
// are project basenames or paths (e.g. "ApolloFederation.Tests" or
// "src/HotChocolate/Core/test/Types.Tests"), so unlike isTestPath it splits on
// both '/' and '.' and matches a whole "test"/"tests" segment. It is conservative
// for the same reason ("testing", "contest", "CostAnalysis" do not match): wrongly
// demoting a product module out of a tight orientation map is the worse error.
func isTestModuleName(name string) bool {
	for _, seg := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '/' || r == '.'
	}) {
		if seg == "test" || seg == "tests" {
			return true
		}
	}
	return false
}

// demoteTests stably partitions results so test symbols sink below product code,
// each group keeping its incoming (fused) order. Like demoteGenerated it drops
// nothing — tests stay findable, just lower — so a concept query surfaces the
// implementation first without losing recall on the page. When every match is a
// test (a test-seeking query) the partition is a no-op, so the best test still
// leads: the demotion is adaptive, never burying the only relevant results.
//
// protect holds lowercased names that must NOT be demoted even in a test file —
// the agent's exact candidate_symbols guesses, whose precise landing outranks the
// test penalty. Pass nil for plain (non-candidate) search.
func demoteTests(in []SearchResult, protect map[string]bool) []SearchResult {
	isTest := func(r SearchResult) bool {
		return isTestPath(r.Path) && !protect[strings.ToLower(r.Name)]
	}
	anyTest := false
	for i := range in {
		if isTest(in[i]) {
			anyTest = true
			break
		}
	}
	if !anyTest {
		return in // no-op: preserve the input slice and its order
	}
	head := make([]SearchResult, 0, len(in))
	var tail []SearchResult
	for _, r := range in {
		if isTest(r) {
			tail = append(tail, r)
		} else {
			head = append(head, r)
		}
	}
	return append(head, tail...)
}
