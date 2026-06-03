package memory

import (
	"context"
	"sort"
)

// BugsForCode returns active kind=bug memories that reference any of the given
// files or symbols, branch-visible (branch-scoped or repo-global), most-recently-
// updated first. It powers just-in-time bug surfacing in the read/edit/grep hooks:
// a bug recorded against a file/symbol reappears the moment Claude touches it.
func (e *Engine) BugsForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
	type qref struct{ t, v string }
	var qrefs []qref
	for _, f := range files {
		qrefs = append(qrefs, qref{string(RefFile), f})
	}
	for _, s := range symbols {
		qrefs = append(qrefs, qref{string(RefSymbol), s})
	}
	if len(qrefs) == 0 {
		return nil, nil
	}
	idset := map[string]bool{}
	for _, qr := range qrefs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT m.id FROM memory_refs r
			   JOIN memories m ON m.id = r.memory_id
			  WHERE r.ref_type=? AND r.ref_value=?
			    AND m.kind='bug' AND m.status='active'
			    AND (m.branch IS NULL OR m.branch=?)`,
			qr.t, qr.v, branch)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			idset[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	recs, err := e.loadByIDs(ctx, idset)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].UpdatedAt > recs[j].UpdatedAt })
	return recs, nil
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
