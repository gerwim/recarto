// Package cli implements Recarto's command-line surface over internal/app, using
// the stdlib flag package (not cobra — see the Phase 1C decision log). Each
// command is a function taking an *app.App and an io.Writer so it is unit-testable
// without spawning a process or touching real git.
package cli

import (
	"errors"
	"flag"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// parseFlags parses fs and reports whether the command should exit cleanly. It
// treats -h/--help specially: flag has already printed usage to the FlagSet's
// output and returns flag.ErrHelp, which is not a real error — the command should
// return nil without doing further work. Genuine parse errors are returned as-is.
func parseFlags(fs *flag.FlagSet, args []string) (helped bool, err error) {
	err = fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return true, nil
	}
	return false, err
}

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
// /--tags flag values. File refs are normalized to repo-relative form (via
// worktree) so they match hook-reported absolute paths, consistent with bug add
// and the MCP record paths.
func refsFromFlags(worktree, files, symbols, tags string) []memory.Ref {
	var refs []memory.Ref
	for _, f := range splitCSV(files) {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(worktree, f)})
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
