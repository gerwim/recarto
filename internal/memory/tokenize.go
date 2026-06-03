package memory

import (
	"strings"
	"unicode"
)

// splitCamel splits camelCase / PascalCase and acronym boundaries
// ("HTTPServer" → "HTTP","Server"; "getUser" → "get","User").
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

// splitIdentifier breaks one token on separators, then on camel/acronym
// boundaries. It returns only the component words (not the original token).
func splitIdentifier(tok string) []string {
	fields := strings.FieldsFunc(tok, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r)
	})
	var out []string
	for _, f := range fields {
		out = append(out, splitCamel(f)...)
	}
	return out
}

// tokenize lowercases text and returns a deduplicated token list containing
// both the original tokens and their identifier-split components.
func tokenize(text string) []string {
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			r != '_' && r != '-' && r != '.' && r != '/'
	})
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, tok := range raw {
		add(tok)
		for _, part := range splitIdentifier(tok) {
			add(part)
		}
	}
	return out
}
