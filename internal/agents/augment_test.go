package agents

import "testing"

func TestAugmentSearchTerm(t *testing.T) {
	cases := []struct {
		name     string
		pattern  string
		wantTerm string
		wantOK   bool
	}{
		// Plain identifiers / Grep regexes pass through unchanged: no file-glob
		// shape, so the augment symbol search must behave exactly as before.
		{"plain identifier", "AddProjection", "AddProjection", true},
		{"regex with star", "Add.*Projection", "Add.*Projection", true},
		{"trailing quantifier", "foo*", "foo*", true},
		{"path literal no wildcard", "config/app.json", "config/app.json", true},

		// Pure-extension / stemless file globs carry no symbol signal: skip.
		{"bare extension glob", "*.cs", "", false},
		{"json extension glob", "*.json", "", false},
		{"recursive extension glob", "**/*.ts", "", false},
		{"path extension glob", "src/**/*.go", "", false},
		{"recursive dir glob", "internal/**", "", false},

		// File globs with a meaningful basename stem reduce to that stem.
		{"stemmed basename glob", "**/*Projection*.cs", "Projection", true},
		{"prefix glob", "Address*.cs", "Address", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term, ok := augmentSearchTerm(tc.pattern)
			if ok != tc.wantOK || term != tc.wantTerm {
				t.Fatalf("augmentSearchTerm(%q) = (%q, %v), want (%q, %v)",
					tc.pattern, term, ok, tc.wantTerm, tc.wantOK)
			}
		})
	}
}
