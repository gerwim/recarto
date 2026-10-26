package remotesync

import (
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/features"
)

func TestSyncFollowsConfig(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "https://sync.example")
	t.Setenv("RECARTO_SYNC_KEY", "k")
	p := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("RECARTO_CONFIG", p)
	if !ConfigFromEnv().Enabled() {
		t.Fatal("sync must be on by default when configured")
	}
	if err := features.Save(p, features.Set{features.Sync: false}); err != nil {
		t.Fatal(err)
	}
	if ConfigFromEnv().Enabled() {
		t.Fatal("sync off in the config, still enabled")
	}
}
