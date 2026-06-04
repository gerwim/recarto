package graph

import (
	"slices"
	"strings"
	"testing"
)

func TestExpandQueryTermsSplitsAndDropsStopwords(t *testing.T) {
	got := expandQueryTerms("how long to driveBetween two Locations")
	// camelCase split + lowercased content terms survive.
	for _, w := range []string{"drive", "locations", "long"} {
		if !slices.Contains(got, w) {
			t.Errorf("expected term %q in %v", w, got)
		}
	}
	// stopwords are dropped (including when they appear as a camelCase component).
	for _, w := range []string{"how", "to", "two", "between"} {
		if slices.Contains(got, w) {
			t.Errorf("stopword %q should have been dropped from %v", w, got)
		}
	}
}

func TestExpandQueryTermsSynonymBridge(t *testing.T) {
	got := expandQueryTerms("auth")
	if !slices.Contains(got, "auth") || !slices.Contains(got, "authentication") {
		t.Errorf("auth should expand to include authentication, got %v", got)
	}
}

func TestOrMatchQuotesAndJoins(t *testing.T) {
	if got := orMatch(nil); got != `""` {
		t.Errorf("empty orMatch = %q, want \"\"", got)
	}
	got := orMatch([]string{"travel", "time"})
	if got != `"travel" OR "time"` {
		t.Errorf("orMatch = %q", got)
	}
	// embedded quotes are escaped, not left to break the MATCH expression.
	if got := orMatch([]string{`a"b`}); !strings.Contains(got, `""`) {
		t.Errorf("orMatch did not escape quote: %q", got)
	}
}

func TestFuseResultsRewardsMultiListAgreement(t *testing.T) {
	// "b" is ranked 2nd in both lists; "a" and "c" are 1st in one list only.
	// RRF should rank the agreed-on "b" first.
	porter := []SearchResult{{NodeID: "a"}, {NodeID: "b"}}
	tri := []SearchResult{{NodeID: "c"}, {NodeID: "b"}}
	out := fuseResults(
		rankedList{weight: 1.0, results: porter},
		rankedList{weight: 1.0, results: tri},
	)
	if len(out) != 3 || out[0].NodeID != "b" {
		t.Fatalf("RRF should rank the cross-list agreement first, got %v", idsOf(out))
	}
}

func idsOf(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.NodeID
	}
	return out
}
