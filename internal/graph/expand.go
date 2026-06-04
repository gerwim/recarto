package graph

import (
	"bufio"
	_ "embed"
	"strings"
	"unicode"
)

//go:embed synonyms.txt
var graphSynonymsData string

// graphSynonyms maps each token to its curated expansions (bidirectional). Kept
// local to the graph layer so it takes no dependency on internal/memory.
var graphSynonyms = parseGraphSynonyms(graphSynonymsData)

func parseGraphSynonyms(data string) map[string][]string {
	m := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		a := strings.ToLower(strings.TrimSpace(parts[0]))
		b := strings.ToLower(strings.TrimSpace(parts[1]))
		if a == "" || b == "" {
			continue
		}
		m[a] = appendUniqueStr(m[a], b)
		m[b] = appendUniqueStr(m[b], a)
	}
	return m
}

func appendUniqueStr(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// queryStopwords are natural-language function words that carry no signal for a
// symbol search. A natural-language query ("how long to drive between two
// locations") is mostly stopwords; dropping them keeps the FTS MATCH focused on
// the content terms so bm25 is not diluted by ubiquitous tokens.
var queryStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true,
	"into": true, "for": true, "and": true, "or": true, "on": true, "at": true,
	"by": true, "with": true, "is": true, "are": true, "be": true, "as": true,
	"how": true, "whether": true, "may": true, "can": true, "do": true, "does": true,
	"that": true, "this": true, "from": true, "between": true, "two": true,
	"it": true, "its": true, "their": true, "which": true, "what": true,
	"when": true, "where": true, "who": true, "a's": true, "per": true,
}

// baseTerms turns a free-text query into the deduped lowercase term set used for
// the PRIMARY FTS MATCH: it identifier-splits each word (camelCase + separators)
// and drops stopwords, but does NOT add synonyms. Splitting mirrors how symbol
// FTS content is stored (ftsText), so "AddressLookup" and "address lookup" yield
// the same terms. Synonyms are deliberately left out here so the primary pass
// stays precise; they are added only as a fallback when the primary pass
// underfills (see Search and withSynonyms).
func baseTerms(q string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		lw := strings.ToLower(strings.TrimSpace(w))
		if lw == "" || seen[lw] || queryStopwords[lw] {
			return
		}
		seen[lw] = true
		out = append(out, lw)
	}
	fields := strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || r == '_' || r == '-' || r == '.' || r == '/' || r == ','
	})
	for _, f := range fields {
		add(f)
		for _, c := range splitCamel(f) {
			add(c)
		}
	}
	return out
}

// withSynonyms returns terms plus their curated synonym expansions, deduped and
// order-preserving (originals first). Used to broaden a query that the precise
// primary pass could not satisfy.
func withSynonyms(terms []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, t := range terms {
		add(t)
	}
	for _, t := range terms {
		for _, syn := range graphSynonyms[t] {
			add(syn)
		}
	}
	return out
}

// expandQueryTerms is baseTerms followed by synonym expansion — the fully
// broadened term set. Retained as a single entry point for callers/tests that
// want the maximal expansion.
func expandQueryTerms(q string) []string {
	return withSynonyms(baseTerms(q))
}

// orMatch builds a safe FTS5 MATCH expression: each term double-quoted (so
// identifiers with punctuation or FTS operators cannot break the query) and
// ORed. OR (not AND) is deliberate — a natural-language or multi-word query
// rarely matches every term against one symbol, so AND returned "(no matches)";
// with OR, partial matches surface and bm25 ranks the fullest match first. An
// empty term set yields a query that matches nothing.
func orMatch(terms []string) string {
	if len(terms) == 0 {
		return `""`
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}

// minLen returns the terms whose length is at least n. The FTS5 trigram
// tokenizer rejects a MATCH whose tokens are shorter than 3 characters, so the
// trigram pass uses only terms long enough to form a trigram.
func minLen(terms []string, n int) []string {
	var out []string
	for _, t := range terms {
		if len(t) >= n {
			out = append(out, t)
		}
	}
	return out
}
