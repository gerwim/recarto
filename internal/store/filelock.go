package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// tryLockFile is lockFile without the wait.
func tryLockFile(f *os.File) (bool, error) { return lockFile(f, false, false) }

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
}

// LockFile takes the exclusive OS lock on the file at path, creating the file
// and its directory when needed, and returns the function that releases it.
// While another holder has the lock it calls waiting (when not nil) once, then
// waits until the lock is free or ctx is done. The OS drops a lock with its
// holder, so a killed process never leaves it held. Lock files are never
// removed: removing a locked file would let two processes lock two different
// files of the same name.
func LockFile(ctx context.Context, path string, waiting func()) (release func(), err error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	// Closing only drops the lock, so a close error loses nothing.
	unlock := func() { _ = f.Close() }
	ok, err := lockFile(f, false, false)
	if err != nil {
		unlock()
		return nil, err
	}
	if ok {
		return unlock, nil
	}
	if waiting != nil {
		waiting()
	}
	if _, err := awaitLock(ctx, f, false, nil); err != nil {
		return nil, err
	}
	return unlock, nil
}

// awaitLock waits for the lock on f, exclusive or shared, until it is got, ctx
// is done or timeout fires. ok is false when the wait ended without the lock;
// f is then closed, at once or, for an abandoned wait, once that wait ends.
func awaitLock(ctx context.Context, f *os.File, shared bool, timeout <-chan time.Time) (ok bool, err error) {
	type result struct {
		ok  bool
		err error
	}
	got := make(chan result, 1)
	go func() {
		ok, err := lockFile(f, true, shared)
		got <- result{ok, err}
	}()
	select {
	case r := <-got:
		if r.err == nil && !r.ok {
			r.err = errors.New("lock wait ended without the lock")
		}
		if r.err != nil {
			// Nothing is held, so a close error loses nothing.
			_ = f.Close()
			return false, r.err
		}
		return true, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timeout:
	}
	// The abandoned wait still holds f; closing it once the wait ends means it
	// never keeps the lock.
	go func() { <-got; _ = f.Close() }()
	return false, err
}

// TryLockFile is LockFile without the wait: ok is false, with nothing held,
// while another holder has the lock.
func TryLockFile(path string) (release func(), ok bool, err error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	ok, err = lockFile(f, false, false)
	if err != nil || !ok {
		// Nothing is held, so a close error loses nothing.
		_ = f.Close()
		return nil, false, err
	}
	return func() { _ = f.Close() }, true, nil
}

// TryShareLockFile takes a shared lock on the existing file at path without
// waiting. Any number of holders share it; ok is false, with nothing held,
// while an exclusive holder (LockFile, TryLockFile) has it. Unlike them it
// never creates the file: a missing file fails with fs.ErrNotExist.
func TryShareLockFile(path string) (release func(), ok bool, err error) {
	f, err := openShared(path)
	if err != nil {
		return nil, false, err
	}
	ok, err = lockFile(f, false, true)
	if err != nil || !ok {
		// Nothing is held, so a close error loses nothing.
		_ = f.Close()
		return nil, false, err
	}
	return func() { _ = f.Close() }, true, nil
}

// WaitShareLockFile takes a shared lock on the existing file at path, waiting
// while an exclusive holder has it until ctx is done or ctx's write deadline
// (WithWriteDeadline) has passed; a deadline already passed gets one try.
// ok is false, with nothing held, when the wait ended without the lock. A
// missing file fails with fs.ErrNotExist; the file is never created.
func WaitShareLockFile(ctx context.Context, path string) (release func(), ok bool, err error) {
	f, err := openShared(path)
	if err != nil {
		return nil, false, err
	}
	release = func() { _ = f.Close() }
	ok, err = lockFile(f, false, true)
	if err != nil {
		release()
		return nil, false, err
	}
	if ok {
		return release, true, nil
	}
	var timeout <-chan time.Time
	if deadline, has := writeDeadline(ctx); has {
		left := time.Until(deadline)
		if left <= 0 {
			release()
			return nil, false, nil
		}
		timer := time.NewTimer(left)
		defer timer.Stop()
		timeout = timer.C
	}
	if ok, err := awaitLock(ctx, f, true, timeout); !ok {
		return nil, false, err
	}
	return release, true, nil
}
