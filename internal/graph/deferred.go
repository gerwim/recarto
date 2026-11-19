package graph

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/gerwim/recarto/internal/ids"
	"github.com/gerwim/recarto/internal/store"
)

// deferredPath is the file whose presence marks that the post-edit hook left an
// edit of worktree to the process holding its index lock. A file, not a store
// row: setting it must not wait on that process's SQLite write transaction.
func (e *Engine) deferredPath(worktree string) string { return e.indexFileBase(worktree) + ".deferred" }

// markDeferred sets worktree's deferred-edit mark.
func (e *Engine) markDeferred(worktree string) error {
	return os.WriteFile(e.deferredPath(worktree), nil, 0o644)
}

// deferred reports whether worktree's deferred-edit mark is set.
func (e *Engine) deferred(worktree string) (bool, error) {
	_, err := os.Stat(e.deferredPath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// clearDeferred removes the mark. Only a lock holder clears it, and only after
// seeing it, so the file exists.
func (e *Engine) clearDeferred(worktree string) error {
	return os.Remove(e.deferredPath(worktree))
}

// drainDeferred takes in the edits left to the lock holder, which must hold
// worktree's index lock: while the mark is set it clears it and reconciles from
// src. Clearing before the pass means an edit deferred during the pass marks
// again and gets its own pass; the mtime and size pre-check makes each pass
// cover only files changed since the last.
func (e *Engine) drainDeferred(ctx context.Context, worktree string, src Source) error {
	for {
		set, err := e.deferred(worktree)
		if err != nil || !set {
			return err
		}
		if err := e.clearDeferred(worktree); err != nil {
			return err
		}
		if err := e.Reconcile(ctx, worktree, src); err != nil {
			return err
		}
	}
}

// takeDeferred runs after a process releases worktree's index lock, and after
// the post-edit hook sets the mark. A mark set while the lock was held but after
// the holder's last check would otherwise wait for the next index. While the
// mark is set and the lock is free, it locks without waiting and drains. When
// another process holds the lock, it returns, because that process drains
// before it releases and checks again after.
func (e *Engine) takeDeferred(ctx context.Context, worktree string) error {
	for {
		set, err := e.deferred(worktree)
		if err != nil || !set {
			return err
		}
		release, ok, err := store.TryLockFile(e.indexLockPath(worktree))
		if err != nil || !ok {
			return err
		}
		err = e.drainDeferred(ctx, worktree, inAttempt(NewSource(worktree), ids.New()))
		release()
		if err != nil {
			return err
		}
	}
}
