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
