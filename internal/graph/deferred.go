package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gerwim/recarto/internal/ids"
	"github.com/gerwim/recarto/internal/parse"
	"github.com/gerwim/recarto/internal/store"
)

// deferredPath is the file whose presence marks that the post-edit hook left an
// edit of worktree to the process holding its index lock: the mark says a pass
// is needed, the edit records (see recordEdit) say whose edits a pass takes in.
// A file, not a store row: setting it must not wait on that process's SQLite
// write transaction.
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
// cover only files changed since the last. Only the mark drives the loop: the
// edit records decide whose session a pass's changes count in, and one dated
// in the future (a clock step) would keep the loop from ending.
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
		attempt := ids.New()
		// Outside git the drain's pending marker would hide an index an earlier version finished without the built mark.
		if !inGitRepo(ctx, worktree, attempt) {
			err = e.markBuiltIfFinished(ctx, worktree)
		}
		if err == nil {
			err = e.drainDeferred(ctx, worktree, inAttempt(NewSource(worktree), attempt))
		}
		release()
		if err != nil {
			return err
		}
	}
}

// pendingEdit is an edit the post-edit hook left to the process holding the
// worktree's index lock. Its change counts at At once an index reads the
// version it made.
type pendingEdit struct {
	Path string `json:"path"` // worktree-relative, slash-separated: files.path
	Hash string `json:"hash"` // sha256hex of the bytes the hook read
	At   int64  `json:"at"`   // the hook's clock, Unix ms
	name string // the record's file name in editsDir
}

// creditCase is how creditOf named the edit a version read counts for.
type creditCase int

const (
	madeByL  creditCase = iota // the last edit recorded before the read made the version
	madeByF                    // the first edit recorded after the read made it: its hook ran late
	afterL                     // an unreported change followed the last edit recorded before the read
	unnamed                    // no record names the version
)

// editsDir holds worktree's pending edit records, one file each.
func (e *Engine) editsDir(worktree string) string { return e.indexFileBase(worktree) + ".edits" }

// recordEdit records that the post-edit hook left the edit of path, whose
// bytes are src, to the index lock holder at time at. Only a file the graph
// indexes has a change to credit.
func (e *Engine) recordEdit(worktree, path string, src []byte, at int64) error {
	if _, ok := parse.LangForExt(path); !ok {
		return nil
	}
	dir := e.editsDir(worktree)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(pendingEdit{Path: path, Hash: sha256hex(src), At: at})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".edit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The rename publishes the record whole: a reader never sees half of it.
	return os.Rename(tmp.Name(), filepath.Join(dir, ids.New()+".json"))
}

// pendingEdits returns worktree's pending edit records by path, each path's
// sorted by (At, name).
func (e *Engine) pendingEdits(worktree string) (map[string][]pendingEdit, error) {
	dir := e.editsDir(worktree)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]pendingEdit{}
	for _, de := range entries {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue // a record still being written
		}
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue // another index took it in
		}
		if err != nil {
			return nil, err
		}
		var pe pendingEdit
		if json.Unmarshal(b, &pe) != nil || pe.Path == "" || pe.Hash == "" {
			// A record cut short (a power loss) can only cost its edit's credit:
			// the edit itself is still indexed through the file's changed stat.
			// Kept, it would fail every index.
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			continue
		}
		pe.name = name
		out[pe.Path] = append(out[pe.Path], pe)
	}
	for _, edits := range out {
		sort.Slice(edits, func(i, j int) bool {
			if edits[i].At != edits[j].At {
				return edits[i].At < edits[j].At
			}
			return edits[i].name < edits[j].name
		})
	}
	return out, nil
}

// creditOf names the pending edit whose session the version of a file with
// content hash hash, read at readAt, counts in. edits are the file's pending
// records in (At, name) order. It returns the records the version includes,
// to remove once its write commits, and the time the change counts at.
//
// L, the last edit recorded before the read, is in what was read; F, the one
// right after it, is the only later record that may still be in it, its hook
// having run after the read. An agent edits one change at a time, so every
// record after F is an edit made after the read, and matching stops at F even
// when a later edit restores the bytes read.
func creditOf(edits []pendingEdit, hash string, readAt int64) (done []pendingEdit, at int64, how creditCase) {
	l := -1
	for i, pe := range edits {
		if pe.At < readAt {
			l = i
		}
	}
	if l >= 0 && edits[l].Hash == hash {
		return edits[:l+1], edits[l].At, madeByL
	}
	if f := l + 1; f < len(edits) && edits[f].Hash == hash {
		return edits[:f+1], edits[f].At, madeByF
	}
	if l >= 0 {
		// A change no hook reported (a formatter) followed L; it counts with
		// the latest edit the version includes.
		return edits[:l+1], edits[l].At, afterL
	}
	return nil, 0, unnamed
}

// removeEdits removes worktree's records of edits.
func (e *Engine) removeEdits(worktree string, edits []pendingEdit) error {
	var errs []error
	for _, pe := range edits {
		err := os.Remove(filepath.Join(e.editsDir(worktree), pe.name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// settleEdits ends a successful Reconcile pass whose listing started at
// listedAt. It drops the records whose version the store already holds (a
// record that came after its version was written, or an edit that restored the
// stored bytes) and those this pass took in, having made the store match a
// version that includes them. Kept, either would credit a later, unrelated
// change to an old session.
func (e *Engine) settleEdits(ctx context.Context, worktree string, listedAt int64) error {
	edits, err := e.pendingEdits(worktree)
	if err != nil || len(edits) == 0 {
		return err
	}
	var settled []pendingEdit
	for path, pes := range edits {
		var stored string
		err := e.st.DB.QueryRowContext(ctx,
			`SELECT content_hash FROM files WHERE worktree=? AND path=?`, worktree, path).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, pe := range pes {
			if pe.Hash == stored || pe.At < listedAt {
				settled = append(settled, pe)
			}
		}
	}
	return e.removeEdits(worktree, settled)
}
