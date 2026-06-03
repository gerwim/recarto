package graph

import (
	"context"
)

// Reconcile makes the worktree's graph match src: it (re)indexes new and changed
// files (mtime/size pre-check, then content-hash confirm), deletes removed files,
// and resolves name references. Unchanged files are skipped cheaply.
func (e *Engine) Reconcile(ctx context.Context, worktree string, src Source) error {
	entries, err := src.List()
	if err != nil {
		return err
	}

	// Load current index state for this worktree.
	type rec struct {
		hash        string
		mtime, size int64
	}
	have := map[string]rec{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, content_hash, mtime, size FROM files WHERE worktree=?`, worktree)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		var r rec
		if err := rows.Scan(&p, &r.hash, &r.mtime, &r.size); err != nil {
			rows.Close()
			return err
		}
		have[p] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	seen := map[string]bool{}
	for _, fe := range entries {
		seen[fe.Path] = true
		prev, known := have[fe.Path]
		if known && prev.mtime == fe.Mtime && prev.size == fe.Size {
			continue // cheap pre-check: unchanged
		}
		content, err := src.Read(fe.Path)
		if err != nil {
			return err
		}
		if known && prev.hash == sha256hex(content) {
			continue // mtime moved but content identical
		}
		if err := e.IndexFile(ctx, worktree, fe.Path, content, fe.Mtime, fe.Size); err != nil {
			return err
		}
	}

	// Delete files that disappeared (CASCADE removes their nodes/edges; FTS rows
	// are dropped explicitly since FTS5 is not linked).
	for p := range have {
		if seen[p] {
			continue
		}
		if err := e.deleteFile(ctx, worktree, p); err != nil {
			return err
		}
	}
	return e.Resolve(ctx, worktree)
}
