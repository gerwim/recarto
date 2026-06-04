package graph

import "sort"

// rrfK is the Reciprocal Rank Fusion constant. A document's fused score is the
// weighted sum over each ranked list of weight/(rrfK + rank). The standard value
// 60 damps the influence of any single list's exact positions while still
// rewarding items several lists rank highly.
const rrfK = 60

// rankedList is one input to fusion: an already best-first list of results and
// the weight its rankings carry. Weighting (not plain RRF) is deliberate — the
// trigram substring pass matches very common short fragments ("day", "time") in
// many symbols, so at equal weight it would dilute precise stemmed matches. A
// low trigram weight lets it ADD tail/substring hits and break ties without ever
// outranking a strong primary match.
type rankedList struct {
	weight  float64
	results []SearchResult
}

// fuseResults merges several weighted ranked lists into one order using weighted
// Reciprocal Rank Fusion. Results are keyed by NodeID; the first-seen
// SearchResult carries the row fields, and Score is overwritten with the fused
// score (higher = better). The merge is stable: among equal fused scores,
// earlier-contributed items keep their relative order.
func fuseResults(lists ...rankedList) []SearchResult {
	score := map[string]float64{}
	first := map[string]SearchResult{}
	var order []string
	for _, list := range lists {
		for rank, r := range list.results {
			if _, ok := score[r.NodeID]; !ok {
				first[r.NodeID] = r
				order = append(order, r.NodeID)
			}
			score[r.NodeID] += list.weight / float64(rrfK+rank+1)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return score[order[i]] > score[order[j]]
	})
	out := make([]SearchResult, len(order))
	for i, id := range order {
		r := first[id]
		r.Score = score[id]
		out[i] = r
	}
	return out
}
