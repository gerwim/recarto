package memory

import (
	"strings"
	"testing"
)

func hasAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	got := tokenize("getUserConfig")
	if !hasAll(got, "getuserconfig", "get", "user", "config") {
		t.Errorf("camelCase split missing parts: %v", got)
	}

	got = tokenize("auth_token-refresh.go")
	if !hasAll(got, "auth", "token", "refresh", "go") {
		t.Errorf("separator split missing parts: %v", got)
	}

	got = tokenize("HTTPServer")
	if !hasAll(got, "http", "server") {
		t.Errorf("acronym split missing parts: %v", got)
	}

	// Deduped + lowercased.
	got = tokenize("Foo foo FOO")
	if strings.Count(strings.Join(got, " "), "foo") != 1 {
		t.Errorf("expected deduped lowercase tokens, got %v", got)
	}
}
