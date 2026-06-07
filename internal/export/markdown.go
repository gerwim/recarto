// Package export renders stored memories to markdown on demand. SQLite is the
// single source of truth (spec D2); markdown is a render-only view with no
// persistent file, so it can never drift.
package export

import (
	"fmt"
	"strings"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// kindSections lists the kinds in display order with their headings. Pinned
// do_not_repeat comes first; the rest follow most-to-least correctness-critical.
var kindSections = []struct {
	kind    memory.Kind
	heading string
}{
	{memory.KindDoNotRepeat, "Do Not Repeat"},
	{memory.KindDecision, "Decisions"},
	{memory.KindPreference, "Preferences"},
	{memory.KindLearning, "Learnings"},
	{memory.KindBug, "Bugs"},
}

// Render returns a markdown document for recs, grouped by kind in display order.
// It is pure (no I/O, no clock) and preserves the input order within each kind,
// so callers control sorting (see Engine.List) and the output is deterministic.
func Render(repoName string, recs []*memory.Record) string {
	byKind := map[memory.Kind][]*memory.Record{}
	for _, r := range recs {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Recarto Memory — %s\n\n", repoName)
	fmt.Fprintf(&b, "_%d active memories_\n", len(recs))

	for _, sec := range kindSections {
		group := byKind[sec.kind]
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", sec.heading)
		for _, r := range group {
			title := r.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- **%s**\n", title)
			if r.Body != "" {
				// Indent continuation lines too, so a multi-line body stays inside
				// the markdown list item instead of breaking out of it.
				fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(r.Body, "\n", "\n  "))
			}
			fmt.Fprintf(&b, "  _updated %s · reinforced ×%d · scope %s · refs: %s_\n",
				time.UnixMilli(r.UpdatedAt).UTC().Format("2006-01-02"),
				r.ReinforcementCount, scope(r.Branch), refList(r.Refs))
		}
	}
	return b.String()
}

func scope(branch string) string {
	if branch == "" {
		return "global"
	}
	return branch
}

// refList joins ref values for the meta line. Only the value is rendered; the
// ref Type (file/symbol/tag) is intentionally omitted for brevity.
func refList(refs []memory.Ref) string {
	if len(refs) == 0 {
		return "none"
	}
	vs := make([]string, len(refs))
	for i, r := range refs {
		vs[i] = r.Value
	}
	return strings.Join(vs, ", ")
}
