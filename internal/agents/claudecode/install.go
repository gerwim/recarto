package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
)

// Result summarizes what Install changed, for human-readable CLI output.
type Result struct {
	MCPStatus          string // "configured" | "already present"
	HooksStatus        string // "installed N" | "already present"
	InstructionsStatus string // "written" | "already present"
}

// hookSpec is one (event, matcher, command) hook the installer registers.
type hookSpec struct{ event, matcher, command string }

// hookSpecs are the advisory hooks. SessionStart and Stop run unconditionally
// (no matcher); post-read, post-edit, and augment are scoped to their tools.
func hookSpecs(binary string) []hookSpec {
	return []hookSpec{
		{"SessionStart", "", binary + " hook session-start"},
		{"PostToolUse", "Read", binary + " hook post-read"},
		{"PostToolUse", "Edit|Write", binary + " hook post-edit"},
		{"PostToolUse", "Grep|Glob", binary + " hook augment"},
		{"Stop", "", binary + " hook stop"},
	}
}

// instructions is the RECARTO.md snippet describing the record/recall discipline.
const instructions = `# Recarto

This repo uses **Recarto** for cross-session memory (preferences, learnings,
decisions, do-not-repeats, bugs) stored in a shared per-repo SQLite cache outside
the working tree.

- At session start, Recarto injects the most relevant memories for this branch.
- **Record** new knowledge with the ` + "`record_memory`" + ` tool (or ` + "`recarto remember`" + `):
  preferences, learnings, decisions, do-not-repeats, and actions.
- **Record bugs** with ` + "`record_bug`" + ` (error, root cause, fix) and **search**
  them with ` + "`search_bugs`" + ` before fixing something.
- **Recall** relevant memory for the current task with ` + "`recall_memory`" + `.

Recarto reinforces near-duplicate memories instead of duplicating them, so record
freely — repeated learnings converge to one entry.
`

// Install sets up Claude Code integration for the repo rooted at dir, invoking
// the recarto binary as `binary`. It is idempotent: re-running adds nothing that
// is already present and preserves unrelated config.
func Install(dir, binary string) (Result, error) {
	var res Result

	// 1. Register the MCP server in .mcp.json.
	mcpPath := filepath.Join(dir, ".mcp.json")
	mcpDoc, err := readJSONObject(mcpPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(mcpDoc, binary) {
		res.MCPStatus = "configured"
	} else {
		res.MCPStatus = "already present"
	}
	if err := writeJSONObject(mcpPath, mcpDoc); err != nil {
		return res, err
	}

	// 2. Install lifecycle hooks into .claude/settings.json.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	settings, err := readJSONObject(settingsPath)
	if err != nil {
		return res, err
	}
	added := 0
	for _, hs := range hookSpecs(binary) {
		if !hasCommand(settings, hs.event, hs.command) {
			appendHook(settings, hs.event, hs.matcher, hs.command)
			added++
		}
	}
	if added > 0 {
		res.HooksStatus = fmt.Sprintf("installed %d", added)
	} else {
		res.HooksStatus = "already present"
	}
	if err := writeJSONObject(settingsPath, settings); err != nil {
		return res, err
	}

	// 3. Write RECARTO.md if absent.
	mdPath := filepath.Join(dir, "RECARTO.md")
	switch _, err := os.Stat(mdPath); {
	case err == nil:
		res.InstructionsStatus = "already present"
	case os.IsNotExist(err):
		if err := os.WriteFile(mdPath, []byte(instructions), 0o644); err != nil {
			return res, err
		}
		res.InstructionsStatus = "written"
	default:
		return res, err
	}

	return res, nil
}
