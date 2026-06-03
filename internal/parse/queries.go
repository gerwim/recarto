package parse

import _ "embed"

//go:embed queries/go.scm
var goQuery string

//go:embed queries/csharp.scm
var csharpQuery string

//go:embed queries/typescript.scm
var typescriptQuery string

//go:embed queries/javascript.scm
var javascriptQuery string

// querySources maps a language id to its compiled-query source. tsx reuses the
// TypeScript query (the tsx grammar shares the same definition/reference nodes).
func querySources() map[string]string {
	return map[string]string{
		"go": goQuery, "csharp": csharpQuery,
		"typescript": typescriptQuery, "tsx": typescriptQuery,
		"javascript": javascriptQuery,
	}
}
