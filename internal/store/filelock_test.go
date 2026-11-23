package store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockFileAbandonedWaitReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "wt.lock")
	release, err := LockFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	abandoned := make(chan error, 1)
	go func() {
		_, err := LockFile(ctx, path, func() { close(waiting) })
		abandoned <- err
	}()
	select {
	case <-waiting:
	case <-time.After(time.Minute):
		t.Fatal("the second LockFile never waited")
	}
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned wait = %v, want context.Canceled", err)
	}
	release()

	third := make(chan error, 1)
	go func() {
		r, err := LockFile(context.Background(), path, nil)
		if err == nil {
			r()
		}
		third <- err
	}()
	select {
	case err := <-third:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the abandoned wait kept the lock")
	}
}

func TestLockHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live")
	release, ok, err := TryLockFile(path)
	if err != nil || !ok {
		t.Fatalf("lock: %v, %v", ok, err)
	}
	if held, err := LockHeld(path); err != nil || !held {
		t.Fatalf("while held: %v, %v", held, err)
	}
	release()
	if held, err := LockHeld(path); err != nil || held {
		t.Fatalf("after release: %v, %v", held, err)
	}
	if _, err := LockHeld(path + "-missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v, want fs.ErrNotExist", err)
	}
}

// holding returns a match that accepts a file holding content.
func holding(content string) func(f *os.File, fi fs.FileInfo) bool {
	return func(f *os.File, _ fs.FileInfo) bool {
		data, err := io.ReadAll(f)
		return err == nil && string(data) == content
	}
}

func TestRemoveIfOneRemovalPerFile(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A removal under way holds its file's exclusive lock.
	release, ok, err := TryLockFile(a)
	if err != nil || !ok {
		t.Fatalf("lock a: %v, %v", ok, err)
	}
	if removed, err := RemoveIf(a, holding("one")); err != nil || removed {
		t.Fatalf("RemoveIf(a) while held = %v, %v; want false, nil", removed, err)
	}
	if removed, err := RemoveIf(b, holding("one")); err != nil || !removed {
		t.Fatalf("RemoveIf(b) = %v, %v; want true, nil", removed, err)
	}
	release()
	if removed, err := RemoveIf(a, holding("one")); err != nil || !removed {
		t.Fatalf("RemoveIf(a) after release = %v, %v; want true, nil", removed, err)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still there: %v", p, err)
		}
	}
	if removed, err := RemoveIf(a, holding("one")); err != nil || removed {
		t.Fatalf("RemoveIf of a missing file = %v, %v", removed, err)
	}
}

func TestRemoveIfLeavesAReplacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, path string)
	}{
		{"new file at the path", func(t *testing.T, path string) {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, []byte("two"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"same file overwritten", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("two"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a")
			if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.replace(t, path)
			if removed, err := RemoveIf(path, holding("one")); err != nil || removed {
				t.Fatalf("RemoveIf = %v, %v; want false, nil", removed, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "two" {
				t.Fatalf("file = %q, %v; want two", data, err)
			}
		})
	}
}

// newLockTarget creates an empty file to lock and returns its path.
func newLockTarget(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type shareResult struct {
	release func()
	ok      bool
	err     error
}

// waitShare runs WaitShareLockFile in a goroutine.
func waitShare(ctx context.Context, path string, waiting func()) <-chan shareResult {
	got := make(chan shareResult, 1)
	go func() {
		release, ok, err := WaitShareLockFile(ctx, path, waiting)
		got <- shareResult{release, ok, err}
	}()
	return got
}

func receiveShare(t *testing.T, got <-chan shareResult) shareResult {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(time.Minute):
		t.Fatal("WaitShareLockFile hung")
		return shareResult{}
	}
}

// requireWaiting returns once WaitShareLockFile has called its waiting callback.
func requireWaiting(t *testing.T, waiting <-chan struct{}) {
	t.Helper()
	select {
	case <-waiting:
	case <-time.After(time.Minute):
		t.Fatal("WaitShareLockFile never waited")
	}
}

func TestWaitShareLockFile(t *testing.T) {
	t.Run("waits for the exclusive holder", func(t *testing.T) {
		path := newLockTarget(t)
		release, ok, err := TryLockFile(path)
		if err != nil || !ok {
			t.Fatalf("TryLockFile = %v, %v", ok, err)
		}
		waiting := make(chan struct{})
		got := waitShare(context.Background(), path, func() { close(waiting) })
		requireWaiting(t, waiting)
		release()
		r := receiveShare(t, got)
		if r.err != nil || !r.ok {
			t.Fatalf("WaitShareLockFile = %v, %v; want the lock", r.ok, r.err)
		}
		defer r.release()
		if held, err := LockHeld(path); err != nil || held {
			t.Fatalf("LockHeld = %v, %v; a shared holder is no exclusive one", held, err)
		}
	})
	t.Run("a write deadline already passed", func(t *testing.T) {
		path := newLockTarget(t)
		release, ok, err := TryLockFile(path)
		if err != nil || !ok {
			t.Fatalf("TryLockFile = %v, %v", ok, err)
		}
		defer release()
		ctx := WithWriteDeadline(context.Background(), time.Now().Add(-time.Second))
		r := receiveShare(t, waitShare(ctx, path, func() { t.Error("WaitShareLockFile waited past its write deadline") }))
		if r.err != nil || r.ok {
			t.Fatalf("WaitShareLockFile = %v, %v; want no lock and no error", r.ok, r.err)
		}
	})
	t.Run("an abandoned wait keeps nothing", func(t *testing.T) {
		path := newLockTarget(t)
		release, ok, err := TryLockFile(path)
		if err != nil || !ok {
			t.Fatalf("TryLockFile = %v, %v", ok, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		waiting := make(chan struct{})
		got := waitShare(ctx, path, func() { close(waiting) })
		requireWaiting(t, waiting)
		cancel()
		if r := receiveShare(t, got); !errors.Is(r.err, context.Canceled) || r.ok {
			t.Fatalf("WaitShareLockFile = %v, %v; want context.Canceled", r.ok, r.err)
		}
		release()
		deadline := time.Now().Add(time.Minute)
		for {
			release, ok, err := TryLockFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				release()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the abandoned wait kept the lock")
			}
			time.Sleep(time.Millisecond)
		}
	})
	t.Run("a missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		if _, _, err := WaitShareLockFile(context.Background(), path, nil); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("WaitShareLockFile = %v; want fs.ErrNotExist", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat = %v; the file was created", err)
		}
	})
}

func TestSharedHoldersDoNotBlockRemoval(t *testing.T) {
	removeWhileHeld := func(t *testing.T, path string, release func()) {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove while a shared holder has the file: %v", err)
		}
		release()
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat after release = %v; want fs.ErrNotExist", err)
		}
	}
	t.Run("a probe's or shared holder's handle", func(t *testing.T) {
		path := newLockTarget(t)
		release, ok, err := TryShareLockFile(path)
		if err != nil || !ok {
			t.Fatalf("TryShareLockFile = %v, %v", ok, err)
		}
		removeWhileHeld(t, path, release)
	})
	t.Run("a waiter that got the lock", func(t *testing.T) {
		path := newLockTarget(t)
		release, ok, err := TryLockFile(path)
		if err != nil || !ok {
			t.Fatalf("TryLockFile = %v, %v", ok, err)
		}
		waiting := make(chan struct{})
		got := waitShare(context.Background(), path, func() { close(waiting) })
		requireWaiting(t, waiting)
		release()
		r := receiveShare(t, got)
		if r.err != nil || !r.ok {
			t.Fatalf("WaitShareLockFile = %v, %v; want the lock", r.ok, r.err)
		}
		removeWhileHeld(t, path, r.release)
	})
}
