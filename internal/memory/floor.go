package memory

import (
	"context"
	"math"
	"strings"
	"unicode"

	"git.gerwim.nl/gerwim/recarto/internal/embedding"
)

// These thresholds are calibrated for embedding.ModelID; recalibrate when the
// embedding model or tokenizer changes.
const (
	floorGateCos = 0.55
	floorGateCov = 0.25
	floorItemCos = 0.35
	floorItemCov = 0.15
)

func ftsTerms(s string) []string {
	s = embedding.Fold(s)
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			terms = append(terms, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func queryTerms(text string) []string {
	parts := tokenize(text)
	return uniqueStrings(ftsTerms(strings.Join(parts, " ")))
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (e *Engine) applyFloor(ctx context.Context, terms, candidates []string, kind Kind, sem *semanticHits) (kept []string, open bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	weights, err := e.idfWeights(ctx, terms)
	if err != nil {
		return candidates, true
	}
	placeholders := make([]string, len(candidates))
	args := make([]any, len(candidates))
	for i, id := range candidates {
		placeholders[i], args[i] = "?", id
	}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id, kind, search_text FROM memories WHERE status='active' AND id IN (`+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return candidates, true
	}
	type candidate struct {
		id         string
		searchText string
	}
	var eligible []candidate
	for rows.Next() {
		var id string
		var candidateKind Kind
		var searchText string
		if err := rows.Scan(&id, &candidateKind, &searchText); err != nil {
			rows.Close()
			return candidates, true
		}
		if kind == "" || candidateKind == kind {
			eligible = append(eligible, candidate{id: id, searchText: searchText})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return candidates, true
	}
	rows.Close()

	covs := make(map[string]float64, len(eligible))
	for _, c := range eligible {
		cov := coverage(terms, ftsTerms(c.searchText), weights)
		covs[c.id] = cov
		cos := -1.0
		if sem != nil {
			if value, ok := sem.cos[c.id]; ok {
				cos = value
			}
		}
		if cos >= floorGateCos || cov >= floorGateCov {
			open = true
		}
	}
	if !open {
		return nil, false
	}
	for _, id := range candidates {
		cov, ok := covs[id]
		if !ok {
			continue
		}
		cos := -1.0
		if sem != nil {
			if value, ok := sem.cos[id]; ok {
				cos = value
			}
		}
		if cos >= floorItemCos || cov >= floorItemCov {
			kept = append(kept, id)
		}
	}
	return kept, true
}

func (e *Engine) idfWeights(ctx context.Context, terms []string) (map[string]float64, error) {
	var n int
	if err := e.st.DB.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE status='active'`).Scan(&n); err != nil {
		return nil, err
	}
	df := make(map[string]int, len(terms))
	for start := 0; start < len(terms); start += 500 {
		end := min(start+500, len(terms))
		chunk := terms[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, term := range chunk {
			placeholders[i], args[i] = "?", term
		}
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT term, doc FROM memories_fts_vocab WHERE term IN (`+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var term string
			var count int
			if err := rows.Scan(&term, &count); err != nil {
				rows.Close()
				return nil, err
			}
			df[term] = count
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	weights := make(map[string]float64, len(terms))
	for _, term := range terms {
		weights[term] = math.Log(1 + (float64(n-df[term])+0.5)/(float64(df[term])+0.5))
	}
	return weights, nil
}

func coverage(terms, docTerms []string, weights map[string]float64) float64 {
	if len(terms) == 0 {
		return 0
	}
	present := make(map[string]bool, len(docTerms))
	for _, term := range docTerms {
		present[term] = true
	}
	var total, matched float64
	for _, term := range terms {
		weight := weights[term]
		total += weight
		if present[term] {
			matched += weight
		}
	}
	if total == 0 {
		return 0
	}
	return matched / total
}
