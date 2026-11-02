package graph

import (
	"context"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// pruneBatchFiles is how many files each prune transaction tears down. Batched
// commits keep the write lock short and make the prune resumable: a worktree
// holds a full graph copy (easily 100k+ rows), and a single transaction would
// both stall readers for its whole duration and lose everything to one
// cancellation — the same trap the per-index rebuild fix closed.
const pruneBatchFiles = 256

// Worktrees returns the distinct worktrees with indexed files in this store.
func (e *Engine) Worktrees(ctx context.Context) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT DISTINCT worktree FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var wt string
		if err := rows.Scan(&wt); err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

// PruneWorktree removes every trace of worktree from the store: its files
// (cascading nodes, edges, and edge candidates), the FTS rows FK constraints
// cannot reach, and its meta keys (freshness token, pending marker). Deletion
// is batched and resumable — an interrupted prune leaves a smaller worktree,
// and the next prune (or this one re-run) finishes the job.
func (e *Engine) PruneWorktree(ctx context.Context, worktree string) error {
	for {
		ids, err := e.fileIDBatch(ctx, worktree)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		args := make([]any, len(ids))
		ph := make([]string, len(ids))
		for i, id := range ids {
			args[i], ph[i] = id, "?"
		}
		in := strings.Join(ph, ",")
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			for _, fts := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
				if _, err := c.ExecContext(ctx, `DELETE FROM `+fts+` WHERE rowid IN (
					SELECT fts_rowid FROM node_fts_rows
					WHERE node_id IN (SELECT id FROM nodes WHERE file_id IN (`+in+`)))`, args...); err != nil {
					return err
				}
			}
			_, err := c.ExecContext(ctx, `DELETE FROM files WHERE id IN (`+in+`)`, args...)
			return err
		}); err != nil {
			return err
		}
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?)`,
			graphTokenKey+worktree, resolvePendingKey(worktree))
		return err
	})
}

// fileIDBatch returns up to pruneBatchFiles file ids of worktree.
func (e *Engine) fileIDBatch(ctx context.Context, worktree string) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM files WHERE worktree=? LIMIT ?`, worktree, pruneBatchFiles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
