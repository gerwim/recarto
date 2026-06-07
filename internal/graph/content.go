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
	"sort"
	"strings"

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

	var hand, gen []ContentMatch
	for _, p := range paths {
		if hasBinaryExt(p) {
			continue // image/archive/compiled blob: never grep-able text
		}
		content, rerr := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(p)))
		if rerr != nil {
			continue // file vanished since index; skip
		}
		if looksBinary(content) {
			continue // NUL byte in the head ⇒ binary; lines would be garbage tokens
		}
		// Whole-file pre-filter: the large majority of files contain no match, so
		// one scan here avoids splitting them into lines and the per-line regex.
		if !re.Match(content) {
			continue
		}
		spans, serr := e.fileSpans(ctx, worktree, p)
		if serr != nil {
			return nil, meta, serr
		}
		bucket := &hand
		if isGeneratedPath(p) {
			bucket = &gen
		}
		// fileMatches counts every matching line in this file; only the first
		// MaxPerFile are attributed and kept. Counting past the cap is what lets
		// meta.Total report the true match count instead of the capped page.
		fileMatches := 0
		var off int64
		for _, line := range strings.SplitAfter(string(content), "\n") {
			if loc := re.FindStringIndex(line); loc != nil {
				fileMatches++
				if fileMatches <= opts.MaxPerFile {
					*bucket = append(*bucket, ContentMatch{
						Path:   p,
						Line:   lineNumberAt(content, off),
						Text:   capLine(strings.TrimSpace(strings.TrimRight(line, "\n"))),
						Symbol: spans.symbolAt(off + int64(loc[0])),
						Kind:   spans.kindAt(off + int64(loc[0])),
					})
				}
			}
			off += int64(len(line))
		}
		meta.Total += fileMatches
		if fileMatches > opts.MaxPerFile {
			meta.PerFileCapped = true
		}
	}
	// Generated-file matches sink below hand-written ones (mirrors search), then
	// the combined list is capped to the budget. Nothing hand-written is dropped
	// before a generated hit.
	out := append(hand, gen...)
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	meta.Shown = len(out)
	meta.Truncated = meta.Total > meta.Shown
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, meta, nil
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

// lineNumberAt returns the 1-based line number of byte offset b in content.
func lineNumberAt(content []byte, b int64) int {
	if b > int64(len(content)) {
		b = int64(len(content))
	}
	return 1 + strings.Count(string(content[:b]), "\n")
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
