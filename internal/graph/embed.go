package graph

import (
	"context"
	"strings"
)

// Embedder turns text into fixed-dimension unit-comparable vectors. It is the
// seam for the optional semantic search tier: an implementation is injected with
// SetEmbedder (see the build-tagged embed_hugot.go for the hugot/ONNX one), and
// an Engine without one stays purely lexical.
//
// Documents and queries are embedded through separate methods because
// retrieval-tuned models are asymmetric: EmbeddingGemma, for instance, requires
// a "title: none | text: …" prefix on documents and a "task: search result |
// query: …" prefix on queries. Vectors must be comparable across the two methods
// (same space, same Dim) and should be L2-normalized so the vec0 index's L2
// distance ranks like cosine similarity.
type Embedder interface {
	EmbedDocs(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// SetEmbedder enables the semantic tier: indexed symbols are embedded and stored
// as vectors, and Search fuses a vector-similarity pass with the lexical passes.
// Passing nil disables it. Call before indexing so vectors are written.
func (e *Engine) SetEmbedder(emb Embedder) { e.emb = emb }

// HasEmbedder reports whether a semantic tier is active.
func (e *Engine) HasEmbedder() bool { return e.emb != nil }

// embeddableKinds are the symbol kinds worth embedding for semantic search: the
// named, searchable declarations an agent reasons about. Members like properties,
// fields, enum members and constructors are excluded — they balloon the vector
// index (the bulk of symbols) and add distractors without being meaningful
// semantic search targets, which only degrades recall@k and index time.
var embeddableKinds = map[string]bool{
	"class": true, "interface": true, "struct": true, "record": true,
	"enum": true, "type": true, "namespace": true,
	"function": true, "method": true, "delegate": true,
}

// symbolDoc builds the text embedded for a symbol: its kind plus the
// identifier-split name and qualified name and the path words (domain context
// such as "appointments" or "routing"). Identifier splitting (ftsText) turns
// "IAddressLookupService" into "address lookup service", which a text embedding
// model reads far better than the raw PascalCase token.
func symbolDoc(kind, name, qualifiedName, path string) string {
	pathWords := strings.Map(func(r rune) rune {
		switch r {
		case '/', '-', '_', '.':
			return ' '
		}
		return r
	}, path)
	return strings.Join([]string{kind, ftsText(name), ftsText(qualifiedName), pathWords}, " ")
}
