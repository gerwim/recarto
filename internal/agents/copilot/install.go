package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// hookFile is the recarto-owned Copilot CLI hook config. Copilot merges hook files
// from all sources, so a dedicated file needs no JSON merge and is idempotent by
// construction (overwrite). postToolUse fans out to the post-edit/post-read/augment
// handlers; the adapter pulls the file path / pattern from toolArgs regardless of
// which tool fired.
func hookFile(binary string) []byte {
	cmd := func(event string) map[string]any {
		return map[string]any{"type": "command", "bash": binary + " hook " + event + " --agent copilot"}
	}
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"sessionStart": []any{cmd("session-start")},
			"postToolUse":  []any{cmd("post-edit"), cmd("post-read"), cmd("augment")},
			"agentStop":    []any{cmd("stop")},
		},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n')
}

// ensureMCPServer adds the recarto entry to a Copilot mcpServers doc if absent.
func ensureMCPServer(doc map[string]any, binary string) bool {
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	if _, exists := servers["recarto"]; exists {
		return false
	}
	servers["recarto"] = map[string]any{
		"type":    "local",
		"command": binary,
		"args":    []any{"serve"},
		"tools":   []any{"*"},
	}
	return true
}

// Install configures Recarto for GitHub Copilot CLI at the given scope. It is
// idempotent: the MCP entry is merged, the hook file and instructions block are
// rewritten in place.
func Install(p agents.Paths, binary string) (agents.InstallResult, error) {
	res := agents.InstallResult{Agent: "copilot"}

	var mcpPath, hookPath, docPath string
	switch p.Scope {
	case agents.ScopeUser:
		mcpPath = p.UserFile(".copilot", "mcp-config.json")
		hookPath = p.UserFile(".copilot", "hooks", "recarto.json")
		docPath = p.UserFile(".copilot", "copilot-instructions.md")
	default:
		mcpPath = p.RepoFile(".mcp.json")
		hookPath = p.RepoFile(".github", "hooks", "recarto.json")
		docPath = p.RepoFile("AGENTS.md")
	}

	doc, err := agents.ReadJSONObject(mcpPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(doc, binary) {
		res.MCP = "configured"
	} else {
		res.MCP = "already present"
	}
	if err := agents.WriteJSONObject(mcpPath, doc); err != nil {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(hookPath, hookFile(binary), 0o644); err != nil {
		return res, err
	}
	res.Hooks = "written"

	if err := agents.WriteInstructionsBlock(docPath); err != nil {
		return res, err
	}
	res.Docs = "written"
	return res, nil
}
