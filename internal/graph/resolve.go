package graph

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Resolve links unresolved reference edges to a defining node within the same
// worktree by matching dst_name against node name or qualified_name. Ambiguous
// names take the first match (LIMIT 1); dst_name is retained regardless, so
// trace_call_path still works by name where resolution is partial.
func (e *Engine) Resolve(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ?
    AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
  LIMIT 1)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree, worktree)
		return err
	})
}

// deleteFile removes a file's graph rows for the given worktree. nodes/edges
// cascade via FK; nodes_fts rows are dropped manually (FTS5 is unlinked).
func (e *Engine) deleteFile(ctx context.Context, worktree, path string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts WHERE node_id IN (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx,
			`DELETE FROM files WHERE worktree = ? AND path = ?`, worktree, path)
		return err
	})
}
