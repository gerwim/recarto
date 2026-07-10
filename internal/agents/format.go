package agents

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Session-start tier sizing. sessionStartBudget is the default token budget for
// the injected block (override via RECARTO_SESSION_BUDGET). The map/bug tiers are
// bounded by line caps; cerebrumReserve keeps headroom for them.
const (
	sessionStartBudget = 1500
	largeReadTokens    = 2000 // a read above this is "large enough" to steer toward the graph
	recentBugLimit     = 5
	archModuleLimit    = 8   // caps both the module list and the dependency-edge list of the map
	cerebrumReserve    = 384 // headroom for the map (~165 tok) + recent-bugs (~60 tok) + delta (~150 tok) tiers
	cerebrumBodyMax    = 160

	// deltaTierBudget caps the session-start "what changed" tier. The full list is
	// one get_graph_delta call away, so the tier only buys orientation.
	deltaTierBudget = 150
)

// sessionBudget is the session-start token budget: RECARTO_SESSION_BUDGET if set
// to a positive integer, else the default.
func sessionBudget() int {
	if v := os.Getenv("RECARTO_SESSION_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return sessionStartBudget
}

// largeReadThreshold is the estimated-token bar above which a first whole-file read
// earns a nudge toward the cheaper graph navigation tools: RECARTO_LARGE_READ_TOKENS
// if set to a positive integer, else the default.
func largeReadThreshold() int {
	if v := os.Getenv("RECARTO_LARGE_READ_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return largeReadTokens
}

// formatSessionContext renders the tiered session-start block: pinned do_not_repeat
// (always, full body), cerebrum memories (title + trimmed body, degrading to
// titles-only then dropping under budget pressure), an optional cross-repo
// workspace tier (wsResults, already token-budgeted by their own recall), a
// bounded module map, and a titles-only recent-bugs tier — followed by the
// record nudge.
//
// It also returns the IDs of the memories it actually rendered (full or title-only,
// excluding any dropped under budget pressure). The caller marks those as surfaced
// so the just-in-time read/edit hooks do not re-inject a guardrail/bug/todo already
// sitting in this block — duplicate context is pure token waste. A memory dropped
// here is deliberately left unmarked so it can still surface when its code is touched.
// Workspace memories are never marked: the just-in-time hooks only serve the repo
// store, so there is nothing to dedup against.
func formatSessionContext(results []memory.RecallResult, wsName string, wsResults []memory.RecallResult,
	extraRepos []string, arch graph.Architecture, bugs []*memory.Record,
	deltas []graph.Delta, deltaTotal int, deltaBaselineMs int64, budget int) (string, []string) {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")

	var emitted []string
	markEmitted := func(r memory.RecallResult) {
		if r.Record != nil && r.Record.ID != "" {
			emitted = append(emitted, r.Record.ID)
		}
	}

	budgetCap := budget - cerebrumReserve
	if budgetCap < 0 {
		budgetCap = budget
	}
	used := 0
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		if r.Record.Kind == memory.KindDoNotRepeat && r.Record.Pinned {
			line := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, r.Record.Body)
			b.WriteString(line)
			used += ledger.EstimateTokens(line)
			if c := r.Caveat(); c != "" {
				fmt.Fprintf(&b, "  %s\n", c)
				used += ledger.EstimateTokens(c)
			}
			markEmitted(r)
			continue
		}
		// Gate on the RAW body cost so a large-bodied memory degrades to title-only,
		// but emit (and account) the trimmed one-line display.
		fullDisplay := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, trimOneLine(r.Record.Body))
		fullCost := fmt.Sprintf("- [%s] %s\n  %s\n", r.Record.Kind, title, r.Record.Body)
		titleOnly := fmt.Sprintf("- [%s] %s\n", r.Record.Kind, title)
		switch {
		case used+ledger.EstimateTokens(fullCost) <= budgetCap:
			b.WriteString(fullDisplay)
			used += ledger.EstimateTokens(fullDisplay)
			if c := r.Caveat(); c != "" {
				fmt.Fprintf(&b, "  %s\n", c)
				used += ledger.EstimateTokens(c)
			}
			markEmitted(r)
		case used+ledger.EstimateTokens(titleOnly) <= budgetCap:
			b.WriteString(titleOnly)
			used += ledger.EstimateTokens(titleOnly)
			if c := r.Caveat(); c != "" {
				fmt.Fprintf(&b, "  %s\n", c)
				used += ledger.EstimateTokens(c)
			}
			markEmitted(r)
		}
	}
	if len(results) == 0 {
		b.WriteString("(no prior memories yet)\n")
	}

	// The cross-repo workspace tier: already token-budgeted by its own recall
	// (a slice of the session budget carved out by the caller), so it renders
	// without further cap math — uniform title + trimmed body, like the
	// cerebrum tier's compact form.
	if len(wsResults) > 0 {
		fmt.Fprintf(&b, "\nWorkspace memory (%s, shared across repos):\n", wsName)
		for _, r := range wsResults {
			title := r.Record.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "- [%s] %s\n  %s\n", r.Record.Kind, title, trimOneLine(r.Record.Body))
		}
	}

	// One line, not a tier: the extra repos' memories reach the agent through
	// recall (merged automatically) and the file-anchored read/edit hooks, so
	// session start only needs to say they are served and how to target them.
	if len(extraRepos) > 0 {
		fmt.Fprintf(&b, "\nAlso serving repos: %s — graph/memory tools accept repo:\"<name>\"; recall and file-anchored surfacing cover them automatically.\n",
			strings.Join(extraRepos, ", "))
	}

	if len(arch.Modules) > 0 {
		// Select the highest-signal modules/deps for the map rather than the
		// alphabetically-first ones: rank product modules by file count and the
		// heaviest dependency edges by reference count, demoting test projects.
		mods, deps := graph.MapArchitecture(arch, archModuleLimit)
		b.WriteString("\nCode map (modules):\n")
		for _, m := range mods {
			fmt.Fprintf(&b, "- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for _, d := range deps {
			fmt.Fprintf(&b, "  %s → %s (%d)\n", d.From, d.To, d.Count)
		}
		b.WriteString("Query the code graph (search/snippet/callers) instead of reading whole files; grep/glob auto-injects matching symbols.\n")
	}

	if tier := formatDeltaTier(deltas, deltaTotal, deltaBaselineMs); tier != "" {
		b.WriteString(tier)
	}

	if len(bugs) > 0 {
		b.WriteString("\nRecent bugs (search_bugs for detail):\n")
		for _, r := range bugs {
			t := r.Title
			if t == "" {
				t = "(untitled)"
			}
			fmt.Fprintf(&b, "- %s\n", t)
		}
	}

	b.WriteString("\nRecord new learnings with the record_memory tool and bug fixes with record_bug.")
	return b.String(), emitted
}

// formatBugWarnings renders prior bugs tied to a code location as advisory lines.
func formatBugWarnings(loc string, bugs []*memory.Record) string {
	lines := make([]string, 0, len(bugs))
	for _, r := range bugs {
		if fix, _ := r.Extra["fix"].(string); fix != "" {
			lines = append(lines, fmt.Sprintf("⚠ prior bug in %s: %s — fix: %s", loc, r.Title, fix))
		} else {
			lines = append(lines, fmt.Sprintf("⚠ prior bug in %s: %s", loc, r.Title))
		}
	}
	return strings.Join(lines, "\n")
}

// formatTodoReminders renders open todos anchored to loc as one-line reminders,
// the just-in-time counterpart to formatBugWarnings for the todo kind.
func formatTodoReminders(loc string, todos []*memory.Record) string {
	lines := make([]string, 0, len(todos))
	for _, r := range todos {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		lines = append(lines, fmt.Sprintf("📝 open todo for %s: %s", loc, title))
	}
	return strings.Join(lines, "\n")
}

// formatGuidance renders file/symbol-anchored guardrail memories as advisory lines
// the moment that code is touched: do_not_repeat as a hard "don't" (🚫), decision
// as a settled rationale (📌), and learning as prior knowledge (📚). It is the
// just-in-time counterpart to formatBugWarnings for the high-signal kinds recall
// would otherwise only surface, budget permitting, at session start.
func formatGuidance(loc string, recs []*memory.Record) string {
	lines := make([]string, 0, len(recs))
	for _, r := range recs {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		switch r.Kind {
		case memory.KindDoNotRepeat:
			lines = append(lines, fmt.Sprintf("🚫 do-not-repeat for %s: %s", loc, title))
		case memory.KindLearning:
			lines = append(lines, fmt.Sprintf("📚 prior learning for %s: %s", loc, title))
		default: // decision
			lines = append(lines, fmt.Sprintf("📌 prior decision for %s: %s", loc, title))
		}
	}
	return strings.Join(lines, "\n")
}

// trimOneLine collapses a memory body to its first line, capped at cerebrumBodyMax
// runes, for the budget-conscious cerebrum tier.
func trimOneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > cerebrumBodyMax {
		s = string(r[:cerebrumBodyMax]) + "…"
	}
	return s
}

// nullable maps "" to a SQL NULL so repo-global rows store NULL, not "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// formatDeltaTier renders the budgeted "changed since your last working
// session" block: per-module +added ~changed -removed counts (module = the
// file's directory, so internal/mcp and internal/parse stay distinct entries),
// symbol names for the heaviest module only, and the drill-down pointer. Empty
// input renders nothing — a no-op session must not produce an empty list.
//
// Budget degradation drops names first, then trims module lines (5→3→1), then
// falls back to a header-only pointer. The header date is never stripped.
// Every stage is re-measured so the invariant "output ≤ deltaTierBudget" is
// guaranteed by construction, not by a post-hoc truncation that can corrupt the
// header (the old approach hit the date's " (" before the names list).
func formatDeltaTier(deltas []graph.Delta, total int, baselineMs int64) string {
	if len(deltas) == 0 {
		return ""
	}
	type agg struct {
		added, changed, removed int
		names                   []string
	}
	mods := map[string]*agg{}
	var order []string
	// Use the file's directory as the module key so "internal/mcp" and
	// "internal/parse" are distinct entries rather than collapsing to "internal".
	// For a flat file (no slash) the file name itself is the key.
	dirOf := func(p string) string {
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i]
		}
		return p
	}
	for _, d := range deltas {
		m := dirOf(d.Path)
		a, ok := mods[m]
		if !ok {
			a = &agg{}
			mods[m] = a
			order = append(order, m)
		}
		switch d.Change {
		case "added":
			a.added++
			a.names = append(a.names, leafName(d.QualifiedName))
		case "removed":
			a.removed++
		default:
			a.changed++
			a.names = append(a.names, leafName(d.QualifiedName))
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		ai, aj := mods[order[i]], mods[order[j]]
		return ai.added+ai.changed+ai.removed > aj.added+aj.changed+aj.removed
	})

	const maxModLines = 5
	date := time.UnixMilli(baselineMs).Format("Jan 2")

	// compose builds the output string with the given configuration. withNames
	// controls whether symbol names are appended to the top module's line.
	// modCap limits how many module lines are emitted.
	compose := func(withNames bool, modCap int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "\nChanged since your last working session here (%s):\n", date)
		for i, m := range order {
			if i >= modCap {
				fmt.Fprintf(&b, "- … %d more modules\n", len(order)-modCap)
				break
			}
			a := mods[m]
			line := "- " + m + ":"
			if a.added > 0 {
				line += fmt.Sprintf(" +%d", a.added)
			}
			if a.changed > 0 {
				line += fmt.Sprintf(" ~%d", a.changed)
			}
			if a.removed > 0 {
				line += fmt.Sprintf(" -%d", a.removed)
			}
			if withNames && i == 0 && len(a.names) > 0 {
				n := a.names
				if len(n) > 3 {
					n = n[:3]
				}
				line += " (" + strings.Join(n, ", ") + ", …)"
				if len(a.names) <= 3 {
					line = strings.Replace(line, ", …)", ")", 1)
				}
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "(get_graph_delta lists all %d)\n", total)
		return b.String()
	}

	// Attempt each degradation stage in order. Names are dropped first, then
	// module-line count is trimmed (5→3→1). Every stage is re-measured so the
	// budget invariant is enforced by construction — we never slice into the
	// already-rendered string (which would corrupt the header date).
	out := compose(true, maxModLines)
	for _, fb := range []struct {
		names bool
		lines int
	}{{false, maxModLines}, {false, 3}, {false, 1}} {
		if ledger.EstimateTokens(out) <= deltaTierBudget {
			return out
		}
		out = compose(fb.names, fb.lines)
	}
	if ledger.EstimateTokens(out) > deltaTierBudget {
		// Last resort: orientation header + drill-down pointer only.
		out = fmt.Sprintf("\nChanged since your last working session here (%s): %d symbols.\n(get_graph_delta lists them)\n",
			date, total)
	}
	return out
}

// leafName returns the last dotted segment of a (possibly qualified) symbol.
// "A.B.Create" → "Create".
func leafName(sym string) string {
	if i := strings.LastIndex(sym, "."); i >= 0 && i < len(sym)-1 {
		return sym[i+1:]
	}
	return sym
}
