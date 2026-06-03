package memory

import (
	"bufio"
	_ "embed"
	"strings"
)

//go:embed synonyms.txt
var synonymsData string

// synonymMap maps each token to its curated expansions (loaded bidirectionally).
var synonymMap = parseSynonyms(synonymsData)

func parseSynonyms(data string) map[string][]string {
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
		m[a] = appendUnique(m[a], b)
		m[b] = appendUnique(m[b], a)
	}
	return m
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// expandSynonyms returns tokens plus their curated synonym expansions, deduped.
func expandSynonyms(tokens []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, t := range tokens {
		add(t)
		for _, syn := range synonymMap[t] {
			add(syn)
		}
	}
	return out
}

// BuildSearchText derives the FTS search_text for a memory: identifier-split,
// synonym-expanded tokens from title + body + ref values, space-joined.
func BuildSearchText(title, body string, refs []Ref) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteByte(' ')
	b.WriteString(body)
	for _, r := range refs {
		b.WriteByte(' ')
		b.WriteString(r.Value)
	}
	return strings.Join(expandSynonyms(tokenize(b.String())), " ")
}
