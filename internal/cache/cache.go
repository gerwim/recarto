// Package cache resolves where Recarto stores its per-repo SQLite database:
// outside the working tree, shared across all worktrees of a repo.
package cache

import (
	"os"
	"path/filepath"
)

// Dir returns the Recarto cache root: $RECARTO_CACHE_DIR, else <os-cache>/recarto.
func Dir() (string, error) {
	if d := os.Getenv("RECARTO_CACHE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "recarto"), nil
}

// StorePath returns the absolute path to the store.db for the given repo id.
func StorePath(repoID string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, repoID, "store.db"), nil
}
