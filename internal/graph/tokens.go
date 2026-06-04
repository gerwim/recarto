package graph

import (
	"strings"
	"unicode"
)

// splitCamel splits camelCase / PascalCase and acronym boundaries
// ("HTTPServer" → "HTTP","Server"; "getUser" → "get","User"). It mirrors the
// memory engine's identifier splitter (internal/memory/tokenize.go) and is kept
// here intentionally so the code-graph layer carries no dependency on the
// memory layer.
func splitCamel(s string) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) &&
			i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			boundary = true // end of an acronym run: HTTPServer → HTTP|Server
		case unicode.IsLetter(prev) != unicode.IsLetter(cur):
			boundary = true // letter↔digit boundary
		}
		if boundary {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	return append(words, string(runes[start:]))
}

// ftsText builds the FTS5 content for one symbol field: the original token plus
// its identifier-split components (split on separators, then on camel/acronym
// boundaries), de-duplicated. Storing the components lets a query for a single
// component word ("Renderer") match a PascalCase symbol ("MessageRenderer"),
// which the default unicode61 tokenizer cannot do on its own. The original token
// is kept first so an exact-name query still matches (and ranks well).
func ftsText(s string) string {
	seen := map[string]bool{}
	var parts []string
	add := func(w string) {
		lw := strings.ToLower(strings.TrimSpace(w))
		if lw == "" || seen[lw] {
			return
		}
		seen[lw] = true
		parts = append(parts, w)
	}
	add(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r)
	})
	for _, f := range fields {
		add(f)
		for _, c := range splitCamel(f) {
			add(c)
		}
	}
	return strings.Join(parts, " ")
}
