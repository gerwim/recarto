// Package claudecode installs Recarto's Claude Code integration (MCP server
// registration + lifecycle hooks + instructions) and implements the Claude Code
// hook adapter. The installer merges into existing .mcp.json / settings.json
// without clobbering unrelated content and is idempotent. Shared JSON object
// read/write lives in the parent agents package.
package claudecode

import (
	"fmt"
	"reflect"
	"strings"
)

// ensureMCPServer keeps the mcpServers.recarto entry current. It creates the
// entry when absent and refreshes the command/args in place when a shipped
// update drifts them (e.g. the binary moved), preserving any unrelated keys the
// user added. It reports whether it changed doc.
func ensureMCPServer(doc map[string]any, binary string) bool {
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	existing, ok := servers["recarto"].(map[string]any)
	if !ok {
		servers["recarto"] = map[string]any{
			"command": binary,
			"args":    []any{"serve"},
		}
		return true
	}
	changed := false
	if existing["command"] != binary {
		existing["command"] = binary
		changed = true
	}
	if !reflect.DeepEqual(existing["args"], []any{"serve"}) {
		existing["args"] = []any{"serve"}
		changed = true
	}
	return changed
}

// isOwnedCommand reports whether a settings.json hook command was installed by
// recarto for the Claude agent. Recarto's hook commands follow the grammar
// `<binary> hook <event> --agent claude`, so this recognizes them regardless of
// where the binary lives — letting a re-install prune hooks shipped by an older
// version (renamed events, moved binary) instead of leaving them to pile up.
func isOwnedCommand(c string) bool {
	return strings.Contains(c, " hook ") && strings.Contains(c, "--agent claude")
}

// ownedCommands returns the set of recarto-owned hook commands currently in
// settings.
func ownedCommands(settings map[string]any) map[string]bool {
	out := map[string]bool{}
	hooks, _ := settings["hooks"].(map[string]any)
	for _, v := range hooks {
		groups, _ := v.([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); isOwnedCommand(c) {
					out[c] = true
				}
			}
		}
	}
	return out
}

// removeOwnedHooks strips every recarto-owned hook command from settings,
// dropping any hook group (and event) left empty, while preserving foreign hooks.
func removeOwnedHooks(settings map[string]any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return
	}
	for event, v := range hooks {
		groups, _ := v.([]any)
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			keptHooks := make([]any, 0, len(hs))
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); isOwnedCommand(c) {
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 {
				continue
			}
			gm["hooks"] = keptHooks
			kept = append(kept, gm)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
}

// syncHooks reconciles recarto's lifecycle hooks in settings against the current
// hookSpecs. If the owned set already matches it is a no-op; otherwise it prunes
// the owned hooks and re-adds the current set, so re-running install upgrades
// stale hooks in place. It returns a human-readable status for InstallResult.
func syncHooks(settings map[string]any, binary string) string {
	desired := hookSpecs(binary)
	want := make(map[string]bool, len(desired))
	for _, hs := range desired {
		want[hs.command] = true
	}
	if reflect.DeepEqual(ownedCommands(settings), want) {
		return "already present"
	}
	removeOwnedHooks(settings)
	for _, hs := range desired {
		appendHook(settings, hs.event, hs.matcher, hs.command)
	}
	return fmt.Sprintf("installed %d", len(desired))
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
