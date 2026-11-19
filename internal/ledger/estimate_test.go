package ledger

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},           // < 1 token rounds up to 1
		{"hello world", 3}, // 11 chars / 3.8 ≈ 2.9 → 3
		{"abcdefghij", 3},  // 10 / 3.8 ≈ 2.6 → 3
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEstimateTokensCeilBoundsTheWhole(t *testing.T) {
	whole := "- [learning, recarto] a title\n  a body of some length\n  caveat\n"
	for _, cut := range [][]int{{1}, {7, 20}, {3, 9, 30, 41}} {
		sum, from := 0, 0
		for _, c := range append(cut, len(whole)) {
			sum += EstimateTokensCeil(whole[from:c])
			from = c
		}
		if sum < EstimateTokens(whole) {
			t.Errorf("cut at %v: parts cost %d, whole %d", cut, sum, EstimateTokens(whole))
		}
	}
}
