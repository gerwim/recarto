package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gerwim/recarto/internal/ids"
	"github.com/gerwim/recarto/internal/store"
)

// Delta is one symbol-level index change: a symbol that was added, removed, or
// changed (span/signature) since some point in time, as journaled at reindex.
type Delta struct {
	Path          string
	QualifiedName string
	Kind          string
	Change        string // added | removed | changed
}

// journalFileDeltas diffs a file's stored node set against the replacement and
// inserts one graph_deltas row per difference, on the caller's transaction.
// The rows count at ts.at. Identity is (qualified_name, kind); a span or signature difference is
// "changed". Called only when the file already had rows (first index of a file
// journals nothing — fresh clones and wipe-migration rebuilds must not flood
// the journal).
//
// Overload sets: C# and other languages can produce multiple nodes with the
// same (qualified_name, kind) — e.g. Add(int) and Add(string). We collect each
// side as a sorted multiset of "startByte:endByte:signature" strings and diff
// the multisets, so an unchanged overload set never emits a spurious "changed".
func journalFileDeltas(ctx context.Context, c store.Conn, worktree, path, fileID string,
	newNodes []nodeRow, ts stamp) error {

	// oldMulti maps key → sorted slice of "sb:eb:sig" fingerprints.
	oldMulti := map[string][]string{}
	rows, err := c.QueryContext(ctx,
		`SELECT qualified_name, kind, start_byte, end_byte, signature FROM nodes WHERE file_id = ?`, fileID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var qn, kind, sg string
		var sb, eb int64
		if err := rows.Scan(&qn, &kind, &sb, &eb, &sg); err != nil {
			rows.Close()
			return err
		}
		key := qn + "\x00" + kind
		fp := fmt.Sprintf("%d:%d:%s", sb, eb, sg)
		oldMulti[key] = append(oldMulti[key], fp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Sort each old slice so multiset comparison is order-independent.
	for k := range oldMulti {
		sort.Strings(oldMulti[k])
	}

	// Build newMulti the same way.
	newMulti := map[string][]string{}
	for _, n := range newNodes {
		key := n.qn + "\x00" + n.kind
		fp := fmt.Sprintf("%d:%d:%s", n.startByte, n.endByte, n.signature)
		newMulti[key] = append(newMulti[key], fp)
	}
	for k := range newMulti {
		sort.Strings(newMulti[k])
	}

	var changes []deltaRow
	emit := func(qn, kind, change string) {
		changes = append(changes, deltaRow{qn: qn, kind: kind, change: change})
	}

	// New keys: added or changed.
	for key, newFPs := range newMulti {
		qn, kind := splitDeltaKey(key)
		oldFPs, ok := oldMulti[key]
		switch {
		case !ok:
			emit(qn, kind, "added")
		case !multisetEqual(oldFPs, newFPs):
			emit(qn, kind, "changed")
		}
	}
	// Old keys absent from new: removed.
	for key := range oldMulti {
		if _, ok := newMulti[key]; ok {
			continue
		}
		qn, kind := splitDeltaKey(key)
		emit(qn, kind, "removed")
	}
	return insertDeltas(ctx, c, worktree, path, changes, ts)
}

// journalRecreatedFileDeltas journals "added", at ts.at, for every node of a file whose
// first index follows earlier journal rows at the same path — the
// delete-then-recreate shape (branch switches, stash pops). Without it the
// recreation is invisible (a first index journals nothing) and the earlier
// "removed" stands as the symbol's disposition while it sits in the worktree.
// Genuinely new paths have no journal rows and still journal nothing, so the
// fresh-clone / wipe-migration flood guard holds. The probe rides the small,
// session-pruned graph_deltas table, so it costs nothing on the fresh-clone
// path where it runs once per file.
func journalRecreatedFileDeltas(ctx context.Context, c store.Conn, worktree, path string,
	newNodes []nodeRow, ts stamp) error {
	var seen bool
	if err := c.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph_deltas WHERE worktree = ? AND path = ?)`,
		worktree, path).Scan(&seen); err != nil {
		return err
	}
	if !seen {
		return nil
	}
	rows := make([]deltaRow, 0, len(newNodes))
	for _, n := range newNodes {
		rows = append(rows, deltaRow{qn: n.qn, kind: n.kind, change: "added"})
	}
	return insertDeltas(ctx, c, worktree, path, rows, ts)
}

// stamp is when a write's journal rows count (at), when the index saw the
// version they come from (seen), and the write's own time (now).
type stamp struct{ at, seen, now int64 }

// deltaRow is one symbol's change in a write's journal.
type deltaRow struct{ qn, kind, change string }

// deltaSeqKey names the meta row holding the seq of the store's last journal
// write. It only grows, so pruning the rows with the highest seq never lets a
// later write reuse a seq a session mark already covers.
const deltaSeqKey = "graph_delta_seq"

// nextDeltaSeq takes the next journal seq on c's write transaction.
//
// It reads the number back instead of using INSERT ... RETURNING: a RETURNING
// statement here makes SQLite lay out the FTS segments of an index writing in
// the same transaction differently, which the graph-wipe size guard pins.
func nextDeltaSeq(ctx context.Context, c store.Conn) (int64, error) {
	if _, err := c.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`, deltaSeqKey); err != nil {
		return 0, err
	}
	return JournalMark(ctx, c)
}

// JournalMark returns the seq of the last journal write c sees, 0 before the
// first. Every row with a seq up to it is visible to a later read on the store.
func JournalMark(ctx context.Context, c store.Conn) (int64, error) {
	var mark int64
	err := c.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?), 0)`, deltaSeqKey).Scan(&mark)
	return mark, err
}

// insertDeltas journals a write's rows of path on c, counting at ts.at, with
// the time the index saw their version. A credited write that changes no
// symbol journals nothing and so moves nothing: the journal stays as it
// would be without the edit. The rows and the seq they take commit together;
// a higher seq is a later commit.
func insertDeltas(ctx context.Context, c store.Conn, worktree, path string, rows []deltaRow, ts stamp) error {
	if len(rows) == 0 {
		return nil
	}
	if err := moveBackLaterRows(ctx, c, worktree, path, ts); err != nil {
		return err
	}
	seq, err := nextDeltaSeq(ctx, c)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at, seen_at, seq)
			 VALUES (?,?,?,?,?,?,?,?,?)`, ids.New(), worktree, path, r.qn, r.kind, r.change, ts.at, ts.seen, seq); err != nil {
			return err
		}
	}
	return nil
}

// moveBackLaterRows readies path's journal on c for a write credited to an
// earlier edit (ts.at < ts.now). Its rows that count after ts.at belong to
// versions the index saw before that edit; DeltasSince folds rows in at
// order and a session's listing starts at the latest session holding any,
// so they move to just before their version was seen, ahead of the edit
// and its session. A row without seen_at (written before it existed), or one
// seen after the credit, goes to the credit itself.
func moveBackLaterRows(ctx context.Context, c store.Conn, worktree, path string, ts stamp) error {
	if ts.at >= ts.now {
		return nil
	}
	_, err := c.ExecContext(ctx,
		`UPDATE graph_deltas SET at = MIN(COALESCE(seen_at - 1, ?1), ?1)
		  WHERE worktree=?2 AND path=?3 AND at > ?1`, ts.at, worktree, path)
	return err
}

// multisetEqual reports whether two sorted string slices are identical.
// Both inputs must already be sorted (journalFileDeltas sorts them).
func multisetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitDeltaKey undoes the qn+"\x00"+kind composite.
func splitDeltaKey(key string) (qn, kind string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// DeltasIn returns r's folded symbol-level changes, as DeltasSince folds them:
// one entry per (path, qualified_name, kind), with the disposition read
// relative to the range, not as a raw event log. The first event in the range
// tells whether the symbol existed at its start (anything but "added" means it
// did); the last tells whether it exists now (anything but "removed" means it
// does). Born-and-gone within the window nets out entirely;
// existed-then-recreated reads as "changed" (it is present, content unknown),
// never "removed". total is the folded count before limit.
func (e *Engine) DeltasIn(ctx context.Context, worktree string, r ChangeRange, limit int) ([]Delta, int, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	// at ties are common (one transaction stamps a whole batch); id is UUIDv7,
	// monotonic with insertion, so it pins chronological order within a tie.
	// seq > a NULL mark is NULL: no late part without a mark.
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, qualified_name, kind, change FROM graph_deltas
		  WHERE worktree = ?1
		    AND (at >= ?2 OR seq > ?3)
		    AND (?4 IS NULL OR seq IS NULL OR seq <= ?4)
		  ORDER BY at, id`,
		worktree, r.Since, r.Mark, r.Upto)
	if err != nil {
		return nil, 0, err
	}
	type state struct {
		first, last string
	}
	folded := map[string]*state{}
	var order []string
	for rows.Next() {
		var p, qn, kind, ch string
		if err := rows.Scan(&p, &qn, &kind, &ch); err != nil {
			rows.Close()
			return nil, 0, err
		}
		key := p + "\x00" + qn + "\x00" + kind
		st, ok := folded[key]
		if !ok {
			st = &state{first: ch}
			folded[key] = st
			order = append(order, key)
		}
		st.last = ch
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var out []Delta
	for _, key := range order {
		st := folded[key]
		existedAtBaseline := st.first != "added"
		existsNow := st.last != "removed"
		var change string
		switch {
		case !existedAtBaseline && !existsNow:
			continue // net nothing: born and gone within the window
		case !existedAtBaseline:
			change = "added"
		case !existsNow:
			change = "removed"
		default:
			change = "changed"
		}
		var p, rest string
		p, rest = splitDeltaKey(key)
		qn, kind := splitDeltaKey(rest)
		out = append(out, Delta{Path: p, QualifiedName: qn, Kind: kind, Change: change})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].QualifiedName < out[j].QualifiedName
	})
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// DeltasSince returns the changes journaled at or after sinceMs, as DeltasIn
// folds a range with no late part and no cap.
func (e *Engine) DeltasSince(ctx context.Context, worktree string, sinceMs int64, limit int) ([]Delta, int, error) {
	return e.DeltasIn(ctx, worktree, ChangeRange{Since: sinceMs}, limit)
}

// DeltaBaseline returns the started_at of the most recent session (for this
// worktree) that is older than the latest session AND whose window journaled at
// least one delta — the last session that did real work. /clear and
// open-and-close sessions journal nothing and are skipped, so the same delta
// keeps showing until a context has genuinely lived alongside the changes.
// ok=false when no such session exists (fresh repo, or no deltas ever).
//
// Sessions are the agents domain, but the walk needs the deltas table too and
// both MCP and the session-start hook ask this exact question — one home in the
// engine beats two copies of cross-table SQL.
func (e *Engine) DeltaBaseline(ctx context.Context, worktree string, nowMs int64) (int64, bool, error) {
	return e.DeltaBaselineWith(ctx, worktree, "", 0, nowMs)
}

// DeltaBaselineWith is DeltaBaseline with session id, started at startedAt,
// counted in worktree while its row is not written yet: as a session with no
// end, both when the latest start is taken and among the earlier sessions a
// baseline is chosen from. An empty id counts only the rows written.
func (e *Engine) DeltaBaselineWith(ctx context.Context, worktree, id string, startedAt, nowMs int64) (int64, bool, error) {
	return deltaBaselineWith(ctx, e.st.DB, worktree, id, startedAt, nowMs)
}

func deltaBaselineWith(ctx context.Context, c store.Conn, worktree, id string, startedAt, nowMs int64) (int64, bool, error) {
	of := RangeOf{}
	if id != "" {
		of.Pending = []Session{{ID: id, StartedAt: startedAt}}
	}
	return baselineOf(ctx, c, worktree, of, nowMs)
}

// Session is a session counted in a worktree while its row is not written: a
// session start's own conversation, or one whose row waits in a pending end.
type Session struct {
	ID        string
	StartedAt int64
	EndedAt   sql.NullInt64 // invalid: no end recorded
	Mark      sql.NullInt64 // the mark of its latest full block; invalid: none
}

// sessionsOf is worktree ?1's sessions, each with the higher of its row's mark
// and its recorded one (?3, a JSON object of id: mark), plus the pending ones
// (?2, a JSON array of {id, started_at, ended_at, mark}); each pending one
// counts only while no row of its id exists.
const sessionsOf = `(SELECT s.id, s.started_at, s.ended_at,
         CASE WHEN r.value > COALESCE(s.delta_mark, -1) THEN r.value ELSE s.delta_mark END AS delta_mark
    FROM sessions s LEFT JOIN json_each(?3) r ON r.key = s.id
   WHERE s.worktree = ?1
  UNION ALL
  SELECT json_extract(p.value, '$.id'), json_extract(p.value, '$.started_at'),
         json_extract(p.value, '$.ended_at'), json_extract(p.value, '$.mark')
    FROM json_each(?2) p
   WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE id = json_extract(p.value, '$.id')))`

// sessionJSON is how a pending session reaches sessionsOf: null for a missing
// end or mark.
type sessionJSON struct {
	ID        string `json:"id"`
	StartedAt int64  `json:"started_at"`
	EndedAt   *int64 `json:"ended_at"`
	Mark      *int64 `json:"mark"`
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func sessionsJSON(sessions []Session) (string, error) {
	if len(sessions) == 0 {
		return "[]", nil
	}
	out := make([]sessionJSON, len(sessions))
	for i, s := range sessions {
		out[i] = sessionJSON{ID: s.ID, StartedAt: s.StartedAt,
			EndedAt: nullableInt64(s.EndedAt), Mark: nullableInt64(s.Mark)}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func recordedJSON(marks map[string]int64) (string, error) {
	if len(marks) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(marks)
	return string(b), err
}

// RangeOf says whose range RecentRange returns.
type RangeOf struct {
	Pending  []Session        // sessions counted while their rows are not written, the caller's included
	Recorded map[string]int64 // marks recorded outside the store (a folder's ledger), by session id; each raises its row's mark
	Upto     sql.NullInt64    // a block's own mark: its baseline and rows are taken as the journal stood then; invalid for a live read
}

// ChangeRange is the part of a worktree's journal that a session start's block
// reports, and get_graph_delta with it: the rows counting from Since on, and
// the rows written after Mark, which no block but the last one reported, as
// the journal stood at Upto.
type ChangeRange struct {
	Since int64         // the baseline: the start of the last earlier session that changed the graph
	Mark  sql.NullInt64 // the highest mark among the sessions but the last block (the highest mark, a tie to the later start); invalid when none has one
	Upto  sql.NullInt64 // copied from RangeOf.Upto
}

// RecentRange returns the range of a block built now in the latest session of
// worktree, counting of.Pending. ok=false when no baseline exists: then nothing
// is listed, as before.
func (e *Engine) RecentRange(ctx context.Context, worktree string, of RangeOf, nowMs int64) (ChangeRange, bool, error) {
	since, ok, err := baselineOf(ctx, e.st.DB, worktree, of, nowMs)
	r := ChangeRange{Since: since, Upto: of.Upto}
	if err != nil || !ok {
		return r, false, err
	}
	pending, err := sessionsJSON(of.Pending)
	if err != nil {
		return ChangeRange{}, false, err
	}
	recorded, err := recordedJSON(of.Recorded)
	if err != nil {
		return ChangeRange{}, false, err
	}
	// The last block is the session with the highest mark; a tie goes to the
	// later start. Its own rows are left out of Mark so a block never hides a
	// change behind the mark it is recording, and the tool, which cannot name
	// its caller, reports the same range.
	const s = sessionsOf
	err = e.st.DB.QueryRowContext(ctx, `
SELECT MAX(s.delta_mark) FROM `+s+` s
 WHERE s.id IS NOT (SELECT b.id FROM `+s+` b WHERE b.delta_mark IS NOT NULL
                     ORDER BY b.delta_mark DESC, b.started_at DESC, b.id DESC LIMIT 1)`,
		worktree, pending, recorded).Scan(&r.Mark)
	if err != nil {
		return ChangeRange{}, false, err
	}
	return r, true, nil
}

// baselineOf returns the start of the last session before the latest one that
// journaled a delta in its span, as the journal stood at of.Upto. ok=false when
// there is no such session.
func baselineOf(ctx context.Context, c store.Conn, worktree string, of RangeOf, nowMs int64) (int64, bool, error) {
	pending, err := sessionsJSON(of.Pending)
	if err != nil {
		return 0, false, err
	}
	recorded, err := recordedJSON(of.Recorded)
	if err != nil {
		return 0, false, err
	}
	var latest int64
	if err := c.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(started_at), 0) FROM `+sessionsOf+` s`,
		worktree, pending, recorded).Scan(&latest); err != nil {
		return 0, false, err
	}
	if latest == 0 {
		return 0, false, nil
	}
	var baseline int64
	err = c.QueryRowContext(ctx, `
SELECT s.started_at FROM `+sessionsOf+` s
 WHERE s.started_at < ?4
   AND EXISTS (SELECT 1 FROM graph_deltas d
                WHERE d.worktree = ?1
                  AND d.at >= s.started_at AND d.at <= COALESCE(s.ended_at, ?5)
                  AND (?6 IS NULL OR d.seq IS NULL OR d.seq <= ?6))
 ORDER BY s.started_at DESC LIMIT 1`, worktree, pending, recorded, latest, nowMs, of.Upto).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return baseline, true, nil
}

// tenthSessionStart returns the started_at of the worktree's 10th-most-recent
// session, or 0 when fewer exist.
func tenthSessionStart(ctx context.Context, c store.Conn, worktree string) (int64, error) {
	var at int64
	err := c.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE worktree = ?
		  ORDER BY started_at DESC LIMIT 1 OFFSET 9`, worktree).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return at, err
}

// PruneUnreachableDeltas drops worktree's journal rows no baseline walk can
// reach: those older than the 10th-most-recent session's start, never past the
// current baseline (session id, started at startedAt, counted while its row is
// not written). hold reports a session end still to be written, which makes it
// delete nothing. Like PruneDeltas before it, it takes the write lock only when
// the reads find ten sessions and no hold; it then decides again inside that
// transaction, so an end written since the reads counts.
func (e *Engine) PruneUnreachableDeltas(ctx context.Context, worktree, id string, startedAt, nowMs int64, hold func(c store.Conn) (bool, error)) error {
	tenth, err := tenthSessionStart(ctx, e.st.DB, worktree)
	if err != nil || tenth == 0 {
		return err
	}
	pending, err := hold(e.st.DB)
	if err != nil || pending {
		return err
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		pending, err := hold(c)
		if err != nil || pending {
			return err
		}
		tenth, err := tenthSessionStart(ctx, c, worktree)
		if err != nil || tenth == 0 {
			return err
		}
		cutoff := tenth
		baseline, ok, err := deltaBaselineWith(ctx, c, worktree, id, startedAt, nowMs)
		if err != nil {
			return err
		}
		if ok && baseline < cutoff {
			cutoff = baseline
		}
		_, err = c.ExecContext(ctx,
			`DELETE FROM graph_deltas WHERE worktree=? AND at<?`, worktree, cutoff)
		return err
	})
}

// PruneDeltas drops journal rows older than beforeMs for the worktree.
func (e *Engine) PruneDeltas(ctx context.Context, worktree string, beforeMs int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`DELETE FROM graph_deltas WHERE worktree = ? AND at < ?`, worktree, beforeMs)
		return err
	})
}
