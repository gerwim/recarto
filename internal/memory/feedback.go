package memory

import (
	"context"
	"database/sql"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// ResolveFeedback marks an active feedback report resolved (active→resolved),
// optionally recording a resolution note in Extra. It mirrors CompleteTodo for the
// feedback kind: safe to call repeatedly, returning (false, nil) when no active
// feedback with that id exists. The row is dropped from the lexical index so a
// resolved report stops surfacing as a recall candidate.
func (e *Engine) ResolveFeedback(ctx context.Context, id, note string) (bool, error) {
	var done bool
	now := e.clk.Now().UnixMilli()
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		var res sql.Result
		var err error
		if note != "" {
			res, err = c.ExecContext(ctx,
				`UPDATE memories
				    SET status='resolved', updated_at=?,
				        extra=json_set(extra, '$.resolution_note', ?)
				  WHERE id=? AND kind='feedback' AND status='active'`,
				now, note, id)
		} else {
			res, err = c.ExecContext(ctx,
				`UPDATE memories SET status='resolved', updated_at=?
				  WHERE id=? AND kind='feedback' AND status='active'`,
				now, id)
		}
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
		if err := e.lex.Delete(ctx, c, id); err != nil {
			return err
		}
		return e.markDirty(ctx, c, id, now)
	})
	return done, err
}
