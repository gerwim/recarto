// Package parse turns source files into a normalized symbol/edge graph using
// tree-sitter. It is the ONLY package that links the tree-sitter C library
// (CGO); every C-allocating object (Parser/Tree/Query/QueryCursor) is closed
// here behind ParseFile, so no raw C object escapes the package.
package parse

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tscs "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Registry holds the per-language grammars and their queries, each compiled on
// first use. Build once per process; safe for concurrent ParseFile calls (each
// creates its own Parser).
type Registry struct {
	langs map[string]*sitter.Language

	mu      sync.Mutex
	queries map[string]*sitter.Query
}

// NewRegistry registers every language. It compiles no query: compiling them all
// costs ~0.25 s of CPU, which every hook and CLI process paid at start-up even
// when it parsed nothing. A malformed query surfaces on that language's first
// ParseFile (and in the package tests).
func NewRegistry() (*Registry, error) {
	return &Registry{
		langs: map[string]*sitter.Language{
			"go":         sitter.NewLanguage(tsgo.Language()),
			"csharp":     sitter.NewLanguage(tscs.Language()),
			"typescript": sitter.NewLanguage(tsts.LanguageTypescript()),
			"tsx":        sitter.NewLanguage(tsts.LanguageTSX()),
			"javascript": sitter.NewLanguage(tsjs.Language()),
			"rust":       sitter.NewLanguage(tsrust.Language()),
			"python":     sitter.NewLanguage(tspy.Language()),
		},
		queries: map[string]*sitter.Query{},
	}, nil
}

// query returns lang's compiled query, compiling it on first use.
func (r *Registry) query(lang string) (*sitter.Query, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q, ok := r.queries[lang]; ok {
		return q, nil
	}
	q, qErr := sitter.NewQuery(r.langs[lang], querySources()[lang])
	if qErr != nil {
		return nil, fmt.Errorf("parse: compile %s query: %v", lang, qErr)
	}
	r.queries[lang] = q
	return q, nil
}

// Close releases the compiled queries (must-Close discipline).
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for lang, q := range r.queries {
		q.Close()
		delete(r.queries, lang)
	}
}

// LangForExt returns the language id for a path's extension, or ok=false.
func LangForExt(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go", true
	case ".cs":
		return "csharp", true
	case ".ts":
		return "typescript", true
	case ".tsx":
		return "tsx", true
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript", true
	case ".rs":
		return "rust", true
	case ".py": // not .pyi: stubs repeat the .py definitions, making every lookup ambiguous
		return "python", true
	}
	return "", false
}
