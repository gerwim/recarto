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

// ownedCommands returns the recarto-owned hook commands currently in settings,
// mapped to their `shell` and `if` fields (hookFields). Tracking them lets a
// re-install detect installs whose hooks predate the per-OS shell or a changed
// filter and re-sync them, rather than treating a matching command set alone
// as up to date.
func ownedCommands(settings map[string]any) map[string]string {
	out := map[string]string{}
	hooks, _ := settings["hooks"].(map[string]any)
	for _, v := range hooks {
		groups, _ := v.([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); isOwnedCommand(c) {
					shell, _ := hm["shell"].(string)
					ifRule, _ := hm["if"].(string)
					out[c] = hookFields(shell, ifRule)
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
	want := make(map[string]string, len(desired))
	for _, hs := range desired {
		want[hs.command] = hookFields(hs.shell, hs.ifRule)
	}
	if reflect.DeepEqual(ownedCommands(settings), want) {
		return "already present"
	}
	removeOwnedHooks(settings)
	// Hooks sharing an event and matcher (the per-command Bash hooks) go into
	// one group.
	groups := map[string]map[string]any{}
	for _, hs := range desired {
		k := hs.event + "\x00" + hs.matcher
		if g, ok := groups[k]; ok {
			g["hooks"] = append(g["hooks"].([]any), hookEntry(hs))
			continue
		}
		groups[k] = appendHook(settings, hs)
	}
	return fmt.Sprintf("installed %d", len(desired))
}

// hookFields is the comparable form of a hook's shell and if fields.
func hookFields(shell, ifRule string) string { return shell + "\x00" + ifRule }

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

// appendHook appends a command hook group for hs.event holding hs, and returns
// the group. The matcher is omitted when empty (events like SessionStart/Stop
// run unconditionally).
func appendHook(settings map[string]any, hs hookSpec) map[string]any {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	groups, _ := hooks[hs.event].([]any)
	group := map[string]any{"hooks": []any{hookEntry(hs)}}
	if hs.matcher != "" {
		group["matcher"] = hs.matcher
	}
	hooks[hs.event] = append(groups, group)
	return group
}

// hookEntry is hs's command hook handler. shell is omitted when empty (Claude
// uses its default shell for the platform), as is an empty if.
func hookEntry(hs hookSpec) map[string]any {
	entry := map[string]any{"type": "command", "command": hs.command}
	if hs.shell != "" {
		entry["shell"] = hs.shell
	}
	if hs.ifRule != "" {
		entry["if"] = hs.ifRule
	}
	return entry
}
