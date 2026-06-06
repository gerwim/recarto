package graph

import (
	"context"
	"fmt"
	"strings"
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

// Candidate-symbol fusion weights. When the LLM supplies candidate_symbols (the
// identifier names it expects), an exact name/qualified-name match is the highest
// possible signal — the model's guess landing precisely — so it outranks every
// lexical pass; an identifier-split match on the guesses ranks just above plain
// natural-language lexical hits.
const (
	candidateExactWeight = 2.0
	candidateSplitWeight = 1.2
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
	Signature     string
	Line          int
	Score         float64 // higher = more relevant
}

// Search returns symbols matching query, scoped to worktree and optionally
// filtered by kind and language. It is SearchGuided with no candidate symbols:
// purely lexical (an exact pass plus stemmed/synonym/trigram broadeners on
// underfill), with the dormant vector tier fused in only when an embedder is set.
func (e *Engine) Search(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	return e.SearchGuided(ctx, worktree, query, nil, kind, lang, limit)
}

// SearchGuided is Search with optional candidate_symbols: the identifier names an
// LLM expects to exist for the concept in query. They drive two high-priority
// lexical passes — exact name/qualified-name equality, then identifier-split FTS —
// fused ABOVE the natural-language passes, so a concept query the agent annotated
// with the right names surfaces those symbols even when the prose shares no tokens
// with them. With no candidates the behaviour is identical to the previous Search.
func (e *Engine) SearchGuided(ctx context.Context, worktree, query string, candidateSymbols []string, kind, lang string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	lex, err := e.lexicalSearch(ctx, worktree, query, kind, lang, limit)
	if err != nil {
		return nil, err
	}
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)

	// Candidate symbols are a stronger, agent-supplied signal than the dormant
	// vector tier, so when they are present they replace it: the candidate path
	// fuses its own passes with the lexical tier and returns, intentionally
	// skipping the embedder branch below.
	if len(candidateSymbols) > 0 {
		return e.mergeCandidates(ctx, worktree, candidateSymbols, lex, kind, lang, limit)
	}

	if e.emb == nil {
		return dedupBySymbol(demoteGenerated(lex, nil), limit), nil
	}
	// Dormant semantic tier (set only under -tags ORT). Best-effort.
	vec, verr := e.vectorPass(ctx, worktree, query, kind, lang, limit*3)
	if verr != nil {
		vec = nil
	}
	return mergeHybrid(lex, vec, limit), nil
}

// mergeCandidates fuses the candidate exact-name and identifier-split passes above
// the natural-language lexical list, then dedups by name. The exact pass leads
// (its hits are the agent's guesses landing precisely), the split pass ranks above
// plain lexical, and lexical backfills the remaining page.
func (e *Engine) mergeCandidates(ctx context.Context, worktree string, candidates []string, lex []SearchResult, kind, lang string, limit int) ([]SearchResult, error) {
	fetch := limit * 3
	exact, err := e.candidateExactPass(ctx, worktree, candidates, kind, lang, fetch)
	if err != nil {
		return nil, err
	}
	var split []SearchResult
	if terms := baseTerms(strings.Join(candidates, " ")); len(terms) > 0 {
		split, err = e.ftsPass(ctx, "nodes_fts", orMatch(terms), worktree, kind, lang, fetch)
		if err != nil {
			return nil, err
		}
	}
	fused := fuseResults(
		rankedList{weight: candidateExactWeight, results: exact},
		rankedList{weight: candidateSplitWeight, results: split},
		rankedList{weight: exactWeight, results: lex},
	)
	// Sink generated-file matches below hand-written ones, but never demote a
	// symbol whose name is one of the agent's exact candidate guesses — that
	// precise landing is the strongest signal we have, generated or not.
	protect := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		protect[strings.ToLower(strings.TrimSpace(c))] = true
	}
	return dedupBySymbol(demoteGenerated(fused, protect), limit), nil
}

// candidateExactPass returns symbols whose name OR qualified_name equals one of
// the candidate identifiers (case-insensitive), scoped to worktree and optional
// kind/language. These are the agent's guesses landing exactly, so the caller
// fuses them at the top weight. Returns nil for an empty/blank candidate set.
//
// Matching is whole-string equality. The name arm is the primary path; the
// qualified_name arm is a rare-hit safety net, since real qualified names are
// dotted (App.AddressLookup) while candidates are usually bare (AddressLookup) —
// the identifier-split FTS pass in mergeCandidates does the actual fuzzy bridging.
func (e *Engine) candidateExactPass(ctx context.Context, worktree string, candidates []string, kind, lang string, limit int) ([]SearchResult, error) {
	seen := map[string]bool{}
	var lowered []any
	var ph []string
	for _, c := range candidates {
		lc := strings.ToLower(strings.TrimSpace(c))
		if lc == "" || seen[lc] {
			continue
		}
		seen[lc] = true
		lowered = append(lowered, lc)
		ph = append(ph, "?")
	}
	if len(lowered) == 0 {
		return nil, nil
	}
	inList := strings.Join(ph, ",")
	args := []any{worktree}
	args = append(args, lowered...) // lower(n.name) IN (...)
	args = append(args, lowered...) // lower(n.qualified_name) IN (...)
	sql := `
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line, n.signature
  FROM nodes n
  JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?
   AND (lower(n.name) IN (` + inList + `) OR lower(n.qualified_name) IN (` + inList + `))`
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	// Every row here is an exact name/qualified-name hit, so there is no intrinsic
	// relevance order among them (unlike the bm25-ranked FTS passes). Order
	// shortest-name-first as a tie-break — a shorter exact match is the tightest
	// reading of the agent's guess — then by name for determinism.
	sql += " ORDER BY length(n.name), n.name LIMIT ?"
	args = append(args, limit)

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line, &r.Signature); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// mergeHybrid fuses the lexical and vector tiers with a reserved tail. Lexical
// fills the non-reserved majority first, so its precise hits keep the top
// positions (hybrid never regresses a keyword query); the vector tier then fills
// the reserved tail with concept matches the lexical pass missed; finally any
// reserve the vector tier left unused is backfilled with more lexical. Dedup is by
// symbol identity throughout (see symbolKey) so the same node reappearing across
// tiers folds while distinct same-named symbols survive. With no vector candidates
// this degrades to a plain lexical dedup.
func mergeHybrid(lex, vec []SearchResult, limit int) []SearchResult {
	lex = demoteGenerated(lex, nil)
	vec = demoteGenerated(vec, nil)
	if len(vec) == 0 {
		return dedupBySymbol(lex, limit)
	}
	lexCap := limit - limit*vecReserveNum/vecReserveDen
	seen := make(map[string]bool, limit)
	out := make([]SearchResult, 0, limit)
	push := func(rs []SearchResult, cap int) {
		for _, r := range rs {
			if len(out) >= cap {
				return
			}
			k := symbolKey(r)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	push(lex, lexCap) // lexical majority + all top positions
	push(vec, limit)  // vector fills the reserved tail
	push(lex, limit)  // backfill if the vector tier underfilled its reserve
	return out
}

// symbolKey identifies a result by the symbol it points at, not by its bare name.
// The qualified name (Namespace.Type.Method) distinguishes the genuinely different
// symbols that share a short name — App.Alpha.Process vs App.Beta.Process, or the
// 298 distinct ExecuteAsync methods across a large codebase — so a "find all
// definitions of X" lookup surfaces every one instead of collapsing to a single
// entry. The path component keeps distinct top-level symbols apart when the
// language gives no qualifier (qualified name == bare name); overloads within one
// type (same qualified name, same file) still fold to one navigable target.
func symbolKey(r SearchResult) string {
	if r.QualifiedName != "" && r.QualifiedName != r.Name {
		return "q\x00" + strings.ToLower(r.QualifiedName)
	}
	return "n\x00" + strings.ToLower(r.Name) + "\x00" + r.Path
}

// dedupBySymbol collapses results that point at the same symbol (see symbolKey),
// keeping the first (best-ranked) occurrence, and truncates to limit. This folds
// the same node reappearing across FTS passes and true overloads within a type,
// while preserving the distinct same-named symbols that a bare-name dedup would
// have hidden.
func dedupBySymbol(in []SearchResult, limit int) []SearchResult {
	seen := make(map[string]bool, limit)
	out := make([]SearchResult, 0, limit)
	for _, r := range in {
		if len(out) >= limit {
			break
		}
		k := symbolKey(r)
		if seen[k] {
			continue
		}
		seen[k] = true
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
       n.signature,
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
			&r.Language, &r.Path, &r.Line, &r.Signature, &bm); err != nil {
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
