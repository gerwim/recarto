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

// RemoveUnreadable removes the regular file listed at path, which the caller
// could not open to read, when the file's owner bits deny its owner, this
// user, reading it: no process of the user that honors them can read it then,
// so none can hold it as a record. It never waits. removed is false, with no
// error, when the file is gone or is no longer the listed one, when another
// process of the user may read it, or when another removal holds lockPath.
// It removes only the listed file, never a symlink's target, and changes no
// permission. On Windows it removes nothing.
func RemoveUnreadable(path string, listed fs.FileInfo, lockPath string) (removed bool, err error) {
	// Checked before the lock, so a file this call leaves never creates the lock file.
	if ok, err := unreadableListed(path, listed); err != nil || !ok {
		return false, err
	}
	release, ok, err := TryLockFile(lockPath)
	if err != nil || !ok {
		// Another removal of an unreadable file in this folder holds the lock;
		// the next session start tries again.
		return false, err
	}
	defer release()
	if ok, err := unreadableListed(path, listed); err != nil || !ok {
		return false, err
	}
	// No process of the user that honors file permissions can read this file, so
	// none takes it for a record: a record removal cannot open it, a stamp
	// replaces only a guard it read, and a link needs a free name. Only
	// RemoveUnreadable unlinks it, each call under lockPath, so it stays at the
	// path until the removal below.
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// unreadableListed reports whether path still holds the listed regular file,
// owned by this user with its owner read bit clear.
func unreadableListed(path string, listed fs.FileInfo) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, listed) ||
		!fi.ModTime().Equal(listed.ModTime()) || fi.Size() != listed.Size() {
		return false, nil
	}
	// A file whose owner bits let its owner read it was denied to this process
	// alone, by a sandbox such as Landlock. Another Recarto process of the user
	// may read it as a record and stamp or remove it, so it is left to a process
	// that can read it.
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid() && fi.Mode().Perm()&0o400 == 0, nil
}
