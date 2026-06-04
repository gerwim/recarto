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

func resultNames(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
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
