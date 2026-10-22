package claudecode

import (
	"runtime"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// goos is the target OS Recarto installs for. It indirects runtime.GOOS so tests
// can exercise the Windows-specific hook shell without running on Windows.
var goos = runtime.GOOS

// hookSpec is one (event, matcher, command, shell, if) hook the installer
// registers. shell is the Claude Code hook `shell` field ("" leaves Claude's
// default); ifRule its `if` permission-rule filter ("" runs it for every
// matched tool call).
type hookSpec struct{ event, matcher, command, shell, ifRule string }

// hookSpecs are the advisory hooks. SessionStart and Stop run unconditionally
// (no matcher); post-read, post-edit, and augment are scoped to their tools. Each
// command carries `--agent claude` so the dispatcher selects the Claude adapter.
//
// Claude Code's native builds have no Grep/Glob tools: the agent searches and
// views files with the Bash tool. One shell hook per agents.ShellCommands key
// runs only when the command line runs that command (its `if`), so any other
// Bash call starts no Recarto process; --cmd tells the hook which key started
// it, so a line matching several keys is answered once.
func hookSpecs(binary string) []hookSpec {
	shell := hookShell()
	specs := []hookSpec{
		{"SessionStart", "", binary + " hook session-start --agent claude", shell, ""},
		{"PostToolUse", "Read", binary + " hook post-read --agent claude", shell, ""},
		{"PostToolUse", "Edit|Write", binary + " hook post-edit --agent claude", shell, ""},
		{"PostToolUse", "Grep|Glob", binary + " hook augment --agent claude", shell, ""},
	}
	for _, c := range agents.ShellCommands {
		rule := c
		if c == "git" {
			rule = "git grep"
		}
		specs = append(specs, hookSpec{"PostToolUse", "Bash", binary + " hook shell --agent claude --cmd " + c, shell, "Bash(" + rule + " *)"})
	}
	return append(specs, hookSpec{"Stop", "", binary + " hook stop --agent claude", shell, ""})
}

// hookShell returns the Claude Code hook `shell` to pin. On Windows, Claude
// launches command hooks via Git Bash when present (else PowerShell), and our
// `recarto hook …` invocation breaks under that bash, so we pin PowerShell. On
// other platforms the default shell is correct, so we leave the field unset.
func hookShell() string {
	if goos == "windows" {
		return "powershell"
	}
	return ""
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
