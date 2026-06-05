package agents

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Session-start tier sizing. sessionStartBudget is the default token budget for
// the injected block (override via RECARTO_SESSION_BUDGET). The map/bug tiers are
// bounded by line caps; cerebrumReserve keeps headroom for them.
const (
	sessionStartBudget = 1500
	recentBugLimit     = 5
	archModuleLimit    = 8
	archDepLimit       = 8
	cerebrumReserve    = 256 // headroom for the map (~165 tok) + recent-bugs (~60 tok) tiers
	cerebrumBodyMax    = 160
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

// formatSessionContext renders the tiered session-start block: pinned do_not_repeat
// (always, full body), cerebrum memories (title + trimmed body, degrading to
// titles-only then dropping under budget pressure), a bounded module map, and a
// titles-only recent-bugs tier — followed by the record nudge.
func formatSessionContext(results []memory.RecallResult, arch graph.Architecture, bugs []*memory.Record, budget int) string {
	var b strings.Builder
	b.WriteString("Recarto memory for this repo/branch:\n")

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
			}
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
			}
		case used+ledger.EstimateTokens(titleOnly) <= budgetCap:
			b.WriteString(titleOnly)
			used += ledger.EstimateTokens(titleOnly)
			if c := r.Caveat(); c != "" {
				fmt.Fprintf(&b, "  %s\n", c)
			}
		}
	}
	if len(results) == 0 {
		b.WriteString("(no prior memories yet)\n")
	}

	if len(arch.Modules) > 0 {
		b.WriteString("\nCode map (modules):\n")
		for i, m := range arch.Modules {
			if i >= archModuleLimit {
				break
			}
			fmt.Fprintf(&b, "- %s: %d files, %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for i, d := range arch.Deps {
			if i >= archDepLimit {
				break
			}
			fmt.Fprintf(&b, "  %s → %s (%d)\n", d.From, d.To, d.Count)
		}
		b.WriteString("Query the code graph (search/snippet/callers) instead of reading whole files; grep/glob auto-injects matching symbols.\n")
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
	return b.String()
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
