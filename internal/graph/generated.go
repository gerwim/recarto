package graph

import (
	"path"
	"slices"
	"strings"
)

// generatedDirs are path segments that, when one names a directory in a file's
// path, mark every file beneath as machine-generated output (GraphQL/codegen,
// Relay-style __generated__).
var generatedDirs = map[string]bool{
	"generated":     true,
	"__generated__": true,
}

// generatedSuffixes are well-known generated filename suffixes across ecosystems
// (protobuf, gRPC, dart/csharp codegen, minified bundles, WinForms designers).
var generatedSuffixes = []string{
	".pb.go", ".pb.cc", ".pb.h", ".pb.py",
	"_pb2.py", "_pb2_grpc.py",
	".g.dart", ".g.cs", ".g.ts",
	".designer.cs",
	".min.js", ".min.css",
}

// isGeneratedPath reports whether a repo-relative path looks machine-generated.
//
// Detection is purely path-based (no file content) so it is cheap enough to run
// per result at query time and needs no schema change or re-index. It is
// deliberately conservative — only unambiguous conventions match — because a
// false positive demotes hand-written code, which would be a quality regression.
// "generator.go" and "src/codegeneration/" do NOT match: the directory test is
// on whole path segments, not substrings.
func isGeneratedPath(p string) bool {
	p = strings.ToLower(p)
	for seg := range strings.SplitSeq(p, "/") {
		if generatedDirs[seg] {
			return true
		}
	}
	base := path.Base(p)
	if strings.Contains(base, ".generated.") {
		return true
	}
	if strings.HasPrefix(base, "zz_generated") {
		return true
	}
	return slices.ContainsFunc(generatedSuffixes, func(suf string) bool {
		return strings.HasSuffix(base, suf)
	})
}

// demoteGenerated stably partitions results so machine-generated symbols sink
// below hand-written ones, while each group keeps its incoming (fused) order.
// Nothing is dropped — generated symbols remain findable, just lower — so a
// concept search surfaces real code first without losing recall on the page.
//
// protect holds lowercased symbol names that must NOT be demoted even when they
// live in a generated file: these are the agent's exact candidate_symbols
// guesses, whose "the model's guess landed precisely" signal outranks the
// generated penalty. Pass nil for plain (non-candidate) search.
func demoteGenerated(in []SearchResult, protect map[string]bool) []SearchResult {
	isGen := func(r SearchResult) bool {
		return isGeneratedPath(r.Path) && !protect[strings.ToLower(r.Name)]
	}
	anyGen := false
	for i := range in {
		if isGen(in[i]) {
			anyGen = true
			break
		}
	}
	if !anyGen {
		return in // no-op: preserve the input slice and its order
	}
	head := make([]SearchResult, 0, len(in))
	var tail []SearchResult
	for _, r := range in {
		if isGen(r) {
			tail = append(tail, r)
		} else {
			head = append(head, r)
		}
	}
	return append(head, tail...)
}
