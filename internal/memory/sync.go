package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// SyncOutcome is the result of merging one wire record via SyncApply.
type SyncOutcome int

const (
	// SyncNoop means the record produced no observable change: an idempotent
	// re-application, or a resurrection attempt dropped by the tombstone guard.
	// Callers (the sync server in particular) must NOT advance any cursor/seq
	// for a Noop, or a repeated no-op push/pull would grow forever.
	SyncNoop SyncOutcome = iota
	SyncInserted
	SyncUpdated
	SyncReinforced
	SyncTombstoned
)

// SyncApply merges one wire record (from a pull or a push) into the store and
// returns the outcome plus the id of the CANONICAL affected row. The canonical
// id differs from rec.ID exactly when rec has no local id match but reinforces
// an existing near-duplicate (SyncReinforced) — callers must index/report the
// existing id, not rec.ID, since rec.ID may never exist as a row.
//
// Semantics, in priority order (mirrors ImportMemory's insert-or-reinforce
// convergence, extended with update-by-id and tombstones for sync):
//
//  1. rec.Deleted (tombstone): delete the local row if one exists and is not
//     newer than the tombstone, then record/extend the tombstone.
//  2. A tombstone already covers rec.ID at >= rec.UpdatedAt: drop rec — an
//     older non-deleted record must never resurrect a later delete.
//  3. rec.ID exists locally: LWW-overwrite content when rec is newer, else
//     keep local content; either way MAX the soft signals (confidence,
//     reinforcement_count, pinned) and union refs, never regressing them.
//  4. rec.ID absent but a near-duplicate exists: reinforce it (converges
//     independently-authored copies), MAX-ing pinned/confidence.
//  5. Otherwise: insert rec verbatim, preserving its original timestamps.
//
// SyncApply never touches memory_sync_outbox: it is the READ side of sync
// (applying something already known elsewhere), so it must never mark the
// merged row dirty for push — that would echo a just-pulled record straight
// back out. Only local authoring paths (Record, ImportMemory, dedup merge)
// call markDirty.
func (e *Engine) SyncApply(ctx context.Context, rec syncwire.Record) (SyncOutcome, string, error) {
	return e.SyncApplyTx(ctx, rec, nil)
}

// SyncApplyTx is SyncApply, but when the merge produces a genuine change
// (outcome != SyncNoop) it runs afterApply — with the canonical outcome/id —
// in the SAME write transaction before committing. This lets a caller couple
// an atomic side effect to the merge: the sync server uses it to bump its
// per-repo seq/sync_index bookkeeping in the identical transaction as the
// memory change, so a crash or error between "apply the change" and "index it
// for future pulls" is impossible — both commit together, or neither does.
func (e *Engine) SyncApplyTx(ctx context.Context, rec syncwire.Record, afterApply func(c store.Conn, outcome SyncOutcome, canonID string) error) (SyncOutcome, string, error) {
	if !rec.Deleted && !Kind(rec.Kind).Valid() {
		return SyncNoop, "", errInvalidKind
	}
	now := e.clk.Now().UnixMilli()
	outcome := SyncNoop
	canonID := rec.ID

	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		tombAt, hasTomb, err := e.getTombstone(ctx, c, rec.ID)
		if err != nil {
			return err
		}

		switch {
		case rec.Deleted:
			outcome, err = e.applyTombstoneRow(ctx, c, rec, tombAt, hasTomb)
		case hasTomb && tombAt >= rec.UpdatedAt:
			// A later (or equal) delete already wins; do not resurrect.
			outcome = SyncNoop
		default:
			var local localRow
			serr := c.QueryRowContext(ctx,
				`SELECT title, body, updated_at, confidence, reinforcement_count, pinned, status
				   FROM memories WHERE id=?`, rec.ID).
				Scan(&local.title, &local.body, &local.updatedAt, &local.confidence,
					&local.reinforcementCount, &local.pinned, &local.status)
			switch {
			case serr == nil:
				outcome, err = e.syncApplyExisting(ctx, c, rec, local)
			case errors.Is(serr, sql.ErrNoRows):
				outcome, canonID, err = e.syncApplyAbsent(ctx, c, rec, now)
			default:
				err = serr
			}
		}
		if err != nil {
			return err
		}
		if outcome != SyncNoop && afterApply != nil {
			return afterApply(c, outcome, canonID)
		}
		return nil
	})
	if err != nil {
		return SyncNoop, "", err
	}
	return outcome, canonID, nil
}

// localRow is the subset of an existing memories row SyncApply needs to decide
// LWW-overwrite vs. keep-local-but-merge-soft-signals.
type localRow struct {
	title, body        string
	updatedAt          int64
	confidence         float64
	reinforcementCount int
	pinned             bool
	status             string
}

// syncApplyExisting merges rec into the local row at rec.ID (already
// confirmed to exist). Refs are always unioned; confidence/reinforcement/pinned
// always take the MAX of local and incoming (soft signals never regress).
// Content (title/body/branch/status/superseded_by + derived search text and
// fingerprints) is overwritten only when rec is strictly newer (LWW).
func (e *Engine) syncApplyExisting(ctx context.Context, c store.Conn, rec syncwire.Record, local localRow) (SyncOutcome, error) {
	changed := false
	refsChanged := false

	newConfidence := local.confidence
	if c2 := clampConfidence(rec.Confidence); c2 > newConfidence {
		newConfidence, changed = c2, true
	}
	newReinforcement := local.reinforcementCount
	if rec.ReinforcementCount > newReinforcement {
		newReinforcement, changed = rec.ReinforcementCount, true
	}
	newPinned := local.pinned
	if rec.Pinned && !newPinned {
		newPinned, changed = true, true
	}

	for _, r := range rec.Refs {
		res, err := c.ExecContext(ctx,
			`INSERT OR IGNORE INTO memory_refs(memory_id, ref_type, ref_value) VALUES (?,?,?)`,
			rec.ID, r.Type, r.Value)
		if err != nil {
			return SyncNoop, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed, refsChanged = true, true
		}
	}

	if rec.UpdatedAt > local.updatedAt {
		// search_text must reflect the UNIONED ref set (local ∪ incoming, just
		// written above), not merely rec.Refs — otherwise a newer remote update
		// that omits a ref the local row already had would silently drop that
		// ref from lexical recall/FTS even though memory_refs still lists it.
		unionedRefs, err := e.loadRefsTx(ctx, c, rec.ID)
		if err != nil {
			return SyncNoop, err
		}
		searchText := BuildSearchText(rec.Title, rec.Body, unionedRefs)
		set := shingleSet(rec.Body)
		bands := bandHashes(minHashSignature(set))
		sim := simHash64(set)
		status := rec.Status
		if status == "" {
			status = "active"
		}
		extraJSON, err := marshalExtra(rec.Extra)
		if err != nil {
			return SyncNoop, err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE memories
			    SET title=?, body=?, search_text=?, status=?, superseded_by=?, branch=?,
			        confidence=?, reinforcement_count=?, pinned=?, simhash=?, updated_at=?, extra=?
			  WHERE id=?`,
			rec.Title, rec.Body, searchText, status, nullStr(rec.SupersededBy), nullStr(rec.Branch),
			newConfidence, newReinforcement, boolToInt(newPinned), int64(sim), rec.UpdatedAt, extraJSON, rec.ID,
		); err != nil {
			return SyncNoop, err
		}
		if err := e.rewriteMinhash(ctx, c, rec.ID, bands); err != nil {
			return SyncNoop, err
		}
		// A terminal incoming status (done/resolved/superseded) must drop out of
		// the lexical index exactly like the local completion paths
		// (CompleteTodo/ResolveFeedback/an explicit supersede) already do —
		// otherwise a remote completion/resolution/supersede pulled in here
		// would keep crowding active memories out of lexical recall's top-N FTS
		// candidates (recall filters by status too, but only after FTS ranking).
		if err := e.lex.Delete(ctx, c, rec.ID); err != nil {
			return SyncNoop, err
		}
		if status == "active" {
			if err := e.lex.Index(ctx, c, rec.ID, searchText); err != nil {
				return SyncNoop, err
			}
		}
		return SyncUpdated, nil
	}

	if !changed {
		return SyncNoop, nil
	}
	if _, err := c.ExecContext(ctx,
		`UPDATE memories SET confidence=?, reinforcement_count=?, pinned=? WHERE id=?`,
		newConfidence, newReinforcement, boolToInt(newPinned), rec.ID); err != nil {
		return SyncNoop, err
	}
	// The incoming record was not newer, so title/body/status were kept as-is —
	// but if it contributed a new ref, memory_refs is now unioned and
	// search_text/FTS must follow, or the index silently falls out of sync with
	// the ref set the row actually reports (structural recall would see the new
	// ref; lexical would not). Only reindex FTS when the row is still active —
	// a terminal row must stay out of the lexical candidate pool regardless.
	if refsChanged {
		unionedRefs, err := e.loadRefsTx(ctx, c, rec.ID)
		if err != nil {
			return SyncNoop, err
		}
		searchText := BuildSearchText(local.title, local.body, unionedRefs)
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET search_text=? WHERE id=?`, searchText, rec.ID); err != nil {
			return SyncNoop, err
		}
		if local.status == "active" {
			if err := e.lex.Delete(ctx, c, rec.ID); err != nil {
				return SyncNoop, err
			}
			if err := e.lex.Index(ctx, c, rec.ID, searchText); err != nil {
				return SyncNoop, err
			}
		}
	}
	return SyncUpdated, nil
}

// syncApplyAbsent handles a rec whose id has no local row: reinforce a
// same-kind near-duplicate if one exists (converging independently-authored
// copies, mirroring ImportMemory), else insert verbatim. now is the local
// wall clock, used only for the reinforce bump (never for the insert, which
// preserves rec's original timestamps).
func (e *Engine) syncApplyAbsent(ctx context.Context, c store.Conn, rec syncwire.Record, now int64) (SyncOutcome, string, error) {
	refs := refsFromWire(rec.Refs)
	nm := NewMemory{
		Kind: Kind(rec.Kind), Title: rec.Title, Body: rec.Body, Refs: refs,
		Branch: rec.Branch, Author: rec.Author, Source: rec.Source, Pinned: rec.Pinned,
		Extra: rec.Extra,
	}
	set := shingleSet(rec.Body)
	bands := bandHashes(minHashSignature(set))

	candID, found, _, err := e.findNearDup(ctx, c, nm.Kind, bands, set)
	if err != nil {
		return SyncNoop, "", err
	}
	if found {
		if err := e.reinforce(ctx, c, candID, nm, now); err != nil {
			return SyncNoop, "", err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET pinned = MAX(pinned, ?), confidence = MAX(confidence, ?) WHERE id=?`,
			boolToInt(rec.Pinned), clampConfidence(rec.Confidence), candID); err != nil {
			return SyncNoop, "", err
		}
		return SyncReinforced, candID, nil
	}

	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	sim := simHash64(set)
	if err := e.insert(ctx, c, rec.ID, nm, searchText, sim, bands, rec.CreatedAt); err != nil {
		return SyncNoop, "", err
	}
	if rec.UpdatedAt != rec.CreatedAt {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET updated_at=? WHERE id=?`, rec.UpdatedAt, rec.ID); err != nil {
			return SyncNoop, "", err
		}
	}
	if _, err := c.ExecContext(ctx,
		`UPDATE memories SET confidence=?, reinforcement_count=? WHERE id=?`,
		clampConfidence(rec.Confidence), rec.ReinforcementCount, rec.ID); err != nil {
		return SyncNoop, "", err
	}
	// insert() always lands a fresh row as status='active'. A record whose
	// origin machine had already moved it on (done/resolved/superseded — e.g. a
	// completed todo this machine never had before bootstrap) must land in that
	// same state, not resurrect as active, and must drop out of the lexical
	// index exactly like the local completion paths (CompleteTodo,
	// ResolveFeedback, supersede) already do.
	if status := rec.Status; status != "" && status != "active" {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET status=?, superseded_by=? WHERE id=?`,
			status, nullStr(rec.SupersededBy), rec.ID); err != nil {
			return SyncNoop, "", err
		}
		if err := e.lex.Delete(ctx, c, rec.ID); err != nil {
			return SyncNoop, "", err
		}
	}
	return SyncInserted, rec.ID, nil
}

// applyTombstoneRow deletes the local row for rec.ID (unless a newer local
// edit beats the incoming delete) and records/extends the tombstone. pushed=true
// because a tombstone reaching SyncApply always originates elsewhere (a pull
// from the server, or the server applying a client's push) — it is already
// known on at least one other side, so this side must never re-push it as if
// it were freshly authored here. Only Forget's direct call to upsertTombstone
// (pushed=false) marks a tombstone as needing an outbound push.
func (e *Engine) applyTombstoneRow(ctx context.Context, c store.Conn, rec syncwire.Record, tombAt int64, hasTomb bool) (SyncOutcome, error) {
	var localUpdatedAt int64
	err := c.QueryRowContext(ctx, `SELECT updated_at FROM memories WHERE id=?`, rec.ID).Scan(&localUpdatedAt)
	switch {
	case err == nil:
		if rec.UpdatedAt < localUpdatedAt {
			// A later local edit beats a stale tombstone: keep the local row and
			// do not record a tombstone that would shadow it.
			return SyncNoop, nil
		}
		if err := e.deleteMemoryRows(ctx, c, []string{rec.ID}); err != nil {
			return SyncNoop, err
		}
	case errors.Is(err, sql.ErrNoRows):
		if hasTomb && tombAt >= rec.UpdatedAt {
			return SyncNoop, nil // already recorded at least this recent; nothing changed
		}
	default:
		return SyncNoop, err
	}
	if err := e.upsertTombstone(ctx, c, rec.ID, rec.UpdatedAt, true); err != nil {
		return SyncNoop, err
	}
	return SyncTombstoned, nil
}

// getTombstone returns the recorded delete time for id, if any.
func (e *Engine) getTombstone(ctx context.Context, c store.Conn, id string) (int64, bool, error) {
	var at int64
	err := c.QueryRowContext(ctx, `SELECT deleted_at FROM memory_tombstones WHERE id=?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return at, true, nil
}

// upsertTombstone records id as deleted at deletedAt (extending forward only:
// a later delete always wins, an earlier one never overwrites a later record).
// pushed carries forward from the existing row when the incoming deletedAt
// does not advance it, so a remote-learned tombstone (pushed=true) is never
// downgraded back to "needs push" by a stale re-application.
func (e *Engine) upsertTombstone(ctx context.Context, c store.Conn, id string, deletedAt int64, pushed bool) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO memory_tombstones(id, deleted_at, pushed) VALUES (?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   pushed = CASE WHEN excluded.deleted_at >= memory_tombstones.deleted_at
		                 THEN excluded.pushed ELSE memory_tombstones.pushed END,
		   deleted_at = MAX(memory_tombstones.deleted_at, excluded.deleted_at)`,
		id, deletedAt, boolToInt(pushed))
	return err
}

// rewriteMinhash replaces id's MinHash band rows with bands (id's content, and
// therefore its bands, changed under an LWW overwrite).
func (e *Engine) rewriteMinhash(ctx context.Context, c store.Conn, id string, bands [lshBands]uint64) error {
	if _, err := c.ExecContext(ctx, `DELETE FROM memory_minhash WHERE memory_id=?`, id); err != nil {
		return err
	}
	for b := 0; b < lshBands; b++ {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memory_minhash(memory_id, band, hash) VALUES (?,?,?)`,
			id, b, int64(bands[b])); err != nil {
			return err
		}
	}
	return nil
}

// loadRefsTx reads id's current ref set on c (the same open transaction that
// just unioned incoming refs into memory_refs via INSERT OR IGNORE), so a
// search_text rebuild sees local ∪ incoming refs rather than only one side.
func (e *Engine) loadRefsTx(ctx context.Context, c store.Conn, id string) ([]Ref, error) {
	rows, err := c.QueryContext(ctx, `SELECT ref_type, ref_value FROM memory_refs WHERE memory_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var t, v string
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		refs = append(refs, Ref{Type: RefType(t), Value: v})
	}
	return refs, rows.Err()
}

// refsFromWire converts wire refs to the engine's Ref type.
func refsFromWire(refs []syncwire.Ref) []Ref {
	out := make([]Ref, 0, len(refs))
	for _, r := range refs {
		out = append(out, Ref{Type: RefType(r.Type), Value: r.Value})
	}
	return out
}

// ToWireRecord converts a stored Record to its wire shape, for the pull/push
// read paths (PendingPush and the sync server's changes handler).
func ToWireRecord(r *Record) syncwire.Record {
	refs := make([]syncwire.Ref, 0, len(r.Refs))
	for _, ref := range r.Refs {
		refs = append(refs, syncwire.Ref{Type: string(ref.Type), Value: ref.Value})
	}
	return syncwire.Record{
		ID: r.ID, Kind: string(r.Kind), Title: r.Title, Body: r.Body, Refs: refs,
		Branch: r.Branch, Author: r.Author, Source: r.Source,
		Confidence: r.Confidence, ReinforcementCount: r.ReinforcementCount, Pinned: r.Pinned,
		Status: r.Status, SupersededBy: r.SupersededBy, Extra: r.Extra,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// markDirty records id as having a locally-authored change not yet confirmed
// pushed. It is called ONLY from local-authoring write paths (Record,
// ImportMemory, the dedup merge's canonical bump) — never from SyncApply,
// which applies changes that originated elsewhere.
func (e *Engine) markDirty(ctx context.Context, c store.Conn, id string, updatedAt int64) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO memory_sync_outbox(memory_id, updated_at) VALUES (?,?)
		 ON CONFLICT(memory_id) DO UPDATE SET updated_at = excluded.updated_at`,
		id, updatedAt)
	return err
}

// PendingPush returns memories with a pending local change (present in
// memory_sync_outbox), as wire records ready to POST to the sync server. This
// is NOT filtered to status='active': the outbox is populated only by
// explicit local-authoring writes (Record, ImportMemory, the dedup merge's
// canonical bump, CompleteTodo, ResolveFeedback, an explicit supersede), and
// those status transitions (active -> done/resolved/superseded) must
// propagate just as much as a still-active row's content does — a completed
// todo that fails to sync would keep surfacing as open work on every other
// machine.
func (e *Engine) PendingPush(ctx context.Context) ([]syncwire.Record, error) {
	rows, err := e.st.DB.QueryContext(ctx, selectMemoriesSQL+
		` WHERE id IN (SELECT memory_id FROM memory_sync_outbox)`)
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
	out := make([]syncwire.Record, len(recs))
	for i, r := range recs {
		out[i] = ToWireRecord(r)
	}
	return out, nil
}

// ClearPushed removes ids from the push outbox after a confirmed push.
func (e *Engine) ClearPushed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`DELETE FROM memory_sync_outbox WHERE memory_id IN (`+strings.Join(ph, ",")+`)`, args...)
		return err
	})
}

// PendingTombstones returns locally-authored deletes (memory_tombstones rows
// with pushed=0) as wire tombstone records ready to push.
func (e *Engine) PendingTombstones(ctx context.Context) ([]syncwire.Record, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id, deleted_at FROM memory_tombstones WHERE pushed=0`)
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

// MarkTombstonesPushed flips pushed=1 for ids after a confirmed push, so they
// are not re-sent on the next push cycle.
func (e *Engine) MarkTombstonesPushed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, id := range ids {
			if _, err := c.ExecContext(ctx,
				`UPDATE memory_tombstones SET pushed=1 WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}
