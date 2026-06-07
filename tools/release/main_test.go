package main

import (
	"os"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/update"
)

func TestBuildReleaseHashesBinaries(t *testing.T) {
	dir := t.TempDir()
	lin := filepath.Join(dir, "linux")
	if err := os.WriteFile(lin, []byte("LINUXBIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := buildRelease("1.5.0", "2026-06-07T00:00:00Z", []artifactInput{
		{Platform: "linux-amd64", File: lin, RPath: "recarto/1.5.0/recarto-linux-amd64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := rel.Artifacts["linux-amd64"]
	if a.Path != "recarto/1.5.0/recarto-linux-amd64" || a.Size != 8 {
		t.Fatalf("bad artifact: %+v", a)
	}
	if err := update.VerifySHA256([]byte("LINUXBIN"), a.SHA256); err != nil {
		t.Fatalf("hash wrong: %v", err)
	}
}
