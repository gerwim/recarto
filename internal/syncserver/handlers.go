package syncserver

import (
	"encoding/json"
	"net/http"
	"strconv"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// defaultChangesLimit / maxChangesLimit bound one GET /changes page.
const (
	defaultChangesLimit = 500
	maxChangesLimit     = 2000
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// handleGetChanges answers GET /v1/repos/{key}/changes?since=&limit=: records
// with sync_index.seq > since, oldest-first, up to limit (+1 to detect `more`
// without a second query).
func (s *Server) handleGetChanges(w http.ResponseWriter, r *http.Request) {
	rs, err := s.storeFor(r.PathValue("key"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit := defaultChangesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxChangesLimit {
		limit = maxChangesLimit
	}

	ctx := r.Context()
	rows, err := rs.st.DB.QueryContext(ctx,
		`SELECT memory_id, seq, deleted, updated_at FROM sync_index
		  WHERE seq > ? ORDER BY seq LIMIT ?`, since, limit+1)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type idxRow struct {
		id      string
		seq     int64
		deleted bool
		at      int64
	}
	var idx []idxRow
	for rows.Next() {
		var ir idxRow
		var delInt int
		if err := rows.Scan(&ir.id, &ir.seq, &delInt, &ir.at); err != nil {
			rows.Close()
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ir.deleted = delInt != 0
		idx = append(idx, ir)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows.Close()

	more := len(idx) > limit
	if more {
		idx = idx[:limit]
	}

	cursor := since
	records := make([]syncwire.Record, 0, len(idx))
	for _, ir := range idx {
		cursor = ir.seq
		if ir.deleted {
			records = append(records, syncwire.Record{ID: ir.id, Deleted: true, UpdatedAt: ir.at})
			continue
		}
		rec, err := rs.eng.GetByID(ctx, ir.id)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rec == nil {
			// Indexed but the row is gone without a tombstone (should not happen in
			// practice — every delete path goes through SyncApply's tombstone
			// branch). Skip rather than fail the whole page.
			continue
		}
		records = append(records, memory.ToWireRecord(rec))
	}

	writeJSON(w, syncwire.ChangesResponse{Records: records, Cursor: cursor, More: more})
}

// handlePostChanges answers POST /v1/repos/{key}/changes: applies each
// incoming record through memory.SyncApplyTx (the same merge core the client
// uses to apply what it pulls — the server is a headless recarto store),
// bumping sync_index/seq in the SAME write transaction as the merge, via the
// afterApply hook — and ONLY for records that produced a genuine change, so
// an idempotent re-push (or a record a resurrection guard drops) never grows
// the server's change log, and a crash between "apply" and "index" can never
// happen (they are one commit).
func (s *Server) handlePostChanges(w http.ResponseWriter, r *http.Request) {
	rs, err := s.storeFor(r.PathValue("key"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := gzipRequestBody(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	defer body.Close()
	var req syncwire.PushRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}

	ctx := r.Context()
	accepted := 0
	for _, rec := range req.Records {
		rec := rec
		indexed := false
		_, _, err := rs.eng.SyncApplyTx(ctx, rec, func(c store.Conn, outcome memory.SyncOutcome, canonID string) error {
			updatedAt := rec.UpdatedAt
			if !rec.Deleted {
				// The merge (LWW / reinforce) may have adopted a different
				// updated_at than the incoming record; read it back on the SAME
				// connection/transaction so the index reflects what was actually
				// committed.
				var ua int64
				if qerr := c.QueryRowContext(ctx, `SELECT updated_at FROM memories WHERE id=?`, canonID).Scan(&ua); qerr == nil {
					updatedAt = ua
				}
			}
			if _, err := bumpSyncIndexTx(ctx, c, canonID, rec.Deleted, updatedAt); err != nil {
				return err
			}
			indexed = true
			return nil
		})
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if indexed {
			accepted++
		}
	}

	var cursor int64
	_ = rs.st.DB.QueryRowContext(ctx,
		`SELECT COALESCE(value, 0) FROM counters WHERE scope='sync' AND metric='seq'`).Scan(&cursor)
	writeJSON(w, syncwire.PushResponse{Cursor: cursor, Accepted: accepted})
}
