package graph

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
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
	if err := e.Resolve(ctx, worktree); err != nil {
		return err
	}
	// Semantic tier (no-op unless an embedder is set): bring the vector index up
	// to date. Best-effort — a model error must not fail reconciliation, since
	// the lexical graph is already written and fully usable on its own.
	if e.emb != nil {
		_ = e.embedWorktree(ctx, worktree)
	}
	return nil
}

// ReconcileIfStale reconciles only when the worktree's git token differs from the
// token stored at the last reconcile. An empty token (non-git worktree) always
// reconciles. On a successful reconcile it records the new token. This makes
// query-time freshness cheap: an unchanged worktree skips the file walk entirely.
//
// The git token (see WorktreeToken) folds in the mtime+size of every dirty file,
// making it exactly as precise as Reconcile's own mtime/size pre-check: even
// re-editing an ALREADY-dirty file (whose porcelain line stays "M <path>") bumps
// the token and triggers a reconcile. The only residual gap is a re-edit that
// leaves both mtime and size byte-for-byte identical, which a real write does not
// produce.
func (e *Engine) ReconcileIfStale(ctx context.Context, worktree string, src Source, token string) error {
	const keyPrefix = "graph_token:"
	if token != "" {
		var stored string
		_ = e.st.DB.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key=?`, keyPrefix+worktree).Scan(&stored)
		if stored == token {
			return nil
		}
	}
	if err := e.Reconcile(ctx, worktree, src); err != nil {
		return err
	}
	if token != "" {
		return e.st.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				keyPrefix+worktree, token)
			return err
		})
	}
	return nil
}
