// Package cli implements Recarto's command-line surface over internal/app, using
// the stdlib flag package (not cobra — see the Phase 1C decision log). Each
// command is a function taking an *app.App and an io.Writer so it is unit-testable
// without spawning a process or touching real git.
package cli

import (
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// splitCSV splits a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// refsFromFlags builds structural refs from the comma-separated --files/--symbols
// /--tags flag values.
func refsFromFlags(files, symbols, tags string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range splitCSV(files) {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: f})
	}
	for _, s := range splitCSV(symbols) {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	for _, tg := range splitCSV(tags) {
		refs = append(refs, memory.Ref{Type: memory.RefTag, Value: tg})
	}
	return refs
}

// branchFor returns the branch to tag a new memory with: the current branch, or
// "" (repo-global → stored as NULL) when --global is set.
func branchFor(a *app.App, global bool) string {
	if global {
		return ""
	}
	return a.Git.Branch
}
