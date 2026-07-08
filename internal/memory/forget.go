package memory

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Forget hard-deletes a memory by id: the row, its refs, and its lexical index
// entry are removed (reusing deleteMemoryRows, the same teardown gcSuperseded
// uses). Unlike complete_todo it is not status-gated — any memory recorded in
// error can be removed. reason is advisory (for the caller's intent) and is not
// persisted, since the row is gone. Returns (false, nil) when id is unknown.
//
// Forget also records a memory_tombstones row (pushed=false) so this explicit
// delete propagates to remote sync peers on the next push — a bare row
// deletion would otherwise leave nothing on the wire to signal it. Compaction's
// own row deletions (superseded/archive GC) deliberately do NOT do this: only
// an explicit Forget is a delete worth telling other machines about.
func (e *Engine) Forget(ctx context.Context, id, reason string) (bool, error) {
	_ = reason // advisory only; nothing to persist on a deleted row
	var found bool
	now := e.clk.Now().UnixMilli()
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		var got string
		err := c.QueryRowContext(ctx, `SELECT id FROM memories WHERE id=?`, id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if err := e.deleteMemoryRows(ctx, c, []string{id}); err != nil {
			return err
		}
		return e.upsertTombstone(ctx, c, id, now, false)
	})
	if err != nil {
		return false, err
	}
	if found {
		_ = e.led.Incr(ctx, "lifetime", "memories_forgotten", 1)
	}
	return found, nil
}
