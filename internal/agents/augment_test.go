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
		// A Grep regex ending in an escaped-dot extension (the idiomatic way to
		// match a literal dot — "import.*\.proto", "foo.*\.go") must NOT be mistaken
		// for a file glob: the backslash before the extension dot is a regex tell, so
		// the pattern passes through to symbol search unchanged.
		{"regex escaped extension", "foo.*\\.go", "foo.*\\.go", true},
		{"regex escaped ext with prefix", "import.*\\.proto", "import.*\\.proto", true},

		// Pure-extension / stemless file globs carry no symbol signal: skip.
		{"bare extension glob", "*.cs", "", false},
		{"json extension glob", "*.json", "", false},
		{"recursive extension glob", "**/*.ts", "", false},
		{"path extension glob", "src/**/*.go", "", false},
		{"recursive dir glob", "internal/**", "", false},

		// File globs with a meaningful basename stem reduce to that stem.
		{"stemmed basename glob", "**/*Projection*.cs", "Projection", true},
		{"prefix glob", "Address*.cs", "Address", true},
		{"infix wildcard glob", "*Foo*.ts", "Foo", true},
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
