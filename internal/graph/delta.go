package graph

import (
	"context"
	"database/sql"
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
// Identity is (qualified_name, kind); a span or signature difference is
// "changed". Called only when the file already had rows (first index of a file
// journals nothing — fresh clones and wipe-migration rebuilds must not flood
// the journal).
//
// Overload sets: C# and other languages can produce multiple nodes with the
// same (qualified_name, kind) — e.g. Add(int) and Add(string). We collect each
// side as a sorted multiset of "startByte:endByte:signature" strings and diff
// the multisets, so an unchanged overload set never emits a spurious "changed".
func journalFileDeltas(ctx context.Context, c store.Conn, worktree, path, fileID string,
	newNodes []nodeRow, now int64) error {

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

	emit := func(qn, kind, change string) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
			 VALUES (?,?,?,?,?,?,?)`, ids.New(), worktree, path, qn, kind, change, now)
		return err
	}

	// New keys: added or changed.
	for key, newFPs := range newMulti {
		qn, kind := splitDeltaKey(key)
		oldFPs, ok := oldMulti[key]
		switch {
		case !ok:
			if err := emit(qn, kind, "added"); err != nil {
				return err
			}
		case !multisetEqual(oldFPs, newFPs):
			if err := emit(qn, kind, "changed"); err != nil {
				return err
			}
		}
	}
	// Old keys absent from new: removed.
	for key := range oldMulti {
		if _, ok := newMulti[key]; ok {
			continue
		}
		qn, kind := splitDeltaKey(key)
		if err := emit(qn, kind, "removed"); err != nil {
			return err
		}
	}
	return nil
}

// journalRecreatedFileDeltas journals "added" for every node of a file whose
// first index follows earlier journal rows at the same path — the
// delete-then-recreate shape (branch switches, stash pops). Without it the
// recreation is invisible (a first index journals nothing) and the earlier
// "removed" stands as the symbol's disposition while it sits in the worktree.
// Genuinely new paths have no journal rows and still journal nothing, so the
// fresh-clone / wipe-migration flood guard holds. The probe rides the small,
// session-pruned graph_deltas table, so it costs nothing on the fresh-clone
// path where it runs once per file.
func journalRecreatedFileDeltas(ctx context.Context, c store.Conn, worktree, path string,
	newNodes []nodeRow, now int64) error {
	var seen bool
	if err := c.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph_deltas WHERE worktree = ? AND path = ?)`,
		worktree, path).Scan(&seen); err != nil {
		return err
	}
	if !seen {
		return nil
	}
	for _, n := range newNodes {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
			 VALUES (?,?,?,?,?,?,?)`, ids.New(), worktree, path, n.qn, n.kind, "added", now); err != nil {
			return err
		}
	}
	return nil
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

// DeltasSince returns the folded symbol-level changes journaled after sinceMs:
// one entry per (path, qualified_name, kind), with the disposition read
// relative to the baseline, not as a raw event log. The first event tells
// whether the symbol existed at baseline (anything but "added" means it did);
// the last tells whether it exists now (anything but "removed" means it does).
// Born-and-gone within the window nets out entirely; existed-then-recreated
// reads as "changed" (it is present, content unknown), never "removed".
// total is the folded count before limit.
func (e *Engine) DeltasSince(ctx context.Context, worktree string, sinceMs int64, limit int) ([]Delta, int, error) {
	if limit <= 0 {
		limit = 50
	}
	// at ties are common (one transaction stamps a whole batch); id is UUIDv7,
	// monotonic with insertion, so it pins chronological order within a tie.
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT path, qualified_name, kind, change FROM graph_deltas
		  WHERE worktree = ? AND at >= ? ORDER BY at, id`,
		worktree, sinceMs)
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
	var latest int64
	err := e.st.DB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(started_at), 0) FROM sessions WHERE worktree = ?`, worktree).Scan(&latest)
	if err != nil {
		return 0, false, err
	}
	if latest == 0 {
		return 0, false, nil
	}
	var baseline int64
	err = e.st.DB.QueryRowContext(ctx, `
SELECT s.started_at FROM sessions s
 WHERE s.worktree = ? AND s.started_at < ?
   AND EXISTS (SELECT 1 FROM graph_deltas d
                WHERE d.worktree = s.worktree
                  AND d.at >= s.started_at AND d.at <= COALESCE(s.ended_at, ?))
 ORDER BY s.started_at DESC LIMIT 1`, worktree, latest, nowMs).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return baseline, true, nil
}

// PruneDeltas drops journal rows older than beforeMs for the worktree.
func (e *Engine) PruneDeltas(ctx context.Context, worktree string, beforeMs int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`DELETE FROM graph_deltas WHERE worktree = ? AND at < ?`, worktree, beforeMs)
		return err
	})
}
