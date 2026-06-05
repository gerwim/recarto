package app

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// graphRefResolver answers memory.RefResolver from the code graph (files/nodes)
// in the shared store, scoped to a worktree. When the worktree has no graph rows
// it reports nothing as dangling, so recall degrades to its pre-resolvability
// behavior (spec §5.5).
type graphRefResolver struct{ st *store.Store }

func (g graphRefResolver) Dangling(ctx context.Context, worktree string, refs []memory.Ref) ([]memory.Ref, error) {
	var n int
	if err := g.st.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM files WHERE worktree=?`, worktree).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil // ungraphed worktree → nothing dangles
	}
	var dangling []memory.Ref
	for _, r := range refs {
		present, err := g.present(ctx, worktree, r)
		if err != nil {
			return nil, err
		}
		if !present {
			dangling = append(dangling, r)
		}
	}
	return dangling, nil
}

func (g graphRefResolver) present(ctx context.Context, worktree string, r memory.Ref) (bool, error) {
	var q string
	var args []any
	switch r.Type {
	case memory.RefFile:
		q = `SELECT 1 FROM files WHERE worktree=? AND path=? LIMIT 1`
		args = []any{worktree, r.Value}
	case memory.RefSymbol:
		q = `SELECT 1 FROM nodes n JOIN files f ON n.file_id=f.id
		      WHERE f.worktree=? AND (n.name=? OR n.qualified_name=?) LIMIT 1`
		args = []any{worktree, r.Value, r.Value}
	default:
		return true, nil // not code
	}
	var x int
	err := g.st.DB.QueryRowContext(ctx, q, args...).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
