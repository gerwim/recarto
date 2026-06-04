package opencode

import (
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// ensureMCPServer adds the recarto entry to an opencode.json "mcp" doc if absent.
// OpenCode local servers use type:"local" with command as a string array.
func ensureMCPServer(doc map[string]any, binary string) bool {
	mcp, ok := doc["mcp"].(map[string]any)
	if !ok {
		mcp = map[string]any{}
		doc["mcp"] = mcp
	}
	if _, exists := mcp["recarto"]; exists {
		return false
	}
	mcp["recarto"] = map[string]any{
		"type":    "local",
		"command": []any{binary, "serve"},
		"enabled": true,
	}
	return true
}

// Install configures Recarto for OpenCode at the given scope: merges the MCP entry
// into opencode.json, writes the generated plugin, and writes the instructions
// block. Idempotent.
func Install(p agents.Paths, binary string) (agents.InstallResult, error) {
	res := agents.InstallResult{Agent: "opencode"}

	var cfgPath, plugPath, docPath string
	switch p.Scope {
	case agents.ScopeUser:
		cfgPath = p.UserFile(".config", "opencode", "opencode.json")
		plugPath = p.UserFile(".config", "opencode", "plugins", "recarto.js")
		docPath = p.UserFile(".config", "opencode", "AGENTS.md")
	default:
		cfgPath = p.RepoFile("opencode.json")
		plugPath = p.RepoFile(".opencode", "plugins", "recarto.js")
		docPath = p.RepoFile("AGENTS.md")
	}

	doc, err := agents.ReadJSONObject(cfgPath)
	if err != nil {
		return res, err
	}
	if ensureMCPServer(doc, binary) {
		res.MCP = "configured"
	} else {
		res.MCP = "already present"
	}
	if err := agents.WriteJSONObject(cfgPath, doc); err != nil {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(plugPath), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(plugPath, []byte(pluginJS(binary)), 0o644); err != nil {
		return res, err
	}
	res.Hooks = "plugin written"

	if err := agents.WriteInstructionsBlock(docPath); err != nil {
		return res, err
	}
	res.Docs = "written"
	return res, nil
}
