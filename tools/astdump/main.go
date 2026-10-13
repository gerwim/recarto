// Command astdump prints the tree-sitter parse tree of a source file, one node
// per line with byte-accurate field labels. It exists to author and debug the
// .scm extraction queries in internal/parse/queries: run it on a snippet of the
// target language to learn the exact node and field names the grammar emits.
//
//	go run ./tools/astdump file.cs
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tscs "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

func langFor(path string) *sitter.Language {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cs":
		return sitter.NewLanguage(tscs.Language())
	case ".go":
		return sitter.NewLanguage(tsgo.Language())
	case ".ts":
		return sitter.NewLanguage(tsts.LanguageTypescript())
	case ".tsx":
		return sitter.NewLanguage(tsts.LanguageTSX())
	case ".js", ".jsx", ".mjs", ".cjs":
		return sitter.NewLanguage(tsjs.Language())
	case ".rs":
		return sitter.NewLanguage(tsrust.Language())
	}
	return nil
}

func dump(n *sitter.Node, src []byte, field string, depth int) {
	if !n.IsNamed() {
		return
	}
	label := n.Kind()
	if field != "" {
		label = field + ": " + label
	}
	text := n.Utf8Text(src)
	if nl := strings.IndexByte(text, '\n'); nl >= 0 {
		text = text[:nl] + "…"
	}
	if len(text) > 60 {
		text = text[:60] + "…"
	}
	fmt.Printf("%s%s  %q  [%d-%d]\n", strings.Repeat("  ", depth), label, text, n.StartPosition().Row+1, n.EndPosition().Row+1)
	for i := uint(0); i < n.ChildCount(); i++ {
		dump(n.Child(i), src, n.FieldNameForChild(uint32(i)), depth+1)
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: astdump <file>")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lang := langFor(os.Args[1])
	if lang == nil {
		fmt.Fprintf(os.Stderr, "astdump: no grammar for %s\n", os.Args[1])
		os.Exit(1)
	}
	p := sitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(lang); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tree := p.Parse(src, nil)
	defer tree.Close()
	dump(tree.RootNode(), src, "", 0)
}
