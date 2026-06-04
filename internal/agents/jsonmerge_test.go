package agents

import (
	"path/filepath"
	"testing"
)

func TestReadWriteJSONObjectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")

	m, err := ReadJSONObject(path) // missing file → empty object, no error
	if err != nil || len(m) != 0 {
		t.Fatalf("missing file should yield empty map: %v, %v", m, err)
	}
	m["x"] = "y"
	if err := WriteJSONObject(path, m); err != nil { // creates parent dirs
		t.Fatal(err)
	}
	got, err := ReadJSONObject(path)
	if err != nil || got["x"] != "y" {
		t.Fatalf("round trip failed: %v, %v", got, err)
	}
}
