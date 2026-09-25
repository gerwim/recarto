package embedding

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func (m *Model) tokenIDs(text string) []int32 {
	if m == nil {
		return nil
	}
	m.load()
	if m.failed {
		return nil
	}
	// ponytail: literal [CLS]/[MASK] text uses ordinary punctuation + WordPiece
	// tokenization rather than HF's special-token matcher; keep this gap until a
	// real memory corpus demonstrates that special-token literals matter.
	words := preTokens(normalize(text))
	var ids []int32
	for _, word := range words {
		if len(word) == 0 || utf8.RuneCountInString(word) > 100 {
			continue
		}
		start, initial, firstID := 0, true, len(ids)
		for start < len(word) {
			found := false
			for end := len(word); end > start; end-- {
				piece := word[start:end]
				if initial {
					if id, ok := m.initial[piece]; ok {
						ids = append(ids, id)
						start = end
						found = true
						initial = false
						break
					}
				} else if id, ok := m.continuation[piece]; ok {
					ids = append(ids, id)
					start = end
					found = true
					break
				}
			}
			if !found {
				ids = ids[:firstID]
				break
			}
		}
	}
	return ids
}

func normalize(s string) string {
	var clean strings.Builder
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			clean.WriteByte(' ')
			continue
		}
		if r == 0 || r == '\uFFFD' || unicode.In(r, unicode.C) {
			continue
		}
		if unicode.IsSpace(r) {
			r = ' '
		}
		if isCJK(r) {
			clean.WriteByte(' ')
			clean.WriteRune(r)
			clean.WriteByte(' ')
		} else {
			clean.WriteRune(r)
		}
	}
	decomposed := norm.NFD.String(clean.String())
	var folded strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			folded.WriteRune(unicode.ToLower(r))
		}
	}
	return folded.String()
}

// Fold matches FTS5 unicode61's lowercase/diacritic-folded term behavior.
func Fold(s string) string {
	decomposed := norm.NFD.String(s)
	var folded strings.Builder
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			folded.WriteRune(unicode.ToLower(r))
		}
	}
	return folded.String()
}

func preTokens(s string) []string {
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			words = append(words, s[start:end])
			start = -1
		}
	}
	for i, r := range s {
		if r == ' ' {
			flush(i)
			continue
		}
		if isPunctuation(r) {
			flush(i)
			words = append(words, s[i:i+utf8.RuneLen(r)])
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(s))
	return words
}

func isPunctuation(r rune) bool {
	return r >= 33 && r <= 47 || r >= 58 && r <= 64 || r >= 91 && r <= 96 || r >= 123 && r <= 126 || unicode.IsPunct(r)
}

func isCJK(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF ||
		r >= 0x20000 && r <= 0x2A6DF || r >= 0x2A700 && r <= 0x2B73F ||
		r >= 0x2B740 && r <= 0x2B81F || r >= 0x2B820 && r <= 0x2CEAF ||
		r >= 0xF900 && r <= 0xFAFF || r >= 0x2F800 && r <= 0x2FA1F
}
