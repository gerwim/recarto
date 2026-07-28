package syncserver

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// reconcileSyncIndex gives every memory and tombstone in the store a sync_index
// entry, assigning fresh seqs to any that lack one.
//
// It exists because sync_index is server-only bookkeeping that store.Repair does
// not (and should not) salvage: it is absent from the client migration set, so a
// rebuilt store has an empty one, and a PARTIALLY salvaged index would be worse
// than an empty one — a row missing from it silently disappears from every
// future pull, forever. Rebuilding is instead self-healing regardless of how the
// index came to be incomplete.
//
// Fresh seqs are safe precisely because the seq counter itself lives in the
// `counters` table (scope="sync", metric="seq") and IS salvaged: reindexed
// entries therefore get seqs above every client's stored cursor, so existing
// clients re-pull them exactly once and memory.SyncApply merges them
// idempotently. A brand-new client (since=0) sees them like anything else.
//
// Tombstones are indexed before memories: if an id somehow appears in both
// tables, the memories pass's NOT IN then skips it, so the delete wins. Both
// passes order by timestamp so assigned seqs approximate real history.
func reconcileSyncIndex(ctx context.Context, st *store.Store) (int, error) {
	n := 0
	err := st.WriteTx(ctx, func(c store.Conn) error {
		type entry struct {
			id string
			at int64
		}
		// Each pass reads its whole result set before writing: the rows cursor
		// holds this connection, and the bumps below run on the same one.
		collect := func(query string) ([]entry, error) {
			rows, err := c.QueryContext(ctx, query)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []entry
			for rows.Next() {
				var e entry
				if err := rows.Scan(&e.id, &e.at); err != nil {
					return nil, err
				}
				out = append(out, e)
			}
			return out, rows.Err()
		}

		tombs, err := collect(
			`SELECT id, deleted_at FROM memory_tombstones
			  WHERE id NOT IN (SELECT memory_id FROM sync_index)
			  ORDER BY deleted_at, id`)
		if err != nil {
			return err
		}
		// Every status, not just active: the index tracks each row the server
		// holds, and a client must learn about a supersede or archive too.
		mems, err := collect(
			`SELECT id, updated_at FROM memories
			  WHERE id NOT IN (SELECT memory_id FROM sync_index)
			  ORDER BY updated_at, id`)
		if err != nil {
			return err
		}

		for _, e := range tombs {
			if _, err := bumpSyncIndexTx(ctx, c, e.id, true, e.at); err != nil {
				return err
			}
			n++
		}
		for _, e := range mems {
			if _, err := bumpSyncIndexTx(ctx, c, e.id, false, e.at); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
