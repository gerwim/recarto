package ledger

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
