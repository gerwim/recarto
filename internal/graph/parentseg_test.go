package graph

import "testing"

func TestParentSegment(t *testing.T) {
	cases := map[string]string{
		"A.B.C.method": "C",
		"Type.method":  "Type",
		"method":       "",
		"":             "",
		"Ns.Type":      "Ns",
	}
	for qn, want := range cases {
		if got := parentSegment(qn); got != want {
			t.Errorf("parentSegment(%q) = %q, want %q", qn, got, want)
		}
	}
}
