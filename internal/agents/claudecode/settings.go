// Package claudecode installs Recarto's Claude Code integration (MCP server
// registration + lifecycle hooks + instructions) and implements the Claude Code
// hook adapter. The installer merges into existing .mcp.json / settings.json
// without clobbering unrelated content and is idempotent. Shared JSON object
// read/write lives in the parent agents package.
package claudecode

// ensureMCPServer adds an mcpServers.recarto entry to doc if absent. It reports
// whether it changed doc.
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
		"command": binary,
		"args":    []any{"serve"},
	}
	return true
}

// hasCommand reports whether settings already contains a command hook with the
// exact command string under the given event (used for idempotency).
func hasCommand(settings map[string]any, event, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); c == command {
				return true
			}
		}
	}
	return false
}

// appendHook appends a command hook group for event. matcher is omitted when
// empty (events like SessionStart/Stop run unconditionally).
func appendHook(settings map[string]any, event, matcher, command string) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	groups, _ := hooks[event].([]any)
	entry := map[string]any{"type": "command", "command": command}
	group := map[string]any{"hooks": []any{entry}}
	if matcher != "" {
		group["matcher"] = matcher
	}
	hooks[event] = append(groups, group)
}
