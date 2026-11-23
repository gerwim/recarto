//go:build !windows

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	lock := filepath.Join(dir, ".stale.lock")
	write := func(mode fs.FileMode, data string) fs.FileInfo {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	present := func() bool {
		t.Helper()
		_, err := os.Lstat(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}

	t.Run("removed", func(t *testing.T) {
		listed := write(0, "")
		if removed, err := RemoveUnreadable(path, listed, lock); err != nil || !removed || present() {
			t.Fatalf("RemoveUnreadable = %v, %v; present %v", removed, err, present())
		}
	})
	t.Run("changed since the listing", func(t *testing.T) {
		listed := write(0, "")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		write(0, "a newer file")
		if removed, err := RemoveUnreadable(path, listed, lock); err != nil || removed || !present() {
			t.Fatalf("RemoveUnreadable = %v, %v; present %v", removed, err, present())
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("readable", func(t *testing.T) {
		listed := write(0o644, "")
		if removed, err := RemoveUnreadable(path, listed, lock); err != nil || removed || !present() {
			t.Fatalf("RemoveUnreadable = %v, %v; present %v", removed, err, present())
		}
	})
}
