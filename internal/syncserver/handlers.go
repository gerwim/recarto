package syncserver

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
	"github.com/gerwim/recarto/internal/syncwire"
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

	// One line per pull so an operator watching the container logs can see each
	// client polling, which store it asked for, and whether anything moved.
	log.Printf("sync: pull %s since=%d → %d records (cursor=%d more=%v)",
		r.PathValue("key"), since, len(records), cursor, more)
	writeJSON(w, syncwire.ChangesResponse{Records: records, Cursor: cursor, More: more})
}

// applyRecord merges rec into rs via memory.SyncApplyTx (the same merge core
// the client uses to apply what it pulls — the server is a headless recarto
// store), bumping sync_index/seq in the SAME write transaction as the merge
// via the afterApply hook — and ONLY when the merge produced a genuine change
// (accepted=true), so an idempotent re-application (or a record a
// resurrection guard drops) never grows the server's change log, and a crash
// between "apply" and "index" can never happen (they are one commit). Shared
// by handlePostChanges (one repo per request) and handleImport (many repos
// fanned out from one bulk archive) — both are, at bottom, "apply one wire
// record to a repoStore and index it if it changed".
func (s *Server) applyRecord(ctx context.Context, rs *repoStore, rec syncwire.Record) (accepted bool, err error) {
	_, _, err = rs.eng.SyncApplyTx(ctx, rec, func(c store.Conn, outcome memory.SyncOutcome, canonID string) error {
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
		accepted = true
		return nil
	})
	return accepted, err
}

// handlePostChanges answers POST /v1/repos/{key}/changes: applies each
// incoming record via applyRecord and reports how many produced a genuine
// change.
func (s *Server) handlePostChanges(w http.ResponseWriter, r *http.Request) {
	rs, err := s.storeFor(r.PathValue("key"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := gzipRequestBody(w, r, maxPushBytes, maxDecompressedBytes)
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
		ok, err := s.applyRecord(ctx, rs, rec)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			accepted++
		}
	}

	var cursor int64
	_ = rs.st.DB.QueryRowContext(ctx,
		`SELECT COALESCE(value, 0) FROM counters WHERE scope='sync' AND metric='seq'`).Scan(&cursor)
	log.Printf("sync: push %s %d records, %d accepted (cursor=%d)",
		r.PathValue("key"), len(req.Records), accepted, cursor)
	writeJSON(w, syncwire.PushResponse{Cursor: cursor, Accepted: accepted})
}
