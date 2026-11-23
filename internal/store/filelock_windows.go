//go:build windows

package store

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f, or a shared one when shared is set;
// shared holders coexist, and any holder excludes an exclusive lock. It waits
// for the lock when wait is set and otherwise reports false when another
// handle holds it. Closing f or exiting
// releases it. The locked byte sits at 4 GiB, past the content: Windows locks
// are mandatory, and the trace must stay readable by processes that do not
// hold the lock.
func lockFile(f *os.File, wait, shared bool) (bool, error) {
	var flags uint32
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{OffsetHigh: 1})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return err == nil, err
}

// openShared opens the existing file at path for reading, for a probe or a
// shared lock.
//
// A probe or a shared waiter must never keep a liveness file's creator from
// removing it: Windows refuses a deletion while any handle lacks FILE_SHARE_DELETE.
func openShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// RemoveIf removes the file at path if match, called on that file as opened
// and before any lock, recognizes it as the file the caller decided to remove.
// match only recognizes; the decision is the caller's, made beforehand. It
// never waits: removed is false, with no error, when the file is missing,
// match rejects it, or another handle has it open (another RemoveIf call of
// the same file included). It removes only that very file.
//
// The open leaves out FILE_SHARE_DELETE, as Go's own opens do, so no file
// another Recarto process has open can be deleted, and two removals of one
// file exclude each other.
func RemoveIf(path string, match func(f *os.File, fi fs.FileInfo) bool) (removed bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	f := os.NewFile(uintptr(h), path)
	// Nothing was written through f, so a close error loses nothing.
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !match(f, fi) {
		return false, nil
	}
	// FILE_DISPOSITION_INFO holds one BOOLEAN: delete exactly the opened file,
	// never whatever is at the path, once its handle closes.
	deleteFile := byte(1)
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &deleteFile, 1); err != nil {
		return false, err
	}
	return true, nil
}

// LockHeld reports whether an exclusive holder (LockFile, TryLockFile) has the
// lock on the existing file at path. A missing file fails with fs.ErrNotExist.
func LockHeld(path string) (held bool, err error) {
	f, err := openShared(path)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		// Only a deletion opens these files with delete access, and it gets that
		// open only while no other handle, an exclusive holder's included, has
		// the file: so no holder has it.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Closing f drops the shared lock taken below, so a close error loses nothing.
	defer func() { _ = f.Close() }()
	ok, err := lockFile(f, false, true)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// RemoveUnreadable would remove the regular file listed at path, which the
// caller could not open to read, when no process of the user could read it.
// It removes nothing: Windows grants each process rights from its own token, so
// a file this process may not read may still be a record another Recarto
// process of the user reads. Nothing here can tell the two apart, so such a
// file stays until the user deletes it.
func RemoveUnreadable(path string, listed fs.FileInfo, lockPath string) (removed bool, err error) {
	return false, nil
}
