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
