package graph

import (
	"context"
	"fmt"
)

// Lexical fusion tuning. The exact (unicode61) primary pass carries full weight;
// the porter, synonym and trigram broadeners are consulted only when the primary
// pass underfills, so they recover recall on hard queries without ever
// displacing precise matches on easy ones. triMinTermLen keeps very short,
// ubiquitous terms out of the substring pass.
const (
	exactWeight   = 1.0
	porterWeight  = 0.6
	synonymWeight = 0.4
	trigramWeight = 0.25
	triMinTermLen = 4
)

// vecReserveNum/vecReserveDen reserve a minority share of the result page for the
// vector tier (here 2/5). Lexical still owns the majority of slots and all the top
// positions — so keyword queries never regress — but the reserved tail guarantees
// the semantic neighbourhood a foothold even when the lexical pass overfills the
// page with weak token matches (e.g. a "options"/"state" query that returns a full
// page of incidental name hits). Without a reserve, lexical starves the vector
// tier of every slot and concept recall collapses to zero.
const (
	vecReserveNum = 2
	vecReserveDen = 5
)

// SearchResult is one symbol match.
type SearchResult struct {
	NodeID        string
	Name          string
	QualifiedName string
	Kind          string
	Language      string
	Path          string
	Line          int
	Score         float64 // higher = more relevant
}

// Search returns symbols matching query, scoped to worktree and optionally
// filtered by kind and language. It always runs the lexical tier (an exact pass
// plus stemmed/synonym/trigram broadeners on underfill). When a semantic
// embedder is set it additionally runs a vector-similarity pass and fuses it with
// the lexical result via Reciprocal Rank Fusion, so symbols that share no tokens
// with the query (true synonyms) are still found. With no embedder, behaviour is
// purely lexical.
func (e *Engine) Search(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	lex, err := e.lexicalSearch(ctx, worktree, query, kind, lang, limit)
	if err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	if e.emb == nil {
		return dedupByName(lex, limit), nil
	}

	// Semantic tier. Embedding is best-effort: on any error, fall back to the
	// lexical result rather than failing the search.
	vec, verr := e.vectorPass(ctx, worktree, query, kind, lang, limit*3)
	if verr != nil {
		vec = nil
	}
	return mergeHybrid(lex, vec, limit), nil
}

// mergeHybrid fuses the lexical and vector tiers with a reserved tail. Lexical
// fills the non-reserved majority first, so its precise hits keep the top
// positions (hybrid never regresses a keyword query); the vector tier then fills
// the reserved tail with concept matches the lexical pass missed; finally any
// reserve the vector tier left unused is backfilled with more lexical. Dedup is by
// name throughout so a same-name overload cluster cannot crowd the page. With no
// vector candidates this degrades to a plain lexical dedup.
func mergeHybrid(lex, vec []SearchResult, limit int) []SearchResult {
	if len(vec) == 0 {
		return dedupByName(lex, limit)
	}
	lexCap := limit - limit*vecReserveNum/vecReserveDen
	seen := make(map[string]bool, limit)
	out := make([]SearchResult, 0, limit)
	push := func(rs []SearchResult, cap int) {
		for _, r := range rs {
			if len(out) >= cap {
				return
			}
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			out = append(out, r)
		}
	}
	push(lex, lexCap) // lexical majority + all top positions
	push(vec, limit)  // vector fills the reserved tail
	push(lex, limit)  // backfill if the vector tier underfilled its reserve
	return out
}

// dedupByName collapses results with the same symbol name, keeping the first
// (best-ranked) occurrence, and truncates to limit. Name-dedup is what lets the
// distinct intended targets fit on the page instead of a single name's overload
// cluster filling it.
func dedupByName(in []SearchResult, limit int) []SearchResult {
	seen := make(map[string]bool, limit)
	out := make([]SearchResult, 0, limit)
	for _, r := range in {
		if len(out) >= limit {
			break
		}
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		out = append(out, r)
	}
	return out
}

// lexicalSearch is the FTS tier: a precise exact (unicode61) pass over the
// identifier-split content, broadened only when it underfills with a
// porter-stemmed pass (morphology), a synonym-expanded pass (abbreviations) and a
// trigram pass (substring / near-miss), all fused UNDER the exact pass's weight
// so precise hits stay on top. It returns up to limit*3 candidates (the caller
// truncates to the page size) so the optional vector tier has room to fuse.
func (e *Engine) lexicalSearch(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	fetch := limit * 3
	base := baseTerms(query)

	primary, err := e.ftsPass(ctx, "nodes_fts", orMatch(base), worktree, kind, lang, fetch)
	if err != nil {
		return nil, err
	}
	// Strong result set: return it as-is (still up to `fetch` exact matches).
	// Broadening would only dilute the top of a query that already matched well.
	if len(primary) >= limit {
		return primary, nil
	}

	lists := []rankedList{{weight: exactWeight, results: primary}}

	stem, err := e.ftsPass(ctx, "nodes_fts_stem", orMatch(base), worktree, kind, lang, fetch)
	if err != nil {
		return nil, err
	}
	lists = append(lists, rankedList{weight: porterWeight, results: stem})

	if syn := withSynonyms(base); len(syn) > len(base) {
		synRes, err := e.ftsPass(ctx, "nodes_fts_stem", orMatch(syn), worktree, kind, lang, fetch)
		if err != nil {
			return nil, err
		}
		lists = append(lists, rankedList{weight: synonymWeight, results: synRes})
	}
	if triTerms := minLen(base, triMinTermLen); len(triTerms) > 0 {
		triRes, err := e.ftsPass(ctx, "nodes_fts_tri", orMatch(triTerms), worktree, kind, lang, fetch)
		if err != nil {
			return nil, err
		}
		lists = append(lists, rankedList{weight: trigramWeight, results: triRes})
	}
	return fuseResults(lists...), nil
}

// ftsPass runs one FTS5 MATCH against the named virtual table (a trusted
// constant — never user input), joining back to nodes/files, scoped to worktree
// and optional kind/language, ordered by bm25 (most relevant first). bm25 is
// negative, so ORDER BY score ASC puts the best match first; the sign is flipped
// on the way out so callers see higher = better.
func (e *Engine) ftsPass(ctx context.Context, table, match, worktree, kind, lang string, limit int) ([]SearchResult, error) {
	args := []any{match, worktree}
	sql := fmt.Sprintf(`
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line,
       bm25(%[1]s) AS score
  FROM %[1]s
  JOIN nodes n ON n.id = %[1]s.node_id
  JOIN files f ON f.id = n.file_id
 WHERE %[1]s MATCH ? AND f.worktree = ?`, table)
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	sql += " ORDER BY score ASC LIMIT ?"
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var bm float64
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line, &bm); err != nil {
			return nil, err
		}
		r.Score = -bm // flip sign so higher = better
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
