package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestInstallRepoScope(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	res, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q", res.MCP)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "opencode.json"))
	cs := string(cfg)
	if !strings.Contains(cs, `"mcp"`) || !strings.Contains(cs, `"recarto"`) || !strings.Contains(cs, `"local"`) {
		t.Errorf("opencode.json: %s", cs)
	}
	plug, _ := os.ReadFile(filepath.Join(dir, ".opencode", "plugins", "recarto.js"))
	ps := string(plug)
	for _, want := range []string{"experimental.chat.system.transform", "file.edited", "/bin/recarto", `run("session-start"`, "--agent opencode"} {
		if !strings.Contains(ps, want) {
			t.Errorf("plugin missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing: %v", err)
	}
	res2, _ := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if res2.MCP != "already present" {
		t.Errorf("not idempotent: %+v", res2)
	}
}

func TestEnsureMCPServerRefreshesOnBinaryDrift(t *testing.T) {
	doc := map[string]any{}
	ensureMCPServer(doc, "/old/recarto")
	if !ensureMCPServer(doc, "/new/recarto") {
		t.Fatal("ensureMCPServer should report a change when the binary path drifts")
	}
	rc := doc["mcp"].(map[string]any)["recarto"].(map[string]any)
	cmd := rc["command"].([]any)
	if cmd[0] != "/new/recarto" {
		t.Errorf("command[0] = %v, want /new/recarto", cmd[0])
	}
	if ensureMCPServer(doc, "/new/recarto") {
		t.Error("ensureMCPServer should be a no-op when nothing drifts")
	}
}

func TestEnsureMCPServerPreservesEnabledFlag(t *testing.T) {
	doc := map[string]any{"mcp": map[string]any{"recarto": map[string]any{
		"type":    "local",
		"command": []any{"/old/recarto", "serve"},
		"enabled": false,
	}}}
	if !ensureMCPServer(doc, "/new/recarto") {
		t.Fatal("expected a change for drifted command")
	}
	rc := doc["mcp"].(map[string]any)["recarto"].(map[string]any)
	if rc["enabled"] != false {
		t.Errorf("user-set enabled flag was clobbered: %#v", rc)
	}
}

func TestInstallUserScopeTargets(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeUser, Dir: dir, Home: home}, "/bin/recarto"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range [][]string{
		{".config", "opencode", "opencode.json"},
		{".config", "opencode", "plugins", "recarto.js"},
		{".config", "opencode", "AGENTS.md"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Errorf("user target %v missing: %v", rel, err)
		}
	}
}
