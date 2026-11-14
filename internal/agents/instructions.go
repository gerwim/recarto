package agents

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gerwim/recarto/internal/features"
)

// Delimiters bounding recarto's managed block inside an instructions file, so a
// re-run replaces only that block and preserves the user's other content.
const (
	recartoBlockBegin = "<!-- recarto:begin -->"
	recartoBlockEnd   = "<!-- recarto:end -->"
)

// The features each part of the instructions needs on.
var (
	needsMemory = []*features.Feature{features.MemoryTools}
	needsTodos  = []*features.Feature{features.MemoryTools, features.Todos}
	needsGraph  = []*features.Feature{features.GraphTools}
	needsIssues = []*features.Feature{features.IssueReports}
)

// instructionParts is the guidance written to each host's instructions file
// (AGENTS.md at repo scope; the host's global instructions file at user scope),
// in order. A part is written only when every feature it needs is on: those
// are the features that register the MCP tools it names (mcp.NewServer).
var instructionParts = []struct {
	needs []*features.Feature
	text  string
}{
	{nil, "# Recarto\n\nThis repo uses **Recarto**: memory that persists across sessions (preferences, learnings, decisions, do-not-repeats, bugs)"},
	{needsGraph, " and a code graph"},
	{nil, "."},
	{needsMemory, "\n\n- **Record** learnings with `record_memory` and bug fixes with `record_bug`. Near-duplicates merge, so record freely." +
		"\n- **Anchor** each memory to the `files` or `symbols` it concerns: an anchored bug, do-not-repeat, decision or learning comes back when that code is read or edited. Without anchors, a memory only returns at session start or through recall." +
		"\n- **Record a `preference`** the moment the user corrects your approach, rejects a suggestion or states a style, workflow or naming choice. The bar is low."},
	{needsTodos, "\n- **Track open work** as a `todo` memory anchored to its files, and close it with `complete_todo` when it is done."},
	{needsMemory, "\n- **Before changing code**, call `recall_memory` with the task in plain words, and `search_bugs`." +
		"\n- `list_memory`: list stored memories by kind or branch." +
		"\n- `get_memory`: read one memory by id." +
		"\n- `forget_memory`: delete a memory recorded in error."},
	{needsGraph, "\n\nUse the code graph instead of reading whole files or grepping:" +
		"\n- `search_graph`: find symbols by name or concept; pass `candidate_symbols`, the names you expect." +
		"\n- `get_code_snippet`: read the source of one symbol, or several at once, instead of the whole file." +
		"\n- `get_file_outline`: a file's symbols and line ranges, before you open a large file." +
		"\n- `search_content`: text or regex over the whole worktree, config and docs included, instead of grep." +
		"\n- `find_references`: every use of a symbol, instead of grepping its name." +
		"\n- `trace_call_path`: who calls a function, or what it calls." +
		"\n- `find_implementations`: the types that implement or extend a type, which grep can't find." +
		"\n- `find_annotated`: the symbols carrying an attribute, annotation or decorator." +
		"\n- `query_graph`: exact lookups by kind, name, path, language or edge." +
		"\n- `get_architecture`: the modules and their dependencies, to orient in unfamiliar code." +
		"\n- `get_graph_delta`: what changed in the code since your last session here, before you trust what you recall about it."},
	{needsIssues, "\n\nWhen a Recarto tool fails, misleads you or lacks something you need, report it with `report_recarto_issue`."},
}

// Instructions returns the guidance for the features in s, naming only the
// MCP tools s leaves registered. A nil s has every feature on.
func Instructions(s features.Set) string {
	var b strings.Builder
	for _, p := range instructionParts {
		on := true
		for _, f := range p.needs {
			on = on && s.On(f)
		}
		if on {
			b.WriteString(p.text)
		}
	}
	return b.String()
}

// WriteInstructionsBlock inserts or replaces the recarto-delimited block in the
// file at path with the guidance for the features in s (creating the file and
// its parents if absent), preserving other content. Re-running rewrites only
// the block.
func WriteInstructionsBlock(path string, s features.Set) error {
	block := recartoBlockBegin + "\n" + Instructions(s) + "\n" + recartoBlockEnd + "\n"

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	src := string(existing)

	if i := strings.Index(src, recartoBlockBegin); i >= 0 {
		if j := strings.Index(src, recartoBlockEnd); j > i {
			src = src[:i] + strings.TrimRight(block, "\n") + src[j+len(recartoBlockEnd):]
			return writeFile(path, src)
		}
	}
	// Append, separating from any prior content by a blank line.
	if len(src) > 0 {
		if !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		src += "\n"
	}
	return writeFile(path, src+block)
}

// writeFile writes content to path, creating parent directories.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
