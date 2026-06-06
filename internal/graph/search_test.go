package graph

import (
	"context"
	"testing"
)

func TestSearchByCamelComponent(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Modern .NET identifiers are PascalCase with no separators; an agent
	// searching a conceptual component word ("Renderer", "Backup") must still
	// find the symbol, not only its exact full name.
	src := []byte("namespace App;\nclass ActivityMessageRenderer {}\nclass BackupService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ query, want string }{
		{"Renderer", "ActivityMessageRenderer"},
		{"Message", "ActivityMessageRenderer"},
		{"Backup", "BackupService"},
		{"ActivityMessageRenderer", "ActivityMessageRenderer"}, // exact still works
	}
	for _, c := range cases {
		got, err := e.Search(ctx, "/wt", c.query, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range got {
			if r.Name == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("Search(%q) did not find %q; got %d results %v",
				c.query, c.want, len(got), resultNames(got))
		}
	}
}

func TestSearchStemsMorphology(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A plural/inflected query term must still find the singular-rooted symbol.
	// The default unicode61 tokenizer treats "coordinates" and "coordinate" as
	// distinct tokens (no match); porter stemming reduces both to a common root.
	src := []byte("namespace App;\nclass CoordinateConverter {}\nclass BookingRepository {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"coordinates", "converting", "bookings"} {
		got, err := e.Search(ctx, "/wt", q, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("Search(%q) found nothing; stemming should match the root", q)
		}
	}
}

func TestSearchSynonymFallback(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// "authn" shares no token with "AuthenticationService"; the precise pass finds
	// nothing, so the synonym broadener (authn -> authentication) must recover it.
	src := []byte("namespace App;\nclass AuthenticationService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "authn", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range got {
		if r.Name == "AuthenticationService" {
			found = true
		}
	}
	if !found {
		t.Errorf("synonym fallback authn->authentication missed AuthenticationService; got %v",
			resultNames(got))
	}
}

func TestSearchTrigramSubstring(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A mid-identifier fragment that is neither a camelCase component nor a stem
	// prefix should still locate the symbol via the trigram substring index.
	src := []byte("namespace App;\nclass AuthenticationMiddleware {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "thentica", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range got {
		if r.Name == "AuthenticationMiddleware" {
			found = true
		}
	}
	if !found {
		t.Errorf("trigram substring search for %q missed AuthenticationMiddleware; got %v",
			"thentica", resultNames(got))
	}
}

func TestSearchReturnsDistinctSameNameDefinitions(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A method name shared across distinct types (interface impls / per-class
	// handlers) is the single most common "find all definitions of X" lookup. Each
	// is a genuinely different symbol (distinct qualified name), so collapsing them
	// to one bare-name entry hides the answer and forces a grep fallback. Only true
	// duplicates — the SAME symbol, or overloads within one type — should collapse.
	src := []byte("namespace App;\n" +
		"class Alpha { public void Process() {} }\n" +
		"class Beta { public void Process() {} }\n" +
		"class Gamma { public void Process() {} }\n")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Plain search for the exact method name must surface all three distinct
	// definitions, not just one.
	got, err := e.Search(ctx, "/wt", "Process", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	qn := map[string]bool{}
	for _, r := range got {
		qn[r.QualifiedName] = true
	}
	for _, want := range []string{"App.Alpha.Process", "App.Beta.Process", "App.Gamma.Process"} {
		if !qn[want] {
			t.Errorf("Search(%q) missed distinct definition %q; got %v", "Process", want, qualNames(got))
		}
	}

	// The candidate-symbol path (agent says "I expect a symbol named Process")
	// must likewise return every distinct definition.
	guided, err := e.SearchGuided(ctx, "/wt", "process records", []string{"Process"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	gqn := map[string]bool{}
	for _, r := range guided {
		gqn[r.QualifiedName] = true
	}
	if len(gqn) < 3 {
		t.Errorf("SearchGuided candidate path collapsed distinct definitions: got %v", qualNames(guided))
	}
}

func qualNames(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.QualifiedName
	}
	return out
}

func resultNames(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

func TestSearchGuidedCandidateBoost(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// The concept query shares NO identifier token with the target name; only the
	// LLM-supplied candidate bridges them.
	src := []byte("namespace App;\nclass PdokAddressLookupService {}\nclass InvoiceService {}\nclass ReportService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Split-token candidate: "AddressLookup" → address/lookup → PdokAddressLookupService.
	got, err := e.SearchGuided(ctx, "/wt", "convert a street into coordinates",
		[]string{"AddressLookup"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "PdokAddressLookupService" {
		t.Fatalf("split candidate should boost PdokAddressLookupService to top, got %v", resultNames(got))
	}

	// Exact-name candidate: lands the precise symbol even with an unrelated query.
	got2, err := e.SearchGuided(ctx, "/wt", "totally unrelated prose",
		[]string{"InvoiceService"}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) == 0 || got2[0].Name != "InvoiceService" {
		t.Fatalf("exact candidate should rank InvoiceService first, got %v", resultNames(got2))
	}
}

func TestSearchGuidedNilCandidatesMatchesSearch(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace App;\nclass AlternativeSlotFinder {}\nclass SlotRepository {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	base, err := e.Search(ctx, "/wt", "alternative slot", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	guided, err := e.SearchGuided(ctx, "/wt", "alternative slot", nil, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	bn, gn := resultNames(base), resultNames(guided)
	if len(bn) != len(gn) {
		t.Fatalf("nil-candidate SearchGuided diverged from Search: %v vs %v", bn, gn)
	}
	for i := range bn {
		if bn[i] != gn[i] {
			t.Fatalf("nil-candidate SearchGuided diverged at %d: %v vs %v", i, bn, gn)
		}
	}
}

func TestSearchByName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Authenticate(){}\nfunc helper(){}\ntype Session struct{}")
	if err := e.IndexFile(ctx, "/wt", "auth.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "Authenticate", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Authenticate" {
		t.Fatalf("Search = %+v, want one result Authenticate", got)
	}
	if got[0].Path != "auth.go" || got[0].Kind != "function" {
		t.Errorf("result = %+v, want path auth.go kind function", got[0])
	}
	only, err := e.Search(ctx, "/wt", "Session", "type", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Kind != "type" {
		t.Errorf("kind-filtered search = %+v", only)
	}
	none, _ := e.Search(ctx, "/other", "Authenticate", "", "", 10)
	if len(none) != 0 {
		t.Errorf("cross-worktree search leaked %d results", len(none))
	}
}

func TestSearchOrReturnsPartialMatches(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// No single symbol contains BOTH query words: AND-matching returns nothing,
	// OR-matching surfaces each partial match (ranked).
	src := []byte("namespace App;\nclass AlternativeSlotFinder {}\nclass TravelTimeService {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "alternative travel", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, n := range resultNames(got) {
		set[n] = true
	}
	if !set["AlternativeSlotFinder"] || !set["TravelTimeService"] {
		t.Fatalf("OR search should surface both partial matches, got %v", resultNames(got))
	}
}

func TestSearchRanksMoreCompleteMatchHigher(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Both symbols match "slot"; only one also matches "alternative". The fuller
	// match must rank first under bm25 even though the query is now OR-matched.
	src := []byte("namespace App;\nclass AlternativeSlotFinder {}\nclass SlotRepository {}")
	if err := e.IndexFile(ctx, "/wt", "App.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	got, err := e.Search(ctx, "/wt", "alternative slot", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Name != "AlternativeSlotFinder" {
		t.Fatalf("fuller match should rank first, got %v", resultNames(got))
	}
}
