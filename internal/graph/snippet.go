package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

// SnippetResult is the structured result of Snippet. A miss is Found=false (not
// an error); only genuine faults (stale byte range, vanished file) return an error.
type SnippetResult struct {
	Found         bool
	StartLine     int
	EndLine       int
	QualifiedName string
	Snippet       string
}

// SnippetCandidate locates one definition that matched a file-less snippet lookup
// when the bare name is ambiguous, so the caller can disambiguate by re-querying
// with the qualified name (or by supplying the file).
type SnippetCandidate struct {
	Path          string
	StartLine     int
	Kind          string
	QualifiedName string
}

// maxSnippetCandidates caps the disambiguation list so a very common name cannot
// blow the token budget; the count reported to the caller is the true total.
const maxSnippetCandidates = 25

// SnippetByName resolves a symbol when the file is unknown — the common case
// right after a search returns a name. It matches name or qualified_name across
// the worktree: exactly one definition returns its source (Found=true, no
// candidates); several return Found=false with the candidate definitions so the
// caller can disambiguate (a qualified name usually resolves uniquely); zero
// return Found=false with no candidates. This removes the search→snippet
// round-trip and the wrong-path errors of having to know the file first.
func (e *Engine) SnippetByName(ctx context.Context, worktree, symbol string) (SnippetResult, []SnippetCandidate, error) {
	// A dotted input like "A.Run" (Type.Method without the namespace) is matched as
	// a suffix of the fully qualified name — mirroring trace's nodesByName — so an
	// identifier an LLM copied out of `graph callees`/`callers` resolves here too
	// instead of falsely missing and forcing a whole-file read. The suffix is only
	// armed when the input contains a dot, so a bare name never triggers a broad
	// LIKE; an exact qualified_name match is flagged and preferred over loose hits.
	suffix := "\x00" // sentinel: never matches when input has no dot
	if strings.Contains(symbol, ".") {
		suffix = "%." + symbol
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT f.path, n.start_line, n.kind, n.qualified_name, (n.qualified_name = ?) AS exact
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?)
 ORDER BY exact DESC, f.path, n.start_line
 LIMIT ?`, symbol, worktree, symbol, symbol, suffix, maxSnippetCandidates+1)
	if err != nil {
		return SnippetResult{}, nil, err
	}
	var cands []SnippetCandidate
	var qualified bool
	for rows.Next() {
		var c SnippetCandidate
		var exact bool
		if err := rows.Scan(&c.Path, &c.StartLine, &c.Kind, &c.QualifiedName, &exact); err != nil {
			rows.Close()
			return SnippetResult{}, nil, err
		}
		if exact && !qualified {
			// First exact qualified-name hit: a precise query. Discard looser
			// same-bare-name / suffix matches and keep only the exact one(s).
			qualified = true
			cands = cands[:0]
		}
		if qualified && !exact {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SnippetResult{}, nil, err
	}
	cands = collapseOwnedConstructors(cands)
	switch {
	case len(cands) == 0:
		return SnippetResult{Found: false}, nil, nil
	case len(cands) == 1:
		res, err := e.Snippet(ctx, worktree, cands[0].Path, symbol)
		return res, nil, err
	case isOverloadSet(cands):
		// Every match is the same logical symbol in one file: an overload set. Neither
		// a qualified name nor the file can pick one out, so bouncing the caller with
		// an "ambiguous" prompt is a dead end that forces a whole-file read. The
		// overloads ARE the answer — return all their bodies so the agent stays in
		// recarto.
		return e.overloadSnippet(ctx, worktree, cands)
	default:
		return SnippetResult{Found: false}, cands, nil
	}
}

// isOverloadSet reports whether every candidate is the same logical symbol — one
// qualified name in one file: a method overload set. That is exactly the case the
// candidate list cannot disambiguate (same qualified name AND same file), so it is
// safe to merge. Same-name symbols in *different* files keep their file as a
// disambiguator and stay ambiguous; a bare/empty qualified name is never merged.
func isOverloadSet(cands []SnippetCandidate) bool {
	if len(cands) < 2 {
		return false
	}
	qn, path := cands[0].QualifiedName, cands[0].Path
	if qn == "" || !strings.Contains(qn, ".") {
		return false // a bare name is not a reliable identity; treat as ambiguous
	}
	for _, c := range cands[1:] {
		if c.QualifiedName != qn || c.Path != path {
			return false
		}
	}
	return true
}

// maxOverloadBodies caps how many overload bodies are inlined into one combined
// snippet; beyond this the remaining locations are listed so a pathological
// overload set cannot blow the token budget.
const maxOverloadBodies = 10

// overloadSnippet concatenates the source of each overload (same qualified name),
// each preceded by a "// path:line" header so every body stays individually
// locatable. The result is Found=true with no candidates: the caller renders the
// combined snippet exactly like a single hit.
func (e *Engine) overloadSnippet(ctx context.Context, worktree string, cands []SnippetCandidate) (SnippetResult, []SnippetCandidate, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s — %d overloads\n", cands[0].QualifiedName, len(cands))
	shown := cands
	var overflow []SnippetCandidate
	if len(cands) > maxOverloadBodies {
		shown, overflow = cands[:maxOverloadBodies], cands[maxOverloadBodies:]
	}
	startLine, endLine := 0, 0
	for i, c := range shown {
		body, sl, el, err := e.snippetAt(ctx, worktree, c.Path, c.StartLine)
		if err != nil {
			return SnippetResult{}, nil, err
		}
		if i == 0 {
			startLine = sl
		}
		endLine = el
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "// %s:%d\n%s\n", c.Path, sl, body)
	}
	for _, c := range overflow {
		fmt.Fprintf(&b, "// … also at %s:%d\n", c.Path, c.StartLine)
	}
	// Tokens saved = the whole file the agent would otherwise have read minus this
	// combined snippet. isOverloadSet guarantees every overload shares one file, so
	// reading it once is the correct baseline.
	if content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(cands[0].Path))); err == nil {
		if saved := ledger.EstimateTokens(string(content)) - ledger.EstimateTokens(b.String()); saved > 0 {
			_ = e.led.Incr(ctx, "lifetime", "graph_tokens_saved", int64(saved))
		}
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return SnippetResult{
		Found: true, StartLine: startLine, EndLine: endLine,
		QualifiedName: cands[0].QualifiedName, Snippet: b.String(),
	}, nil, nil
}

// snippetAt returns the exact source of the definition at path:startLine, using
// its stored byte range. Unlike Snippet (keyed by name, LIMIT 1), it pins a single
// overload by location so each member of an overload set is individually reachable.
func (e *Engine) snippetAt(ctx context.Context, worktree, path string, startLine int) (string, int, int, error) {
	var sb, eb int64
	var sl, el int
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte, n.start_line, n.end_line
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND n.start_line = ?
 LIMIT 1`, worktree, path, startLine).Scan(&sb, &eb, &sl, &el)
	if err != nil {
		return "", 0, 0, err
	}
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if err != nil {
		return "", 0, 0, err
	}
	if sb < 0 || eb > int64(len(content)) || sb > eb {
		return "", 0, 0, fmt.Errorf("graph: stale byte range at %s:%d (reindex needed)", path, startLine)
	}
	return string(content[sb:eb]), sl, el, nil
}

// collapseOwnedConstructors drops a constructor candidate when one of the other
// candidates is the very type that declares it — i.e. the constructor's qualified
// name is "<typeQN>.<ctorName>" and a type-like candidate has qualified name
// "<typeQN>". In C#/Java a type and its constructor share the bare name, so a
// file-less lookup for the type matched both and bounced the caller with a
// spurious "ambiguous" prompt; the constructor's source is a subset of the type's
// and is not a genuine alternative. Real ambiguity — two distinct same-named
// types — is untouched, since neither type owns the other's constructor.
func collapseOwnedConstructors(cands []SnippetCandidate) []SnippetCandidate {
	typeQNs := make(map[string]bool, len(cands))
	for _, c := range cands {
		if typeLikeKinds[c.Kind] {
			typeQNs[c.QualifiedName] = true
		}
	}
	if len(typeQNs) == 0 {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if c.Kind == "constructor" {
			if i := lastDot(c.QualifiedName); i >= 0 && typeQNs[c.QualifiedName[:i]] {
				continue // owned by a type already in the candidate set
			}
		}
		out = append(out, c)
	}
	return out
}

// typeLikeKinds are declaration kinds that own a constructor sharing their name.
var typeLikeKinds = map[string]bool{
	"class": true, "struct": true, "record": true,
	"interface": true, "enum": true, "type": true,
}

// lastDot returns the index of the final '.' in s, or -1 if there is none.
func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// Snippet returns the exact source for a symbol (by name or qualified_name) in a
// file, using its stored byte range. It records the tokens saved vs. reading the
// whole file into the ledger.
func (e *Engine) Snippet(ctx context.Context, worktree, path, symbol string) (SnippetResult, error) {
	var sb, eb int64
	var sl, el int
	var qn string
	// Accept a dotted Type.Method suffix as well as the bare name / full qualified
	// name, so the form resolved by SnippetByName (and by trace) resolves here too.
	// Exact qualified_name is ordered first so a precise input never loses to a
	// looser suffix collision within the same file.
	suffix := "\x00"
	if strings.Contains(symbol, ".") {
		suffix = "%." + symbol
	}
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte, n.start_line, n.end_line, n.qualified_name
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND (n.name = ? OR n.qualified_name = ? OR n.qualified_name LIKE ?)
 ORDER BY (n.qualified_name = ?) DESC, n.start_byte
 LIMIT 1`, worktree, path, symbol, symbol, suffix, symbol).Scan(&sb, &eb, &sl, &el, &qn)
	if errors.Is(err, sql.ErrNoRows) {
		return SnippetResult{Found: false}, nil
	}
	if err != nil {
		return SnippetResult{}, err
	}
	content, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(path)))
	if err != nil {
		return SnippetResult{}, err
	}
	if sb < 0 || eb > int64(len(content)) || sb > eb {
		return SnippetResult{}, fmt.Errorf("graph: stale byte range for %q (reindex needed)", symbol)
	}
	snippet := string(content[sb:eb])

	saved := ledger.EstimateTokens(string(content)) - ledger.EstimateTokens(snippet)
	if saved > 0 {
		_ = e.led.Incr(ctx, "lifetime", "graph_tokens_saved", int64(saved))
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return SnippetResult{Found: true, StartLine: sl, EndLine: el, QualifiedName: qn, Snippet: snippet}, nil
}
