package agents

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/memory"
)

// extGlobRE matches a wildcard basename ending in a file extension, e.g. "*.cs",
// "*Foo*.ts" — the shape that marks a pattern as a file glob rather than a Grep
// term. The extension is 1–5 alphanumerics so it never matches a regex like
// "Add.*Projection" (which ends in an identifier, not a ".<ext>"). The run between
// the wildcard and the extension dot is restricted to identifier/wildcard chars
// (no regex metacharacters), so an escaped-dot Grep regex like "import.*\.proto"
// or "foo.*\.go" is NOT mistaken for a glob: the backslash before the extension
// breaks the run, leaving the pattern to pass through to symbol search unchanged.
var extGlobRE = regexp.MustCompile(`[*?][A-Za-z0-9_*?]*\.[A-Za-z0-9]{1,5}$`)

// identRunRE captures maximal identifier runs used to recover a glob's stem.
var identRunRE = regexp.MustCompile(`[A-Za-z0-9_]+`)

// minGlobStem is the shortest identifier stem worth feeding to symbol search; a
// shorter remnant (e.g. the "cs" of "*.cs") is an extension, not a symbol query.
const minGlobStem = 3

// globStem returns the longest identifier run (≥ minGlobStem) of a file
// glob's basename without extension; ok=false when there is none. A
// pure-extension or stemless glob ("*.cs", "src/**/*.go", "internal/**")
// names no symbol: a literal "cs" search would only find minified-JS noise.
func globStem(glob string) (string, bool) {
	base := path.Base(glob)
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

// searchIdentifiers returns the identifiers a search pattern names, deduped in
// first-seen order: a file glob's stem, or each whole word of a Grep pattern
// that is an identifier or a dotted chain of identifiers.
func searchIdentifiers(pattern string) []string {
	if looksLikeFileGlob(pattern) {
		if stem, ok := globStem(pattern); ok {
			return []string{stem}
		}
		return nil
	}
	// An escaped dot is a literal dot; any other escape (\b, \s, \|, \() is regex
	// syntax and names nothing, so it only separates words.
	var b strings.Builder
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' {
			b.WriteRune(rs[i])
			continue
		}
		if i+1 < len(rs) && rs[i+1] == '.' {
			b.WriteRune('.')
		} else {
			b.WriteRune(' ')
		}
		i++
	}
	words := strings.FieldsFunc(b.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-/", r)
	})
	var out []string
	for _, w := range words {
		// Paths, kebab-case file names and flags are not identifiers.
		if strings.ContainsAny(w, "-/") {
			continue
		}
		w = strings.Trim(w, ".")
		if w == "" || slices.Contains(out, w) || !isIdentChain(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// isIdentChain reports whether w is one identifier or a dotted chain of them
// with at least one letter: "404", "v2..x" and a bare "_" are not.
func isIdentChain(w string) bool {
	for seg := range strings.SplitSeq(w, ".") {
		if seg == "" {
			return false
		}
		if r := []rune(seg)[0]; !unicode.IsLetter(r) && r != '_' {
			return false
		}
	}
	return strings.IndexFunc(w, unicode.IsLetter) >= 0
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

// Search-hint caps per tool call. hintMax counts bytes, so the hint is at most
// that many characters however they are counted.
const (
	hintSymbolMax = 5
	hintBugMax    = 2
	hintMax       = 800
)

// searchHint returns the hint for one tool call's search patterns, "" when it
// has nothing to list. See README "Search hints".
func searchHint(ctx context.Context, a *app.App, scope string, patterns []string) (string, error) {
	pending, names, err := hintPatterns(ctx, a, scope, patterns)
	if err != nil || len(names) == 0 {
		return "", err
	}
	repos := a.AllRepos() // fresh contexts per call; listed hits are matched to these by pointer
	symbols, listed, size, err := hintSymbols(ctx, a, repos, names)
	if err != nil || len(listed) == 0 {
		return "", err
	}
	bugs, printed, err := hintBugs(ctx, a, scope, repos, listed, size)
	if err != nil {
		return "", err
	}

	markSurfaced(ctx, a, scope, printed)
	if scope != "" {
		for p, ids := range pending {
			if slices.ContainsFunc(listed, func(h hintHit) bool {
				return slices.Contains(ids, h.r.Name) || slices.Contains(ids, h.r.QualifiedName)
			}) {
				// A lost mark only lets the pattern hint again.
				_ = a.Ledger.Incr(ctx, scope, searchMetric+p, 1)
			}
		}
	}
	return symbols + bugs, nil
}

// hintHit is a symbol a search hint lists, with the repo that defines it.
type hintHit struct {
	rc *app.RepoContext
	r  graph.SearchResult
}

// hintPatterns returns the identifiers of each pattern not yet hinted in
// scope's conversation, and all of them deduped. A repeat is counted in
// repeated_augments_blocked.
func hintPatterns(ctx context.Context, a *app.App, scope string, patterns []string) (map[string][]string, []string, error) {
	pending := map[string][]string{}
	seen := map[string]bool{}
	var names []string
	for _, p := range patterns {
		if seen[p] {
			continue
		}
		seen[p] = true
		ids := searchIdentifiers(p)
		if len(ids) == 0 {
			continue
		}
		if scope != "" {
			n, err := a.Ledger.Get(ctx, scope, searchMetric+p)
			if err != nil {
				return nil, nil, err
			}
			if n > 0 {
				_ = a.Ledger.Incr(ctx, "lifetime", "repeated_augments_blocked", 1)
				continue
			}
		}
		pending[p] = ids
		for _, id := range ids {
			if !slices.Contains(names, id) {
				names = append(names, id)
			}
		}
	}
	return pending, names, nil
}

// hintSymbols renders the symbol section (header, lines, footer) for the
// symbols named, within hintMax. It returns the section, the listed symbols
// and the section's byte length.
func hintSymbols(ctx context.Context, a *app.App, repos []*app.RepoContext, names []string) (string, []hintHit, int, error) {
	var hits []hintHit
	for _, rc := range repos {
		res, err := rc.Graph.SymbolsNamed(ctx, rc.Git.Worktree, names, hintSymbolMax)
		if err != nil {
			return "", nil, 0, err
		}
		for _, r := range res {
			hits = append(hits, hintHit{rc, r})
		}
	}
	// Stable, so within a tier the primary repo (first in repos) leads.
	slices.SortStableFunc(hits, func(x, y hintHit) int { return graph.ValueTier(x.r) - graph.ValueTier(y.r) })

	const header = "Recarto: symbols named in your search:"
	footer := ""
	if a.Features.On(features.GraphTools) {
		footer = "\nUse get_code_snippet to read one symbol instead of the whole file."
	}
	size := len(header) + len(footer)
	var b strings.Builder
	b.WriteString(header)
	var listed []hintHit
	for _, h := range hits {
		if len(listed) == hintSymbolMax {
			break
		}
		prefix := ""
		if h.rc.Git.RepoID != a.Git.RepoID {
			prefix = h.rc.Git.RepoName + ":"
		}
		line := fmt.Sprintf("\n- [%s] %s  %s%s:%d", h.r.Kind, h.r.QualifiedName, prefix, h.r.Path, h.r.Line)
		if size+len(line) > hintMax {
			break
		}
		b.WriteString(line)
		size += len(line)
		listed = append(listed, h)
	}
	b.WriteString(footer)
	return b.String(), listed, size, nil
}

// hintBugs renders the bug section for the listed symbols, fitting it in what
// size leaves of hintMax, and returns it with the ids it printed.
func hintBugs(ctx context.Context, a *app.App, scope string, repos []*app.RepoContext, listed []hintHit, size int) (string, []string, error) {
	header := "\nKnown bugs in this code"
	if a.Features.On(features.MemoryTools) {
		header += " (search_bugs for detail)"
	}
	header += ":"
	var b strings.Builder
	var printed []string
	seen := map[string]bool{}
	for _, rc := range repos {
		if len(printed) == hintBugMax {
			break
		}
		files, syms := hintAnchors(rc, listed)
		if len(files) == 0 {
			continue
		}
		bugs, err := rc.Engine.BugsForCode(ctx, rc.Git.Branch, files, syms)
		if err != nil {
			return "", nil, err
		}
		for _, r := range bugs {
			if len(printed) == hintBugMax {
				break
			}
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			ok, err := hintableBug(ctx, a, scope, rc, r)
			if err != nil {
				return "", nil, err
			}
			line := "\n- ⚠ " + noteTitle(r)
			need := len(line)
			if len(printed) == 0 {
				need += len(header)
			}
			if !ok || size+need > hintMax {
				continue // a shorter title may still fit
			}
			if len(printed) == 0 {
				b.WriteString(header)
			}
			b.WriteString(line)
			size += need
			printed = append(printed, r.ID)
		}
	}
	return b.String(), printed, nil
}

// hintAnchors returns the files and symbol names of rc's listed symbols, deduped.
func hintAnchors(rc *app.RepoContext, listed []hintHit) (files, syms []string) {
	for _, h := range listed {
		if h.rc != rc {
			continue
		}
		if !slices.Contains(files, h.r.Path) {
			files = append(files, h.r.Path)
		}
		for _, n := range []string{h.r.Name, h.r.QualifiedName} {
			if !slices.Contains(syms, n) {
				syms = append(syms, n)
			}
		}
	}
	return files, syms
}

// hintableBug reports whether r may be listed: scope's conversation has not
// been shown it and the branch check does not flag it.
func hintableBug(ctx context.Context, a *app.App, scope string, rc *app.RepoContext, r *memory.Record) (bool, error) {
	if scope != "" {
		n, err := a.Ledger.Get(ctx, scope, shownMetric+r.ID)
		if err != nil || n > 0 {
			return false, err
		}
	}
	dangling, err := rc.Engine.Dangling(ctx, rc.Git.Worktree, r)
	return len(dangling) == 0, err
}
