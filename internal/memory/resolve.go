package memory

import (
	"context"
	"fmt"
	"strings"
)

// RefResolver reports which of a memory's code refs point at code NOT present in
// a worktree. The graph-backed implementation lives in internal/app and queries
// the code graph (files/nodes) in the shared store; tests inject a fake. A nil
// resolver, or one whose worktree has no graph rows, disables the check so recall
// behaves exactly as before (graceful degradation, spec §5.5).
type RefResolver interface {
	// Dangling returns the subset of refs whose referenced code is absent from
	// worktree. Implementations MUST return nil (nothing dangles) when worktree
	// has no graph rows. tag refs are not code and are never returned.
	Dangling(ctx context.Context, worktree string, refs []Ref) ([]Ref, error)
}

// SetRefResolver installs the resolvability backend (called once during wiring).
func (e *Engine) SetRefResolver(r RefResolver) { e.res = r }

// Dangling returns r's code refs that are absent from worktree: nil when no
// resolver is set, worktree is "" or r has no file/symbol refs.
func (e *Engine) Dangling(ctx context.Context, worktree string, r *Record) ([]Ref, error) {
	code := codeRefsOf(r.Refs)
	if e.res == nil || worktree == "" || len(code) == 0 {
		return nil, nil
	}
	return e.res.Dangling(ctx, worktree, code)
}

// codeRefsOf returns only file/symbol refs (tags are not code).
func codeRefsOf(refs []Ref) []Ref {
	var out []Ref
	for _, r := range refs {
		if r.Type == RefFile || r.Type == RefSymbol {
			out = append(out, r)
		}
	}
	return out
}

// Caveat renders the dangling-reference warning for a recalled memory, or "" when
// nothing dangles.
func (r RecallResult) Caveat() string {
	if len(r.DanglingRefs) == 0 {
		return ""
	}
	vals := make([]string, 0, len(r.DanglingRefs))
	for _, d := range r.DanglingRefs {
		vals = append(vals, d.Value)
	}
	branch := "unknown"
	if r.Record != nil && r.Record.Branch != "" {
		branch = r.Record.Branch
	}
	return fmt.Sprintf("⚠ references code not present in this worktree (branch %s): %s — may not apply to the current branch",
		branch, strings.Join(vals, ", "))
}
