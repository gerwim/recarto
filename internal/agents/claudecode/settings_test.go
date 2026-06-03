package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureMCPServerIdempotent(t *testing.T) {
	doc := map[string]any{}
	if !ensureMCPServer(doc, "recarto") {
		t.Fatal("first ensure should report a change")
	}
	if ensureMCPServer(doc, "recarto") {
		t.Error("second ensure should be a no-op")
	}
	servers := doc["mcpServers"].(map[string]any)
	rc := servers["recarto"].(map[string]any)
	if rc["command"] != "recarto" {
		t.Errorf("command = %v, want recarto", rc["command"])
	}
}

func TestAppendHookAndHasCommandIdempotent(t *testing.T) {
	s := map[string]any{}
	cmd := "recarto hook session-start"
	if hasCommand(s, "SessionStart", cmd) {
		t.Fatal("empty settings should not contain the command")
	}
	appendHook(s, "SessionStart", "", cmd)
	if !hasCommand(s, "SessionStart", cmd) {
		t.Fatal("command should be present after append")
	}
	// A second, unrelated hook is added as a separate group (nothing clobbered).
	appendHook(s, "SessionStart", "", "other-tool --flag")
	groups := s["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(groups) != 2 {
		t.Errorf("expected 2 hook groups, got %d", len(groups))
	}
}

func TestReadWriteJSONObjectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "settings.json")

	m, err := readJSONObject(path) // missing file → empty object, no error
	if err != nil || len(m) != 0 {
		t.Fatalf("missing file should yield empty map: %v, %v", m, err)
	}
	m["x"] = "y"
	if err := writeJSONObject(path, m); err != nil { // creates parent dirs
		t.Fatal(err)
	}
	got, err := readJSONObject(path)
	if err != nil || got["x"] != "y" {
		t.Fatalf("round trip failed: %v, %v", got, err)
	}
	b, _ := os.ReadFile(path)
	var probe map[string]any
	if json.Unmarshal(b, &probe) != nil {
		t.Error("written file is not valid JSON")
	}
}
