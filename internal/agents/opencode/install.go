package opencode

import (
	"os"
	"path/filepath"
	"reflect"

	"github.com/gerwim/recarto/internal/agents"
	"github.com/gerwim/recarto/internal/features"
)

// ensureMCPServer keeps the recarto entry in an opencode.json "mcp" doc current.
// OpenCode local servers use type:"local" with command as a string array. The V1
// shape (mcp.recarto, enabled) is read by OpenCode 1 and 2; OpenCode 2's native
// shape nests servers under mcp.servers (disabled), and a native entry wins over
// a legacy one of the same name. So it refreshes the command of every recarto
// entry present — preserving any unrelated keys the user added (e.g. enabled) —
// and, when there is none, creates one in the doc's own format. It reports
// whether it changed doc.
func ensureMCPServer(doc map[string]any, binary string) bool {
	mcp, ok := doc["mcp"].(map[string]any)
	if !ok {
		mcp = map[string]any{}
		doc["mcp"] = mcp
	}
	// mcp.servers is OpenCode 2's server map unless it is itself a V1 server entry.
	native, _ := mcp["servers"].(map[string]any)
	if t := native["type"]; t == "local" || t == "remote" {
		native = nil
	}
	want := []any{binary, "serve"}
	changed, found := false, false
	for _, m := range []map[string]any{mcp, native} {
		existing, ok := m["recarto"].(map[string]any)
		if !ok {
			continue
		}
		found = true
		if !reflect.DeepEqual(existing["command"], want) {
			existing["command"] = want
			changed = true
		}
	}
	if found {
		return changed
	}
	if native != nil {
		native["recarto"] = map[string]any{"type": "local", "command": want, "disabled": false}
	} else {
		mcp["recarto"] = map[string]any{"type": "local", "command": want, "enabled": true}
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

	if err := agents.WriteInstructionsBlock(docPath, features.Load()); err != nil {
		return res, err
	}
	res.Docs = "written"
	return res, nil
}
