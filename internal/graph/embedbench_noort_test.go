//go:build !ORT

package graph

import "testing"

// benchEmbedder returns no embedder in the default (lexical-only) build, so the
// search-quality benchmark measures the lexical tier. Build with `-tags ORT` and
// set RECARTO_BENCH_EMBED_MODEL to measure the hybrid (semantic) tier.
func benchEmbedder(t *testing.T) Embedder { return nil }
