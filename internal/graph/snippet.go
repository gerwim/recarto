package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT f.path, n.start_line, n.kind, n.qualified_name
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND (n.name = ? OR n.qualified_name = ?)
 ORDER BY f.path, n.start_line
 LIMIT ?`, worktree, symbol, symbol, maxSnippetCandidates+1)
	if err != nil {
		return SnippetResult{}, nil, err
	}
	var cands []SnippetCandidate
	for rows.Next() {
		var c SnippetCandidate
		if err := rows.Scan(&c.Path, &c.StartLine, &c.Kind, &c.QualifiedName); err != nil {
			rows.Close()
			return SnippetResult{}, nil, err
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
	default:
		return SnippetResult{Found: false}, cands, nil
	}
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
	err := e.st.DB.QueryRowContext(ctx, `
SELECT n.start_byte, n.end_byte, n.start_line, n.end_line, n.qualified_name
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ? AND (n.name = ? OR n.qualified_name = ?)
 LIMIT 1`, worktree, path, symbol, symbol).Scan(&sb, &eb, &sl, &el, &qn)
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
