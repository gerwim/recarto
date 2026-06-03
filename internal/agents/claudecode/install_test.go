package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCreatesConfigAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	// A pre-existing, unrelated hook must be preserved across install.
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"keep-me"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(dir, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.HooksStatus, "installed") {
		t.Errorf("HooksStatus = %q, want installed N", res.HooksStatus)
	}
	if res.MCPStatus != "configured" {
		t.Errorf("MCPStatus = %q, want configured", res.MCPStatus)
	}

	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json missing recarto serve:\n%s", mcp)
	}

	settings, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	for _, want := range []string{
		"keep-me",
		"recarto hook session-start",
		"recarto hook post-read",
		"recarto hook post-edit",
		"recarto hook augment",
		"recarto hook stop",
	} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("settings.json missing %q:\n%s", want, settings)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "RECARTO.md")); err != nil {
		t.Errorf("RECARTO.md not written: %v", err)
	}

	// Re-running installs nothing new.
	res2, err := Install(dir, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res2.HooksStatus != "already present" || res2.MCPStatus != "already present" {
		t.Errorf("second install not idempotent: %+v", res2)
	}
}
