package memory

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// AllRecords returns every stored memory (any status), with refs, ordered by
// id. Unlike List (which defaults to status='active'), this is for the
// full-history CLI export (recarto export): archived/done/superseded rows are
// part of a machine's exported history too, not just its currently-recalled
// set.
func (e *Engine) AllRecords(ctx context.Context) ([]*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx, selectMemoriesSQL+` ORDER BY id`)
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

// AllTombstones returns every recorded delete tombstone this store knows
// about, regardless of pushed, as wire records. Unlike PendingTombstones
// (which filters to pushed=0, the local push queue), the full-history export
// must ship every tombstone — including ones already confirmed pushed by
// ordinary sync — so a bulk-imported archive can delete rows a fresh server
// bucket never saw tombstoned.
func (e *Engine) AllTombstones(ctx context.Context) ([]syncwire.Record, error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT id, deleted_at FROM memory_tombstones`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []syncwire.Record
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out = append(out, syncwire.Record{ID: id, Deleted: true, UpdatedAt: at})
	}
	return out, rows.Err()
}
