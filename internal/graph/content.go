package graph

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
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
	PathGlob   string // optional filepath.Match glob over the repo-relative path
	Language   string // optional language filter (go, csharp, …)
	Limit      int    // max matches returned (default 50)
	MaxPerFile int    // max matches per file (default 5)
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
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.MaxPerFile <= 0 {
		opts.MaxPerFile = 5
	}

	re, err := compilePattern(pattern, opts.Regex, opts.IgnoreCase)
	if err != nil {
		return nil, err
	}

	q := `SELECT path FROM files WHERE worktree = ?`
	args := []any{worktree}
	if opts.Language != "" {
		q += ` AND language = ?`
		args = append(args, opts.Language)
	}
	q += ` ORDER BY path`
	rows, err := e.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		if opts.PathGlob != "" && !globMatch(opts.PathGlob, p) {
			continue
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var hand, gen []ContentMatch
	for _, p := range paths {
		content, rerr := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(p)))
		if rerr != nil {
			continue // file vanished since index; skip
		}
		// Whole-file pre-filter: the large majority of files contain no match, so
		// one scan here avoids splitting them into lines and the per-line regex.
		if !re.Match(content) {
			continue
		}
		spans, serr := e.fileSpans(ctx, worktree, p)
		if serr != nil {
			return nil, serr
		}
		bucket := &hand
		if isGeneratedPath(p) {
			bucket = &gen
		}
		perFile := 0
		var off int64
		for _, line := range strings.SplitAfter(string(content), "\n") {
			if loc := re.FindStringIndex(line); loc != nil {
				*bucket = append(*bucket, ContentMatch{
					Path:   p,
					Line:   lineNumberAt(content, off),
					Text:   capLine(strings.TrimSpace(strings.TrimRight(line, "\n"))),
					Symbol: spans.symbolAt(off + int64(loc[0])),
					Kind:   spans.kindAt(off + int64(loc[0])),
				})
				perFile++
				if perFile >= opts.MaxPerFile {
					break
				}
			}
			off += int64(len(line))
		}
	}
	// Generated-file matches sink below hand-written ones (mirrors search), then
	// the combined list is capped to the budget. Nothing hand-written is dropped
	// before a generated hit.
	out := append(hand, gen...)
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return out, nil
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

// globMatch reports whether a repo-relative path matches glob. A glob with no
// slash is matched against the basename (so "*.cs" hits "src/a.cs"); otherwise it
// is matched against the full path.
func globMatch(glob, p string) bool {
	if !strings.Contains(glob, "/") {
		if ok, _ := path.Match(glob, path.Base(p)); ok {
			return true
		}
	}
	ok, _ := path.Match(glob, p)
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
