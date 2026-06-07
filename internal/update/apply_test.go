package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfUpdateApplierReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "recarto-fake")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := SelfUpdateApplier{TargetPath: target}
	if err := a.Apply(strings.NewReader("NEW")); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "NEW" {
		t.Fatalf("target not replaced: %q", got)
	}
}
