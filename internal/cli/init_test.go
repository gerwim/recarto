package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesConfig(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Init(&out, dir, "recarto"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf(".mcp.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json not created: %v", err)
	}
	if !strings.Contains(out.String(), "mcp server") {
		t.Errorf("init output missing summary:\n%s", out.String())
	}
}
