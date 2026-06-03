// Package claudecode installs Recarto's Claude Code integration (MCP server
// registration + lifecycle hooks) and implements the hook entrypoints. The
// installer merges into existing .mcp.json / .claude/settings.json without
// clobbering unrelated content and is idempotent.
package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// readJSONObject reads a JSON object file into a map. A missing or empty file
// yields an empty (non-nil) map and no error, so callers can merge unconditionally.
func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeJSONObject writes m as indented JSON, creating parent directories.
func writeJSONObject(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

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
