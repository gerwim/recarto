package graph

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

// ContentMatch is one line in a source file matching a content search, annotated
// with the enclosing symbol from the graph so the LLM gets grep-style recall
// plus structural context in a single result.
type ContentMatch struct {
	Path   string // repo-relative, slash-separated
	Line   int    // 1-based line number
	Text   string // the matching line, trimmed and length-capped
	Symbol string // qualified name of the enclosing definition, or "" at file scope
	Kind   string // kind of the enclosing definition (method, class, …)
}

// ContentSearchOpts tunes a content search.
type ContentSearchOpts struct {
	Regex      bool   // treat pattern as a regular expression (default: literal substring)
	IgnoreCase bool   // case-insensitive match
	PathGlob   string // optional doublestar glob over the repo-relative path (** spans dirs)
	Language   string // optional language filter (go, csharp, …)
	Limit      int    // max matches returned (default 50)
	MaxPerFile int    // max matches per file (default 5)
	Offset     int    // skip the first N matches of the stable result order (paging)
}

// ContentMeta describes the completeness of a content search so a caller can tell
// the LLM whether the page is exhaustive. Without it, the global/per-file caps
// truncate silently and grep-style "I've seen every hit" reasoning is wrong.
type ContentMeta struct {
	Total         int  // every matching line across all matching files, both caps ignored
	Shown         int  // number of matches actually returned (== len(result))
	Truncated     bool // Total > Shown: some matches were withheld
	PerFileCapped bool // at least one file had more matches than MaxPerFile
}

// nodeSpan carries a node's identity and byte range for enclosing-symbol lookup.
type nodeSpan struct {
	qn, kind   string
	start, end int64
}

// SearchContent runs a full-text (literal or regex) search over the worktree's
// indexed source files, returning each matching line annotated with its enclosing
// symbol. It is the structural counterpart to grep: same recall over arbitrary
// text (string literals, error messages, config keys, call sites inside bodies),
// but every hit is placed in the code graph.
func (e *Engine) SearchContent(ctx context.Context, worktree, pattern string, opts ContentSearchOpts) ([]ContentMatch, error) {
	matches, _, err := e.SearchContentWithMeta(ctx, worktree, pattern, opts)
	return matches, err
}

// SearchContentWithMeta is SearchContent plus a ContentMeta describing whether
// the returned page is complete. It counts every matching line (ignoring both the
// global limit and the per-file cap) so the caller can report "showing N of M"
// instead of presenting a silently-truncated page as the whole answer.
func (e *Engine) SearchContentWithMeta(ctx context.Context, worktree, pattern string, opts ContentSearchOpts) ([]ContentMatch, ContentMeta, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.MaxPerFile <= 0 {
		opts.MaxPerFile = 5
	}
	var meta ContentMeta

	re, err := compilePattern(pattern, opts.Regex, opts.IgnoreCase)
	if err != nil {
		return nil, meta, err
	}

	// File set. A language filter only makes sense for parsed code, so it stays
	// scoped to the index (files table). Without one, grep scans the whole
	// worktree — including config/build/docs/data files the indexer never parsed
	// (.csproj, .json, .md, .yaml) — so it is a true ripgrep replacement and an
	// agent never has to shell out for a file the graph happens not to cover.
	var paths []string
	if opts.Language != "" {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT path FROM files WHERE worktree = ? AND language = ? ORDER BY path`,
			worktree, opts.Language)
		if err != nil {
			return nil, meta, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, meta, err
			}
			paths = append(paths, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, meta, err
		}
	} else {
		all, lerr := listSearchableFiles(worktree)
		if lerr != nil {
			return nil, meta, lerr
		}
		paths = all
	}
	if opts.PathGlob != "" {
		kept := paths[:0]
		for _, p := range paths {
			if globMatch(opts.PathGlob, p) {
				kept = append(kept, p)
			}
		}
		paths = kept
	}

	// Scan files in parallel: reading + matching is embarrassingly parallel and
	// dominated by bytes throughput (a serial pass over a multi-hundred-MB
	// worktree was the whole latency). Workers fill results[i] for paths[i]; the
	// merge below runs sequentially in path order, so output order, caps, and
	// meta are byte-identical to the serial form.
	//
	// requiredLiteral is the second lever: a literal that provably occurs in
	// every match lets the common no-match file be rejected with one
	// bytes.Contains pass instead of a full regex scan.
	prefilter := requiredLiteral(re.String())
	results := make([]*fileScan, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, p := range paths {
		if hasBinaryExt(p) {
			continue // image/archive/compiled blob: never grep-able text
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer func() { <-sem; wg.Done() }()
			results[i] = e.scanFile(ctx, worktree, p, re, prefilter, opts.MaxPerFile)
		}(i, p)
	}
	wg.Wait()

	var hand, gen []ContentMatch
	for _, r := range results {
		if r == nil || r.err != nil {
			continue // unreadable/binary/no-match files contribute nothing
		}
		bucket := &hand
		if isGeneratedPath(r.path) {
			bucket = &gen
		}
		*bucket = append(*bucket, r.matches...)
		meta.Total += r.total
		if r.total > opts.MaxPerFile {
			meta.PerFileCapped = true
		}
	}
	// Generated-file matches sink below hand-written ones (mirrors search), then
	// the combined list is capped to the budget. Nothing hand-written is dropped
	// before a generated hit.
	out := append(hand, gen...)
	// Offset pages through the post-cap ordering (hand-written before generated,
	// path order) — the same stable order every call returns, so offset+limit
	// walks it without overlap. Overshooting is an empty page, not an error.
	if opts.Offset > 0 {
		if opts.Offset >= len(out) {
			out = out[:0]
		} else {
			out = out[opts.Offset:]
		}
	}
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	meta.Shown = len(out)
	meta.Truncated = meta.Total > opts.Offset+meta.Shown
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, meta, nil
}

// fileScan is one file's scan result: its kept (capped) matches and the true
// per-line match count, produced by a parallel worker and merged in path order.
type fileScan struct {
	path    string
	matches []ContentMatch
	total   int
	err     error
}

// scanFile greps one file. It returns nil when the file contributes nothing (a
// vanished/binary file, or simply no match) so the merge can skip it without
// distinguishing the cases. The per-LINE regex semantics are preserved exactly:
// each line is matched on its own, so ^ anchors to the line and a cross-line
// pattern never matches — identical to the original SplitAfter loop, but
// without materializing the file as strings.
func (e *Engine) scanFile(ctx context.Context, worktree, p string, re *regexp.Regexp, prefilter string, maxPerFile int) *fileScan {
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(p)))
	if err != nil {
		return nil // file vanished since listing; skip
	}
	if looksBinary(content) {
		return nil // NUL byte in the head ⇒ binary; lines would be garbage tokens
	}
	// Whole-file pre-filter: the large majority of files contain no match. When a
	// required literal is known, one sub-string pass rejects them; otherwise an
	// unanchored pattern is gated by one whole-file regex pass. An ^/$-anchored
	// pattern means something different against the whole file than against each
	// line (the loop below matches per line), so the regex gate is unsound for it
	// — the old form silently made `^foo` match nothing — and is skipped.
	switch {
	case prefilter != "":
		if !bytes.Contains(content, []byte(prefilter)) {
			return nil
		}
	case !hasLineAnchors(re.String()):
		if !re.Match(content) {
			return nil
		}
	}
	spans, serr := e.fileSpans(ctx, worktree, p)
	if serr != nil {
		return &fileScan{path: p, err: serr}
	}
	fs := &fileScan{path: p}
	var off int64
	lineNo := 1
	for rest := content; len(rest) > 0; lineNo++ {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		rest = rest[len(line):]
		if loc := re.FindIndex(line); loc != nil {
			fs.total++
			if fs.total <= maxPerFile {
				fs.matches = append(fs.matches, ContentMatch{
					Path:   p,
					Line:   lineNo,
					Text:   capLine(strings.TrimSpace(strings.TrimRight(string(line), "\n"))),
					Symbol: spans.symbolAt(off + int64(loc[0])),
					Kind:   spans.kindAt(off + int64(loc[0])),
				})
			}
		}
		off += int64(len(line))
	}
	if fs.total == 0 {
		return nil
	}
	return fs
}

// requiredLiteral returns a literal substring that EVERY match of the pattern
// must contain, or "" when none can be proven. It is deliberately conservative:
// alternations and case-folded literals yield "" (scan everything) rather than
// risk skipping a matching file. Zero-width assertions (\b, ^, $) are skipped
// without invalidating their siblings, so the common `\bIdentifier\b` reference
// search gets its identifier as the filter.
func requiredLiteral(expr string) string {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return ""
	}
	return reqLit(re.Simplify())
}

func reqLit(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return "" // (?i) literal: bytes.Contains would miss folded variants
		}
		return string(re.Rune)
	case syntax.OpConcat:
		best := ""
		for _, s := range re.Sub {
			if l := reqLit(s); len(l) > len(best) {
				best = l
			}
		}
		return best
	case syntax.OpCapture:
		return reqLit(re.Sub[0])
	case syntax.OpPlus:
		return reqLit(re.Sub[0]) // one occurrence is guaranteed
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return reqLit(re.Sub[0])
		}
	}
	return "" // alternation, star, quest, classes, …: nothing provably required
}

// hasLineAnchors reports whether the pattern uses ^/$ (or \A/\z) assertions,
// which match differently against a whole file than against a single line.
func hasLineAnchors(expr string) bool {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return true // unparseable here ⇒ be conservative, skip the gate
	}
	var walk func(*syntax.Regexp) bool
	walk = func(re *syntax.Regexp) bool {
		switch re.Op {
		case syntax.OpBeginText, syntax.OpEndText, syntax.OpBeginLine, syntax.OpEndLine:
			return true
		}
		for _, s := range re.Sub {
			if walk(s) {
				return true
			}
		}
		return false
	}
	return walk(re)
}

// maxLineLen caps a returned match line so one pathological minified/generated
// line cannot blow the token budget.
const maxLineLen = 240

func capLine(s string) string {
	if len(s) <= maxLineLen {
		return s
	}
	return s[:maxLineLen] + " …"
}

// compilePattern builds the line matcher. A literal pattern is quoted so regex
// metacharacters are matched verbatim; case-insensitivity is a regex flag either way.
func compilePattern(pattern string, asRegex, ignoreCase bool) (*regexp.Regexp, error) {
	expr := pattern
	if !asRegex {
		expr = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// listSearchableFiles enumerates every file grep should scan in worktree. When
// worktree is a git repo it uses `git ls-files` (tracked + untracked-but-not-
// ignored), so .gitignore is honored exactly like ripgrep; otherwise it walks the
// filesystem under the directory denylist. Unlike the indexer's Source.List it
// does NOT filter by code-language extension — grep must reach config, build,
// docs, and data files the graph never parses.
func listSearchableFiles(worktree string) ([]string, error) {
	out, err := exec.Command("git", "-C", worktree,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err == nil {
		seen := make(map[string]bool)
		var paths []string
		for _, rel := range strings.Split(string(out), "\x00") {
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		return paths, nil
	}
	// Not a git repo (or git missing): walk the filesystem, skipping the denylist.
	var paths []string
	werr := filepath.WalkDir(worktree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(worktree, p)
		if rerr != nil {
			return rerr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	sort.Strings(paths)
	return paths, nil
}

// binaryExt lists extensions whose contents are never grep-able text, so the file
// is skipped before it is even read. (SVG is XML and deliberately absent.)
var binaryExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".bmp": true, ".tiff": true, ".pdf": true, ".zip": true,
	".gz": true, ".tar": true, ".tgz": true, ".7z": true, ".rar": true,
	".dll": true, ".exe": true, ".so": true, ".dylib": true, ".bin": true,
	".class": true, ".o": true, ".a": true, ".lib": true, ".pdb": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	".mp4": true, ".mov": true, ".mp3": true, ".wav": true, ".avi": true,
	".snk": true, ".pfx": true, ".nupkg": true, ".wasm": true,
}

func hasBinaryExt(p string) bool {
	return binaryExt[strings.ToLower(filepath.Ext(p))]
}

// looksBinary reports whether content is binary, detected by a NUL byte in the
// head — the same heuristic grep and ripgrep use to skip non-text files.
func looksBinary(content []byte) bool {
	return bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0
}

// globMatch reports whether the repo-relative, slash-separated path p matches
// glob. It uses doublestar semantics: a single * stays within one path segment
// while ** spans separators, so recursive patterns (**/*.cs, src/**, src/**/*.cs)
// work — unlike stdlib path.Match, which has no ** and silently filtered every
// nested file to zero. A glob with no slash is matched against the basename so
// the common *.cs / *Handler*.cs forms still match files at any depth.
func globMatch(glob, p string) bool {
	if !strings.Contains(glob, "/") {
		if ok, _ := doublestar.Match(glob, path.Base(p)); ok {
			return true
		}
	}
	ok, _ := doublestar.Match(glob, p)
	return ok
}

// fileSpans loads the byte ranges of every definition node in a file, for
// enclosing-symbol attribution.
type fileSpanSet []nodeSpan

func (e *Engine) fileSpans(ctx context.Context, worktree, path string) (fileSpanSet, error) {
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT n.qualified_name, n.kind, n.start_byte, n.end_byte
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ?`, worktree, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var spans fileSpanSet
	for rows.Next() {
		var s nodeSpan
		if err := rows.Scan(&s.qn, &s.kind, &s.start, &s.end); err != nil {
			return nil, err
		}
		spans = append(spans, s)
	}
	return spans, rows.Err()
}

// innermostAt returns the smallest-range span containing byte b, or nil.
func (s fileSpanSet) innermostAt(b int64) *nodeSpan {
	var best *nodeSpan
	for i := range s {
		if b >= s[i].start && b < s[i].end {
			if best == nil || (s[i].end-s[i].start) < (best.end-best.start) {
				best = &s[i]
			}
		}
	}
	return best
}

func (s fileSpanSet) symbolAt(b int64) string {
	if n := s.innermostAt(b); n != nil {
		return n.qn
	}
	return ""
}

func (s fileSpanSet) kindAt(b int64) string {
	if n := s.innermostAt(b); n != nil {
		return n.kind
	}
	return ""
}
