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
// versions the index saw before that edit; a range folds rows in at
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

// foldDeltas returns worktree's changes from since on, and those written after
// mark (none when invalid), read on c and folded: one entry per (path,
// qualified_name, kind), with the disposition read relative to the range, not
// as a raw event log. The first event in the range tells whether the symbol
// existed at its start (anything but "added" means it did); the last tells
// whether it exists now (anything but "removed" means it does). Born-and-gone
// within the window nets out entirely; existed-then-recreated reads as
// "changed" (it is present, content unknown), never "removed". total is the
// folded count before limit.
func foldDeltas(ctx context.Context, c store.Conn, worktree string, since int64, mark sql.NullInt64, limit int) ([]Delta, int, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	// at ties are common (one transaction stamps a whole batch); id is UUIDv7,
	// monotonic with insertion, so it pins chronological order within a tie.
	// seq > a NULL mark is NULL: no late part without a mark.
	rows, err := c.QueryContext(ctx,
		`SELECT path, qualified_name, kind, change FROM graph_deltas
		  WHERE worktree = ?1 AND (at >= ?2 OR seq > ?3) ORDER BY at, id`,
		worktree, since, mark)
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

// DeltasSince returns the changes journaled at or after sinceMs, folded as a
// range with no late part.
func (e *Engine) DeltasSince(ctx context.Context, worktree string, sinceMs int64, limit int) ([]Delta, int, error) {
	return foldDeltas(ctx, e.st.DB, worktree, sinceMs, sql.NullInt64{}, limit)
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
	var pending []Session
	if id != "" {
		pending = []Session{{ID: id, StartedAt: startedAt}}
	}
	b, ok, err := baselineOf(ctx, c, worktree, pending, nowMs)
	return b.since, ok, err
}

// Session is a session a range counts beside the worktree's rows, as the state
// still to come will leave its row: one whose end waits in a pending end, or a
// conversation of a folder outside git whose roster covers the worktree.
type Session struct {
	ID        string
	StartedAt int64
	EndedAt   sql.NullInt64 // the end a missing or open row takes; invalid: none
	Mark      sql.NullInt64 // the mark a missing or open row is raised to; invalid: none
	EndedMark sql.NullInt64 // the mark an ended row is raised to; invalid: it keeps its own
}

// sessionsWith defines s: worktree ?1's sessions with the entries of ?2 (a
// JSON array of {id, started_at, ended_at, mark, ended_mark}, one per id)
// folded in as the state still to come leaves them. An entry with no row in
// the store counts as itself; one whose row is open in the worktree gives it
// its end and the higher of the two marks, as recovery (writeRepoEnd) and warm
// (raiseMark) write it; one whose row is ended there gives it only the higher
// of its mark and ended_mark, the roster mark warm writes to any row (a
// pending end's record is dropped once its row is ended); one whose row is in
// another worktree is not counted. The entries are read once and each finds
// its row by key, so a folder's whole history costs a lookup per entry, not a
// scan per row.
const sessionsWith = `WITH p AS MATERIALIZED (
  SELECT json_extract(value, '$.id') AS id, json_extract(value, '$.started_at') AS started_at,
         json_extract(value, '$.ended_at') AS ended_at, json_extract(value, '$.mark') AS mark,
         json_extract(value, '$.ended_mark') AS ended_mark
    FROM json_each(?2)),
s AS (
  SELECT r.id, r.started_at, r.ended_at, r.delta_mark FROM sessions r
   WHERE r.worktree = ?1 AND r.id NOT IN (SELECT id FROM p)
  UNION ALL
  SELECT id, started_at, ended_at,
         CASE WHEN NOT has_row OR lifted > COALESCE(row_mark, -1) THEN lifted ELSE row_mark END
    FROM (SELECT p.id, COALESCE(r.started_at, p.started_at) AS started_at,
                 COALESCE(r.ended_at, p.ended_at) AS ended_at,
                 r.id IS NOT NULL AS has_row, r.delta_mark AS row_mark,
                 CASE WHEN r.ended_at IS NULL THEN p.mark ELSE p.ended_mark END AS lifted
            FROM p LEFT JOIN sessions r ON r.id = p.id
           WHERE r.id IS NULL OR r.worktree = ?1))
`

// sessionJSON is how a pending session reaches sessionsWith: null for a
// missing end or mark.
type sessionJSON struct {
	ID        string `json:"id"`
	StartedAt int64  `json:"started_at"`
	EndedAt   *int64 `json:"ended_at"`
	Mark      *int64 `json:"mark"`
	EndedMark *int64 `json:"ended_mark"`
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
			EndedAt: nullableInt64(s.EndedAt), Mark: nullableInt64(s.Mark),
			EndedMark: nullableInt64(s.EndedMark)}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// baseline is the session a range starts at: the last session before the
// latest one that journaled a delta in its span.
type baseline struct {
	since int64         // its start
	id    string        // its id
	mark  sql.NullInt64 // its mark; invalid: none
}

// ChangeRange is what a session-start block built now in a worktree's latest
// session reports, and get_graph_delta with it: the rows counting from Since
// on and the rows written after the baseline session's mark, folded, as one
// state of the journal holds them.
type ChangeRange struct {
	Since  int64         // the baseline: the start of the last session before the latest one that changed the graph
	Base   string        // that session's id
	Mark   sql.NullInt64 // that session's mark; invalid: no late part
	Read   int64         // the journal mark the range was read at; set even when there is no baseline
	Deltas []Delta       // folded, sorted by path and name, at most limit
	Total  int           // the folded count before limit
}

// afterRangeBaseline runs inside RecentRange's read, between its baseline and
// its listing. A var so a test can commit a write there.
var afterRangeBaseline = func() {}

// RecentRange reads, in one read transaction, the range of a block built now
// in the latest session of worktree, counting the pending sessions (at most
// one entry per session id), up to limit changes. ok=false when no baseline
// exists: nothing is listed, and Read is still set.
func (e *Engine) RecentRange(ctx context.Context, worktree string, pending []Session, nowMs int64, limit int) (ChangeRange, bool, error) {
	var r ChangeRange
	ok := false
	err := e.st.ReadTx(ctx, func(c store.Conn) error {
		read, err := JournalMark(ctx, c)
		if err != nil {
			return err
		}
		r.Read = read
		b, found, err := baselineOf(ctx, c, worktree, pending, nowMs)
		if err != nil || !found {
			return err
		}
		ok = true
		r.Since, r.Base, r.Mark = b.since, b.id, b.mark
		afterRangeBaseline()
		r.Deltas, r.Total, err = foldDeltas(ctx, c, worktree, r.Since, r.Mark, limit)
		return err
	})
	if err != nil {
		return ChangeRange{}, false, err
	}
	return r, ok, nil
}

// baselineOf returns the session a range starts at, as the journal stands on
// c: the last session before the latest one that journaled a delta in its
// span. ok=false when there is no such session.
func baselineOf(ctx context.Context, c store.Conn, worktree string, pending []Session, nowMs int64) (baseline, bool, error) {
	pendingJSON, err := sessionsJSON(pending)
	if err != nil {
		return baseline{}, false, err
	}
	var latest int64
	if err := c.QueryRowContext(ctx,
		sessionsWith+`SELECT COALESCE(MAX(started_at), 0) FROM s`,
		worktree, pendingJSON).Scan(&latest); err != nil {
		return baseline{}, false, err
	}
	if latest == 0 {
		return baseline{}, false, nil
	}
	var b baseline
	err = c.QueryRowContext(ctx, sessionsWith+`
SELECT s.id, s.started_at, s.delta_mark FROM s
 WHERE s.started_at < ?3
   AND EXISTS (SELECT 1 FROM graph_deltas d
                WHERE d.worktree = ?1
                  AND d.at >= s.started_at AND d.at <= COALESCE(s.ended_at, ?4))
 ORDER BY s.started_at DESC, s.id DESC LIMIT 1`, worktree, pendingJSON, latest, nowMs).Scan(&b.id, &b.since, &b.mark)
	if errors.Is(err, sql.ErrNoRows) {
		return baseline{}, false, nil
	}
	if err != nil {
		return baseline{}, false, err
	}
	return b, true, nil
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
// baseline of the range that counts pending. hold reports a session end still
// to be written, which makes it delete nothing. Like PruneDeltas before it, it
// takes the write lock only when the reads find ten sessions and no hold; it
// then decides again inside that transaction, so an end written since the
// reads counts.
func (e *Engine) PruneUnreachableDeltas(ctx context.Context, worktree string, pending []Session, nowMs int64, hold func(c store.Conn) (bool, error)) error {
	tenth, err := tenthSessionStart(ctx, e.st.DB, worktree)
	if err != nil || tenth == 0 {
		return err
	}
	held, err := hold(e.st.DB)
	if err != nil || held {
		return err
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		held, err := hold(c)
		if err != nil || held {
			return err
		}
		tenth, err := tenthSessionStart(ctx, c, worktree)
		if err != nil || tenth == 0 {
			return err
		}
		cutoff := tenth
		b, ok, err := baselineOf(ctx, c, worktree, pending, nowMs)
		if err != nil {
			return err
		}
		if ok && b.since < cutoff {
			cutoff = b.since
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
