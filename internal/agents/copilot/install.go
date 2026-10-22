package copilot

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

// hookFile is the recarto-owned Copilot CLI hook config. Copilot merges hook files
// from all sources, so a dedicated file needs no JSON merge and is idempotent by
// construction (overwrite). Each postToolUse entry is matched to one built-in
// tool (single names: Copilot before v1.0.63 dropped pipe-separated postToolUse
// matchers), so a tool call starts at most the one Recarto handler it feeds and
// any other tool starts none. Every entry carries both shell keys: Copilot runs
// `bash` on Linux/macOS and `powershell` on Windows.
func hookFile(binary string) []byte {
	cmd := func(event, matcher string) map[string]any {
		args := " hook " + event + " --agent copilot"
		h := map[string]any{
			"type":       "command",
			"bash":       shQuote(binary) + args,
			"powershell": "& " + psQuote(binary) + args,
		}
		if matcher != "" {
			h["matcher"] = matcher
		}
		return h
	}
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"sessionStart": []any{cmd("session-start", "")},
			"postToolUse": []any{
				cmd("post-read", "view"),
				cmd("post-edit", "edit"),
				cmd("post-edit", "create"),
				cmd("augment", "grep"),
				cmd("augment", "glob"),
			},
			"agentStop": []any{cmd("stop", "")},
		},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep PowerShell's `&` readable
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
	return b.Bytes()
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// psQuote single-quotes s for PowerShell (a quote inside doubles); the caller
// prefixes the call operator `&` so the quoted path runs as a command.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ensureMCPServer keeps the recarto entry in a Copilot mcpServers doc current:
// it creates the entry when absent and refreshes the command/args in place when
// a shipped update drifts them, preserving any unrelated keys the user added. It
// reports whether it changed doc.
func ensureMCPServer(doc map[string]any, binary string) bool {
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	existing, ok := servers["recarto"].(map[string]any)
	if !ok {
		servers["recarto"] = map[string]any{
			"type":    "local",
			"command": binary,
			"args":    []any{"serve"},
			"tools":   []any{"*"},
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
