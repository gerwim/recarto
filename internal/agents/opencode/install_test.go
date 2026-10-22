package opencode

import (
	"os"
	"path/filepath"
	"reflect"
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
	for _, want := range []string{`export default { id: "recarto", setup, server };`, `ctx.session.hook("context"`, "experimental.chat.system.transform", `const BIN = "/bin/recarto";`} {
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

func TestEnsureMCPServerFormats(t *testing.T) {
	cmd := func(bin string) []any { return []any{bin, "serve"} }
	for _, tc := range []struct {
		name   string
		mcp    map[string]any
		legacy map[string]any // want mcp.recarto
		native map[string]any // want mcp.servers.recarto
	}{
		{
			name:   "empty config gets the V1 shape OpenCode 1 and 2 both read",
			mcp:    map[string]any{},
			legacy: map[string]any{"type": "local", "command": cmd("/new"), "enabled": true},
		},
		{
			name:   "OpenCode 2 native config gets a native entry, not a legacy twin",
			mcp:    map[string]any{"servers": map[string]any{"other": map[string]any{"type": "local"}}},
			native: map[string]any{"type": "local", "command": cmd("/new"), "disabled": false},
		},
		{
			name:   "drifted native entry is refreshed, keeping the user's disabled flag",
			mcp:    map[string]any{"servers": map[string]any{"recarto": map[string]any{"type": "local", "command": cmd("/old"), "disabled": true}}},
			native: map[string]any{"type": "local", "command": cmd("/new"), "disabled": true},
		},
		{
			name: "legacy entry in a native config is refreshed in place",
			mcp: map[string]any{
				"recarto": map[string]any{"type": "local", "command": cmd("/old"), "enabled": true},
				"servers": map[string]any{"other": map[string]any{"type": "local"}},
			},
			legacy: map[string]any{"type": "local", "command": cmd("/new"), "enabled": true},
		},
		{
			name: "legacy and native twins are both refreshed",
			mcp: map[string]any{
				"recarto": map[string]any{"type": "local", "command": cmd("/old"), "enabled": true},
				"servers": map[string]any{"recarto": map[string]any{"type": "local", "command": cmd("/old")}},
			},
			legacy: map[string]any{"type": "local", "command": cmd("/new"), "enabled": true},
			native: map[string]any{"type": "local", "command": cmd("/new")},
		},
		{
			name:   "a V1 server named servers is not the OpenCode 2 server map",
			mcp:    map[string]any{"servers": map[string]any{"type": "local", "command": cmd("/x")}},
			legacy: map[string]any{"type": "local", "command": cmd("/new"), "enabled": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{"mcp": tc.mcp}
			if !ensureMCPServer(doc, "/new") {
				t.Fatal("expected a change")
			}
			if ensureMCPServer(doc, "/new") {
				t.Error("second run should be a no-op")
			}
			mcp := doc["mcp"].(map[string]any)
			servers, _ := mcp["servers"].(map[string]any)
			if got, _ := mcp["recarto"].(map[string]any); !reflect.DeepEqual(got, tc.legacy) {
				t.Errorf("mcp.recarto = %#v, want %#v", got, tc.legacy)
			}
			if got, _ := servers["recarto"].(map[string]any); !reflect.DeepEqual(got, tc.native) {
				t.Errorf("mcp.servers.recarto = %#v, want %#v", got, tc.native)
			}
		})
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
