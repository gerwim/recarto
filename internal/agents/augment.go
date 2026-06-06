package agents

import (
	"path"
	"regexp"
	"strings"
)

// extGlobRE matches a wildcard basename ending in a file extension, e.g. "*.cs",
// "*Foo*.ts" — the shape that marks a pattern as a file glob rather than a Grep
// term. The extension is 1–5 alphanumerics so it never matches a regex like
// "Add.*Projection" (which ends in an identifier, not a ".<ext>").
var extGlobRE = regexp.MustCompile(`[*?][^/]*\.[A-Za-z0-9]{1,5}$`)

// identRunRE captures maximal identifier runs used to recover a glob's stem.
var identRunRE = regexp.MustCompile(`[A-Za-z0-9_]+`)

// minGlobStem is the shortest identifier stem worth feeding to symbol search; a
// shorter remnant (e.g. the "cs" of "*.cs") is an extension, not a symbol query.
const minGlobStem = 3

// augmentSearchTerm derives the symbol-search term for an augment (Grep/Glob)
// pattern, distinguishing a Grep identifier/regex from a file glob.
//
//   - A pattern that does not look like a file glob passes through unchanged, so
//     ordinary Grep searches ("AddProjection", "Add.*Projection", "foo*") behave
//     exactly as before — no regression.
//   - A file glob is reduced to the longest identifier run of its basename (minus
//     extension and wildcards): "**/*Projection*.cs" → "Projection".
//   - A pure-extension or stemless glob ("*.cs", "src/**/*.go", "internal/**")
//     carries no symbol signal and returns ok=false, so the caller injects
//     nothing rather than the minified-JS noise a literal "cs" search produces.
func augmentSearchTerm(pattern string) (term string, ok bool) {
	if !looksLikeFileGlob(pattern) {
		return pattern, true
	}
	base := path.Base(pattern)
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i] // drop the extension
	}
	stem := ""
	for _, run := range identRunRE.FindAllString(base, -1) {
		if len(run) > len(stem) {
			stem = run
		}
	}
	if len(stem) < minGlobStem {
		return "", false
	}
	return stem, true
}

// looksLikeFileGlob reports whether a pattern is a filename/path glob rather than
// a Grep regex. Deliberately narrow: a regex that merely contains "*" (a
// quantifier) is not a file glob unless it also has the path/extension shape.
func looksLikeFileGlob(p string) bool {
	if strings.Contains(p, "**") {
		return true
	}
	hasWildcard := strings.ContainsAny(p, "*?")
	if hasWildcard && strings.Contains(p, "/") {
		return true
	}
	return extGlobRE.MatchString(p)
}
