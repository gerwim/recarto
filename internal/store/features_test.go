package store

import (
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/features"
)

func TestAutoRepairFollowsConfig(t *testing.T) {
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	p := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("RECARTO_CONFIG", p)
	if !AutoRepairEnabled() {
		t.Fatal("auto-repair must be on by default")
	}
	if err := features.Save(p, features.Set{features.AutoRepair: false}); err != nil {
		t.Fatal(err)
	}
	if AutoRepairEnabled() {
		t.Fatal("auto-repair off in the config, still enabled")
	}
}
