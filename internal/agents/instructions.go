package agents

import (
	"os"
	"path/filepath"
	"strings"
)

// Delimiters bounding recarto's managed block inside an instructions file, so a
// re-run replaces only that block and preserves the user's other content.
const (
	recartoBlockBegin = "<!-- recarto:begin -->"
	recartoBlockEnd   = "<!-- recarto:end -->"
)

// Instructions is the canonical guidance written to each host's instructions file
// (AGENTS.md at repo scope; the host's global instructions file at user scope).
const Instructions = `# Recarto

This repo uses **Recarto** for cross-session memory (preferences, learnings,
decisions, do-not-repeats, bugs) and a tree-sitter code graph, stored in a shared
per-repo SQLite cache outside the working tree.

- At session start Recarto injects the most relevant memories for this branch.
- **Record** knowledge with ` + "`record_memory`" + ` and bug fixes with ` + "`record_bug`" + `.
- **Recall** with ` + "`recall_memory`" + ` and ` + "`search_bugs`" + ` before changing code.
- **Track open work** as a ` + "`todo`" + ` memory (` + "`record_memory`" + ` with kind ` + "`todo`" + `): it
  resurfaces at session start and when you touch its referenced files. Close it with
  ` + "`complete_todo`" + ` (or ` + "`recarto todo done <id>`" + `) when the work is finished.
- Query the code graph with ` + "`search_graph` / `get_code_snippet` / `trace_call_path`" + `
  instead of reading whole files. When searching ` + "`search_graph`" + ` by concept, also
  pass ` + "`candidate_symbols`" + ` — the identifier names you expect (e.g. ` + "`AddressLookup`, `Geocode`" + `);
  the names you supply are what make concept search land.
- **Orient first** in unfamiliar code with ` + "`get_architecture`" + ` (module/file/symbol
  map), then ` + "`search_graph` / `query_graph`" + ` to locate symbols and
  ` + "`get_code_snippet`" + ` to read one instead of a whole file. A lookup miss is an
  empty result, not an error.
- **Trace** with ` + "`trace_call_path`" + `: each hop is annotated with a confidence —
  trust high/typed/self hops; treat low/name hops as candidates to verify with
  ` + "`get_code_snippet`" + `.
- **Inspect & curate** memories with ` + "`list_memory` / `get_memory`" + `; delete one
  recorded in error with ` + "`forget_memory`" + ` (or supersede it via ` + "`supersedes_id`" + `).

Recarto reinforces near-duplicate memories instead of duplicating them, so record
freely — repeated learnings converge to one entry.`

// WriteInstructionsBlock inserts or replaces the recarto-delimited block in the
// file at path (creating it and its parents if absent), preserving other content.
// It is idempotent: re-running rewrites only the block.
func WriteInstructionsBlock(path string) error {
	block := recartoBlockBegin + "\n" + Instructions + "\n" + recartoBlockEnd + "\n"

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(existing)

	if i := strings.Index(s, recartoBlockBegin); i >= 0 {
		if j := strings.Index(s, recartoBlockEnd); j > i {
			s = s[:i] + strings.TrimRight(block, "\n") + s[j+len(recartoBlockEnd):]
			return writeFile(path, s)
		}
	}
	// Append, separating from any prior content by a blank line.
	if len(s) > 0 {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += "\n"
	}
	return writeFile(path, s+block)
}

// writeFile writes content to path, creating parent directories.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
