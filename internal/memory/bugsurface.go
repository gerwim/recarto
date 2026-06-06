package memory

import (
	"context"
)

// BugsForCode returns active kind=bug memories that reference any of the given
// files or symbols, branch-visible (branch-scoped or repo-global), most-recently-
// updated first. It powers just-in-time bug surfacing in the read/edit/grep hooks:
// a bug recorded against a file/symbol reappears the moment Claude touches it.
func (e *Engine) BugsForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
	return e.memoriesForCodeByKinds(ctx, branch, []Kind{KindBug}, files, symbols)
}

// RecentBugs returns up to limit active, branch-visible bug memories, most recent
// first — the ambient "bugs we've bitten on" tier for session start.
func (e *Engine) RecentBugs(ctx context.Context, branch string, limit int) ([]*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		selectMemoriesSQL+`
		  WHERE status='active' AND kind='bug'
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	return recs, err
}
