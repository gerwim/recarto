package ledger

import "math"

// charsPerToken is the heuristic ratio for estimating model tokens from
// character length (~3.8 ≈ OpenWolf parity, good to ~15%).
const charsPerToken = 3.8

// EstimateTokens approximates the number of model tokens in s. It is a pure
// function so callers can budget recall output without a tokenizer dependency.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := float64(len(s)) / charsPerToken
	if n < 1 {
		return 1
	}
	return int(n + 0.5)
}

// EstimateTokensCeil is EstimateTokens rounded up: the costs of parts add up to
// at least the estimate of their concatenation, so a sum of costs that stays
// within a budget keeps the whole text within it.
func EstimateTokensCeil(s string) int {
	if s == "" {
		return 0
	}
	return int(math.Ceil(float64(len(s)) / charsPerToken))
}
