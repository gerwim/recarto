package memory

import (
	"context"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// CompleteTodo marks an active todo as done (a terminal status, distinct from the
// 'archived' status decay produces, so finished work is distinguishable from faded
// work). It is safe to call repeatedly: it returns (false, nil) when no active
// todo with that id exists. The row is dropped from the lexical index so a
// completed todo stops surfacing as a recall candidate (recall also filters by
// status, but this keeps the candidate pool clean — mirrors supersede in insert).
func (e *Engine) CompleteTodo(ctx context.Context, id string) (bool, error) {
	var done bool
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		res, err := c.ExecContext(ctx,
			`UPDATE memories SET status='done', updated_at=?
			   WHERE id=? AND kind='todo' AND status='active'`,
			e.clk.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		done = true
		return e.lex.Delete(ctx, c, id)
	})
	return done, err
}

// ActiveTodos returns up to limit active, branch-visible todos, most recently
// updated first — the open-work list for `recarto todo list` and a tier the
// session-start baseline also draws on.
func (e *Engine) ActiveTodos(ctx context.Context, branch string, limit int) ([]*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		selectMemoriesSQL+`
		  WHERE status='active' AND kind='todo'
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// TodosForCode returns active kind=todo memories referencing any of the given
// files or symbols, branch-visible, most-recently-updated first. It powers
// just-in-time todo surfacing in the read/edit hooks: a todo anchored to a
// file/symbol reappears the moment that code is touched (mirrors BugsForCode).
func (e *Engine) TodosForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
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
			    AND m.kind='todo' AND m.status='active'
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
