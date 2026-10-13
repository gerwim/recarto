// Package parse turns source files into a normalized symbol/edge graph using
// tree-sitter. It is the ONLY package that links the tree-sitter C library
// (CGO); every C-allocating object (Parser/Tree/Query/QueryCursor) is closed
// here behind ParseFile, so no raw C object escapes the package.
package parse

import (
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tscs "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Registry holds the per-language grammars and pre-compiled queries. Build once
// per process; safe for concurrent ParseFile calls (each creates its own Parser).
type Registry struct {
	langs   map[string]*sitter.Language
	queries map[string]*sitter.Query
}

// NewRegistry compiles every language's query. A malformed query is a programmer
// error surfaced at startup.
func NewRegistry() (*Registry, error) {
	langs := map[string]*sitter.Language{
		"go":         sitter.NewLanguage(tsgo.Language()),
		"csharp":     sitter.NewLanguage(tscs.Language()),
		"typescript": sitter.NewLanguage(tsts.LanguageTypescript()),
		"tsx":        sitter.NewLanguage(tsts.LanguageTSX()),
		"javascript": sitter.NewLanguage(tsjs.Language()),
		"rust":       sitter.NewLanguage(tsrust.Language()),
	}
	queries := map[string]*sitter.Query{}
	for lang, src := range querySources() {
		q, qErr := sitter.NewQuery(langs[lang], src)
		if qErr != nil {
			// Close any already-compiled queries before bailing (must-Close).
			for _, already := range queries {
				already.Close()
			}
			return nil, fmt.Errorf("parse: compile %s query: %v", lang, qErr)
		}
		queries[lang] = q
	}
	return &Registry{langs: langs, queries: queries}, nil
}

// Close releases the compiled queries (must-Close discipline).
func (r *Registry) Close() {
	for _, q := range r.queries {
		q.Close()
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
	}
	return "", false
}
