package memory

import (
	"context"
	"sort"
	"strings"
)

// memoriesForCodeByKinds returns active, branch-visible memories of any of the
// given kinds that reference any of the given files or symbols, most-recently-
// updated first. It is the shared engine of the just-in-time surfacing helpers
// (BugsForCode, TodosForCode, GuidanceForCode): a memory anchored to a file/symbol
// reappears the moment that code is touched.
func (e *Engine) memoriesForCodeByKinds(ctx context.Context, branch string, kinds []Kind, files, symbols []string) ([]*Record, error) {
	type qref struct{ t, v string }
	var qrefs []qref
	for _, f := range files {
		qrefs = append(qrefs, qref{string(RefFile), f})
	}
	for _, s := range symbols {
		qrefs = append(qrefs, qref{string(RefSymbol), s})
	}
	if len(qrefs) == 0 || len(kinds) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(kinds))
	for i := range kinds {
		placeholders[i] = "?"
	}
	idset := map[string]bool{}
	for _, qr := range qrefs {
		args := []any{qr.t, qr.v}
		for _, k := range kinds {
			args = append(args, string(k))
		}
		args = append(args, branch)
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT m.id FROM memory_refs r
			   JOIN memories m ON m.id = r.memory_id
			  WHERE r.ref_type=? AND r.ref_value=?
			    AND m.kind IN (`+strings.Join(placeholders, ",")+`) AND m.status='active'
			    AND (m.branch IS NULL OR m.branch=?)`,
			args...)
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

// GuidanceForCode returns active do_not_repeat (guardrail) and decision (rationale)
// memories anchored to any of the given files or symbols, branch-visible, most-
// recently-updated first. It powers the high-precision just-in-time guidance tier
// in the read/edit hooks: the moment the agent touches a file carrying a "don't do
// this" or "we settled on this" memory, that guidance reappears — so prior
// decisions are respected and prior mistakes are not repeated. Bug and todo kinds
// have their own tiers; learning/preference belong to recall, so they are excluded
// here to keep the tier lean.
func (e *Engine) GuidanceForCode(ctx context.Context, branch string, files, symbols []string) ([]*Record, error) {
	return e.memoriesForCodeByKinds(ctx, branch, []Kind{KindDoNotRepeat, KindDecision}, files, symbols)
}
