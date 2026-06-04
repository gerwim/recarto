package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitClaudeRepoScope(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Init(&out, dir, "recarto", []string{"--agent", "claude", "--scope", "repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf(".mcp.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json not created: %v", err)
	}
	if !strings.Contains(out.String(), "claude") || !strings.Contains(out.String(), "mcp:") {
		t.Errorf("init output missing summary:\n%s", out.String())
	}
}

func TestInitAllAgentsRepoScope(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := Init(&out, dir, "/bin/recarto", []string{"--agent", "claude,opencode,copilot", "--scope", "repo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".mcp.json", "opencode.json", filepath.Join(".github", "hooks", "recarto.json"), "AGENTS.md", filepath.Join(".opencode", "plugins", "recarto.js")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	for _, want := range []string{"claude", "opencode", "copilot"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output should summarize %q:\n%s", want, out.String())
		}
	}
}

func TestInitUnknownAgentErrors(t *testing.T) {
	if err := Init(&bytes.Buffer{}, t.TempDir(), "/bin/recarto", []string{"--agent", "vim", "--scope", "repo"}); err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestInitInvalidScopeErrors(t *testing.T) {
	if err := Init(&bytes.Buffer{}, t.TempDir(), "/bin/recarto", []string{"--scope", "galaxy"}); err == nil {
		t.Error("expected error for invalid scope")
	}
}
