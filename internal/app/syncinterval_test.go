package app

import (
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/remotesync"
)

// TestSyncIntervalNonGitWorkspaceOnly covers the 1.2.0 crash state: sync fully
// configured, cwd not a git repo — primary syncer nil, workspace syncer set.
// SyncInterval must come from the configured (workspace) syncer, not from a
// dereference of the nil primary.
func TestSyncIntervalNonGitWorkspaceOnly(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_SYNC_URL", "https://x.invalid")
	t.Setenv("RECARTO_SYNC_KEY", "k")
	t.Setenv("RECARTO_WORKSPACE", "w")
	t.Setenv("RECARTO_SYNC_INTERVAL", "123")
	t.Setenv("RECARTO_REPOS", "")
	t.Setenv("RECARTO_SYNC_REPO", "")

	a, err := Open(Config{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	if a.Sync != nil {
		t.Fatal("primary syncer must be nil in a non-git cwd")
	}
	if !a.HasSync() {
		t.Fatal("workspace syncer must make HasSync true")
	}
	if got := a.SyncInterval(); got != 123*time.Second {
		t.Fatalf("SyncInterval = %v, want 123s from RECARTO_SYNC_INTERVAL", got)
	}
}

// TestSyncIntervalUnconfigured: no syncer anywhere → the package default, so a
// caller never needs its own fallback.
func TestSyncIntervalUnconfigured(t *testing.T) {
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")
	t.Setenv("RECARTO_WORKSPACE", "")
	t.Setenv("RECARTO_REPOS", "")

	a, err := Open(Config{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	if a.HasSync() {
		t.Fatal("no syncer should be configured")
	}
	if got := a.SyncInterval(); got != remotesync.DefaultInterval {
		t.Fatalf("SyncInterval = %v, want the default %v", got, remotesync.DefaultInterval)
	}
}
