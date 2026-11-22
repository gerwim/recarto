//go:build !windows

package store

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f, or a shared one when shared is set;
// shared holders coexist, and any holder excludes an exclusive lock. It waits
// for the lock when wait is set and otherwise reports false when another open
// file holds it. Closing f or
// exiting releases it. flock binds to the open file description, so two opens
// in one process exclude each other, which POSIX fcntl locks would not.
func lockFile(f *os.File, wait, shared bool) (bool, error) {
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		// A signal can cut a wait short.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return err == nil, err
	}
}

// RemoveIf removes the file at path if match, called on that file as opened
// and before any lock, recognizes it as the file the caller decided to remove.
// match only recognizes; the decision is the caller's, made beforehand. The
// file stays open from match to its removal, and its lock is taken only after
// match accepts it. It never waits: removed is false, with no error, when the
// file is missing, match rejects it, it has left the path, or another RemoveIf
// call holds it, which then removes it. It removes only that very file.
//
// The lock is the file's own, so removals of different files never meet and
// nothing makes a removal wait.
func RemoveIf(path string, match func(f *os.File, fi fs.FileInfo) bool) (removed bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Nothing was written through f, so a close error loses nothing.
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !match(f, fi) {
		return false, nil
	}
	got, err := lockFile(f, false, false)
	if err != nil || !got {
		// Every caller decides before calling, and its match accepts only the
		// very file it decided on: whoever holds this lock has decided to remove
		// this file, and only it can unlink it until it releases.
		return false, err
	}
	// f is still open, so its file cannot be freed and its ID cannot pass to
	// another file while the two are compared.
	now, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(fi, now) {
		return false, nil
	}
	// Every remover of these files goes through RemoveIf and unlinks a file only
	// while it holds that file's lock, so no one else unlinks it meanwhile, and a
	// hard link cannot create a new file at a name that is still taken.
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// LockHeld reports whether an exclusive holder (LockFile, TryLockFile) has the
// lock on the existing file at path. A missing file fails with fs.ErrNotExist.
func LockHeld(path string) (held bool, err error) {
	release, ok, err := TryShareLockFile(path)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	release()
	return false, nil
}
