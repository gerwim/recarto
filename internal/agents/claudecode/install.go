package claudecode

import (
	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// hookSpec is one (event, matcher, command) hook the installer registers.
type hookSpec struct{ event, matcher, command string }

// hookSpecs are the advisory hooks. SessionStart and Stop run unconditionally
// (no matcher); post-read, post-edit, and augment are scoped to their tools. Each
// command carries `--agent claude` so the dispatcher selects the Claude adapter.
func hookSpecs(binary string) []hookSpec {
	return []hookSpec{
		{"SessionStart", "", binary + " hook session-start --agent claude"},
		{"PostToolUse", "Read", binary + " hook post-read --agent claude"},
		{"PostToolUse", "Edit|Write", binary + " hook post-edit --agent claude"},
		{"PostToolUse", "Grep|Glob", binary + " hook augment --agent claude"},
		{"Stop", "", binary + " hook stop --agent claude"},
	}
}

// Install sets up Claude Code integration at the given scope. It is idempotent:
// re-running adds nothing already present and preserves unrelated config.
//
// Repo scope writes .mcp.json, .claude/settings.json, and AGENTS.md. User scope
// writes ~/.claude.json (mcpServers), ~/.claude/settings.json (hooks), and
// ~/.claude/CLAUDE.md (Claude's global instructions file).
func Install(p agents.Paths, binary string) (agents.InstallResult, error) {
	res := agents.InstallResult{Agent: "claude"}

	var mcpPath, settingsPath, docPath string
	switch p.Scope {
	case agents.ScopeUser:
		mcpPath = p.UserFile(".claude.json")
		settingsPath = p.UserFile(".claude", "settings.json")
		docPath = p.UserFile(".claude", "CLAUDE.md")
	default:
		mcpPath = p.RepoFile(".mcp.json")
		settingsPath = p.RepoFile(".claude", "settings.json")
		docPath = p.RepoFile("AGENTS.md")
	}

	// 1. Register the MCP server.
	mcpDoc, err := agents.ReadJSONObject(mcpPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(mcpDoc, binary) {
		res.MCP = "configured"
	} else {
		res.MCP = "already present"
	}
	if err := agents.WriteJSONObject(mcpPath, mcpDoc); err != nil {
		return res, err
	}

	// 2. Install lifecycle hooks.
	settings, err := agents.ReadJSONObject(settingsPath)
	if err != nil {
		return res, err
	}
	res.Hooks = syncHooks(settings, binary)
	if err := agents.WriteJSONObject(settingsPath, settings); err != nil {
		return res, err
	}

	// 3. Write the instructions block.
	if err := agents.WriteInstructionsBlock(docPath); err != nil {
		return res, err
	}
	res.Docs = "written"

	return res, nil
}
