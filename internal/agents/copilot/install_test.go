package copilot

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
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json: %s", mcp)
	}
	hook, _ := os.ReadFile(filepath.Join(dir, ".github", "hooks", "recarto.json"))
	hs := string(hook)
	for _, want := range []string{`"sessionStart"`, `"postToolUse"`, `"agentStop"`, "hook session-start --agent copilot", "hook post-edit --agent copilot"} {
		if !strings.Contains(hs, want) {
			t.Errorf("hook file missing %q:\n%s", want, hs)
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
		{".copilot", "mcp-config.json"},
		{".copilot", "hooks", "recarto.json"},
		{".copilot", "copilot-instructions.md"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Errorf("user target %v missing: %v", rel, err)
		}
	}
}
