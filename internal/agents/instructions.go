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
- **Anchor** each memory to the files/symbols it concerns (the ` + "`record_memory`" + ` refs):
  an anchored bug, do-not-repeat or decision resurfaces automatically the moment you
  read or edit that code — just-in-time, costing no tokens until it is relevant, and
  shown only once per session. Guidance with no refs only appears in the budgeted
  session-start tier, so anchor anything you want to fire at the point of change.
- **Recall** with ` + "`recall_memory`" + ` and ` + "`search_bugs`" + ` before changing code.
- **Track open work** as a ` + "`todo`" + ` memory (` + "`record_memory`" + ` with kind ` + "`todo`" + `): it
  resurfaces at session start and when you touch its referenced files. Close it with
  ` + "`complete_todo`" + ` (or ` + "`recarto todo done <id>`" + `) when the work is finished.
- **Capture preferences** as they surface: the moment the user corrects your approach,
  states a style / workflow / naming choice, or rejects a suggestion, record a
  ` + "`preference`" + ` memory with what they preferred instead. These cues are conversational —
  not anchored to a file edit — so they are the easiest to miss; the bar is low.
- Query the code graph with ` + "`search_graph` / `get_code_snippet` / `trace_call_path`" + `
  instead of reading whole files. When searching ` + "`search_graph`" + ` by concept, also
  pass ` + "`candidate_symbols`" + ` — the identifier names you expect (e.g. ` + "`AddressLookup`, `Geocode`" + `);
  the names you supply are what make concept search land. Results are ranked so the
  implementation leads: hand-written product code outranks tests, generated code,
  and namespace/package declarations, so the top hits are usually what you want —
  ` + "`get_code_snippet`" + ` the first relevant one rather than re-running a shell grep.
  Every list result (search/query/outline/impls/annotated) identifies a symbol by
  its ` + "`qualified_name`" + ` — the precise, overload-safe form the symbol-taking tools
  accept; its last segment is the short name, so the redundant ` + "`name`" + ` field is
  omitted when derivable. Read ` + "`qualified_name`" + ` and feed it straight back to
  ` + "`get_code_snippet`/`trace_call_path`/`find_*`" + `; don't expect a separate ` + "`name`" + `.
- **Read one symbol, not the file**: ` + "`get_code_snippet`" + ` takes just a ` + "`symbol`" + `
  (the ` + "`file`" + ` is optional) and resolves it by name across the repo — the usual
  next step after a search returns a ` + "`qualified_name`" + `, so you needn't know the path. It accepts a
  bare name, a fully-qualified name, or a ` + "`Type.Method`" + ` suffix (e.g.
  ` + "`DefaultRequestExecutor.ExecuteAsync`" + `) — the exact identifier form
  ` + "`trace_call_path`/`callers`/`callees`" + ` print, so a name you copied from a trace
  resolves without reformatting. If the bare name is ambiguous it returns
  ` + "`candidates`" + ` (path/line/qualified_name); re-query with a qualified name (usually
  unique) or add the file. Overloads of one method (same qualified name, same file)
  come back together in a single snippet — no disambiguation needed.
- **Orient first** in unfamiliar code with ` + "`get_architecture`" + ` (module/file/symbol
  map), then ` + "`search_graph`" + ` (ranked concept/identifier search) or ` + "`query_graph`" + `
  (exact structured filters — kind/name/path/language/edge, at least one required;
  it won't dump the whole graph) to locate symbols and ` + "`get_code_snippet`" + ` to read
  one instead of a whole file. A lookup miss is an empty result, not an error.
- **Outline a file** with ` + "`get_file_outline`" + ` to see its symbols (kinds, names,
  signatures, line ranges) in source order instead of reading the whole file; then
  ` + "`get_code_snippet`" + ` the one you need. Prefer this over opening a large file to
  find your way around it. If you do read a large indexed file whole, Recarto emits a
  one-time nudge with its symbol count — treat that whole-file read as the expensive
  path and switch to ` + "`get_file_outline`/`get_code_snippet`" + ` for the rest of that file.
- **Grep without leaving the graph** with ` + "`search_content`" + ` for arbitrary text
  (string literals, error messages, config keys, TODOs). It searches the *whole*
  worktree honoring ` + "`.gitignore`" + ` — not just parsed code but config, build, docs
  and data files the graph never indexes (` + "`.csproj`, `.json`, `.yaml`, `.md`" + `, …) —
  so it is a complete shell-` + "`grep`" + ` replacement; you never have to shell out for a
  file kind the graph happens not to cover. Hits in indexed code carry their
  enclosing symbol; hits elsewhere read as file scope. The result reports
  ` + "`total`/`truncated`" + ` so you know whether the page is complete. Prefer it over a
  shell ` + "`grep`" + `; raise ` + "`limit`" + ` /
  ` + "`max_per_file`" + ` (or narrow with ` + "`path_glob`/`language`" + `) when it says it truncated.
  It does full regex too: set ` + "`regex=true`" + ` (` + "`ignore_case`" + ` for case-folding). For
  *references* to a type/symbol, use ` + "`find_references`" + ` instead of a hand-rolled
  regex — it is the structural answer with a text sweep built in.
- **Trace** with ` + "`trace_call_path`" + `: every chain reads in the calls direction —
  entry *n* calls entry *n+1* (` + "`A → B`" + ` always means A calls B), in both ` + "`callers`" + `
  and ` + "`callees`" + ` mode. So callees put your symbol first and its callees after it;
  callers put the outer caller first and your symbol last. The ` + "`legend`" + ` field
  restates this on every result. Each hop is annotated with a confidence —
  trust high/typed/self hops; treat low/name hops as candidates to verify with
  ` + "`get_code_snippet`" + `. Tracing a *type* (class/interface/record) dead-ends —
  calls target its methods, not its declaration — so it returns a ` + "`hint`" + `: use
  ` + "`find_implementations`" + ` for implementers and ` + "`find_references`" + ` for references.
  A resolved symbol with no edges returns an honest dead-end (a root-only chain plus
  a ` + "`hint`" + `), never an empty result — so "no callers" means it has none (an
  entrypoint, or dynamically-bound call sites), not that the symbol is unknown.
- **Find implementations** of an interface or base type with ` + "`find_implementations`" + `
  (direction ` + "`down`" + ` for subtypes/implementers, ` + "`up`" + ` for supertypes) instead of
  grepping for ` + "`: TypeName`" + ` — implementers don't share the interface's name, so
  ` + "`search_graph`" + ` can't surface them.
- **Find references** to a symbol with ` + "`find_references`" + ` — call sites, type
  usages, imports and annotations from the graph's edges, plus a word-boundary
  text sweep for what the graph cannot see, deduplicated by path:line. Each hit
  carries its referencing symbol, an edge kind (or ` + "`text`" + ` for grep-only hits),
  and ` + "`is_test`" + `. Prefer it over a ` + "`\\bName\\b`" + ` regex in ` + "`search_content`" + `.
- **Find annotated symbols** (classes/methods carrying an attribute or decorator)
  with ` + "`find_annotated`" + ` — e.g. every ` + "`[ExtendObjectType]`" + ` class or ` + "`@Component`" + `.
  Prefer it over grepping for ` + "`[Attr`" + `: the decorated symbol shares no name with
  the attribute, and a text grep also matches the attribute's own definition,
  doc comments, and string literals.
- **Report Recarto's own faults**: if a Recarto tool errors, ranks the wrong
  result first, confuses you, or lacks a capability you needed, call ` + "`report_recarto_issue`" + `
  (signal_type: error|quality|friction|gap). It routes to a global cross-repo queue
  the maintainer triages — describe the Recarto interaction and a minimal repro, not
  this repo's source.
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
