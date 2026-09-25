package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repairing a corrupt store is a BUILD-ASIDE operation: nothing at the store's
// path is touched until a complete replacement has been built next to it and
// has passed an integrity check on a fresh connection. The corrupt file is then
// renamed aside (quarantined, never deleted) and the replacement moved into its
// place.
//
// The split that shapes the whole design is that Recarto's data is only half
// precious. Memories, their refs/minhash bands, counters, sessions, the sync
// tombstones/outbox/merge ledger and the graph delta journal are unregenerable;
// the code graph itself (files/nodes/edges/type_bindings/edge_candidates and
// their FTS shadows) is derived from source and is rebuilt by the next lazy
// reconcile. So repair salvages the small precious set row by row and simply
// leaves the large derivable set empty — the same convention the graph-invalidating
// migrations (v5–v18) already established.
//
// Repair is only ever performed with no connection pool of this process open on
// the file: at Open time (before the pool exists) or from `recarto repair`.
// Recarto deliberately does NOT swap the file under a live pool — Store.DB is
// read directly all over the codebase by goroutines this package does not
// synchronise, and renaming a WAL-mode database out from under active
// connections is not safe. A corrupt write inside a running session therefore
// returns actionable guidance (see writeTx) rather than repairing in place; the
// next Open — including the proactive quick_check on `recarto serve` startup —
// heals the store.
//
// Other PROCESSES may still hold the file. The repair lockfile serialises
// repairers and is also honoured by ordinary openers (waitForRepair), so a
// concurrent recarto process does not open the store during the rename window.
// The lock is HEARTBEATED: its owner touches the file's mtime every
// repairHeartbeatInterval, so a fresh mtime means "a repairer is alive right
// now" no matter how long the repair takes. Waiters therefore have no deadline
// — they block for exactly as long as the repair runs — while a lock whose
// heartbeat stopped for repairLockStaleAfter is treated as abandoned and
// claimed by an atomic rename, so two processes can never both conclude they
// stole it and start overlapping repairs.
//
// A foreign process that is mid-write on the quarantined inode loses those
// writes; that is acceptable for an exceptional event (those writes were
// failing anyway) and configured HTTP sync re-converges the store afterwards.

// TableStats is the salvage outcome for one table.
type TableStats struct {
	// Salvaged is the number of rows copied into the replacement.
	Salvaged int `json:"salvaged"`
	// Lost estimates rows present in the corrupt file that could not be copied.
	// It is best effort: when even the row count could not be read it stays 0
	// and Note explains why.
	Lost int `json:"lost,omitempty"`
	// Note records why a table was skipped or truncated (absent from the source
	// schema, unreadable cursor, ...). Empty on a clean full copy.
	Note string `json:"note,omitempty"`
}

// RepairStats summarises one repair.
type RepairStats struct {
	// Tables maps table name to its salvage outcome.
	Tables map[string]TableStats `json:"tables"`
	// SourceOpen reports whether ANYTHING was readable in the corrupt file: true
	// when its schema could be enumerated, and also when enumeration failed but a
	// blind per-table salvage still recovered rows. False therefore means nothing
	// at all was readable and the replacement is an empty, healthy store (the
	// original is still in Quarantine).
	SourceOpen bool `json:"source_open"`
	// Quarantine is the path the corrupt file was renamed to, or "" when there
	// was no pre-existing file.
	Quarantine string `json:"quarantine,omitempty"`
	// At is the repair's Unix timestamp.
	At int64 `json:"at"`
}

// Salvaged returns the total number of rows copied across all tables.
func (s RepairStats) Salvaged() int {
	n := 0
	for _, t := range s.Tables {
		n += t.Salvaged
	}
	return n
}

// String renders a one-line summary suitable for a log notice.
func (s RepairStats) String() string {
	names := make([]string, 0, len(s.Tables))
	for k, v := range s.Tables {
		if v.Salvaged > 0 || v.Lost > 0 {
			names = append(names, fmt.Sprintf("%s=%d", k, v.Salvaged))
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("source_readable=%v rows=%d [%s] quarantine=%s",
		s.SourceOpen, s.Salvaged(), strings.Join(names, " "), s.Quarantine)
}

// ErrRepairContended is returned when another repairer already holds the
// store's repair lock. Callers should wait for it to finish (waitForRepair) and
// then simply re-open — the winner has probably fixed the store.
var ErrRepairContended = errors.New("store: a repair of this store is already in progress")

// preciousTables are the tables salvaged into the replacement, in
// foreign-key-safe insertion order (parents before children). Everything not
// listed here is derived state. This deliberately omits memory_vectors: they
// are derived embeddings, never salvaged, and lazily re-embedded after repair.
// The FTS vocabulary view is likewise recreated from memories_fts.
//
// graph_deltas is precious despite its name: it is a historical journal of
// symbol-level index changes read by graph.Engine.DeltaBaseline/DeltasSince
// alongside sessions, NOT rebuildable graph state (migration v19 introduced it
// specifically outside the wipe set).
//
// The sync server's sync_index is deliberately absent: it is server-only (not in
// the client migration set, so a freshly migrated replacement has no such table
// to copy into), and a PARTIALLY salvaged index is worse than none — rows missing
// from it would silently vanish from every future pull. The server rebuilds it on
// open instead, via syncserver.reconcileSyncIndex.
var preciousTables = []string{
	"meta",
	"counters",
	"sessions",
	"memories",
	"memory_refs",
	"memory_minhash",
	"memory_tombstones",
	"memory_sync_outbox",
	"memory_sync_merges",
	"graph_deltas",
}

// salvageWhere restricts what is copied out of a table.
//
// meta drops the per-worktree graph freshness tokens: the replacement's graph is
// empty, and a surviving graph_token:<worktree> would make it look freshly
// indexed and suppress the lazy rebuild that repopulates it.
var salvageWhere = map[string]string{
	"meta": `key NOT LIKE 'graph_token:%'`,
}

// orphanCleanup removes children whose parent row could not be salvaged. The
// replacement is built with foreign keys OFF (salvage must not fail a whole
// table because one parent row was unreadable), so referential integrity is
// restored explicitly before the file is put into service.
var orphanCleanup = []struct{ table, sql string }{
	{"memory_refs", `DELETE FROM memory_refs        WHERE memory_id NOT IN (SELECT id FROM memories)`},
	{"memory_minhash", `DELETE FROM memory_minhash     WHERE memory_id NOT IN (SELECT id FROM memories)`},
	{"memory_sync_outbox", `DELETE FROM memory_sync_outbox WHERE memory_id NOT IN (SELECT id FROM memories)`},
}

// quarantineRetention is how long quarantined corrupt files are kept. The
// newest set is always kept regardless of age.
const quarantineRetention = 7 * 24 * time.Hour

// The repair lock's liveness parameters. They are variables, not constants, so
// tests can shrink them to milliseconds instead of sleeping for real minutes.
var (
	// repairHeartbeatInterval is how often the lock's owner refreshes its mtime.
	// This is what makes "the mtime is fresh" mean "a repairer is alive", so a
	// repair of any duration keeps its lock honoured.
	repairHeartbeatInterval = 5 * time.Second
	// repairLockStaleAfter is the mtime age at which a lock is considered
	// abandoned (its owner was killed, the machine crashed). It must be a
	// comfortable multiple of the heartbeat so a merely slow scheduler never
	// looks like a dead process.
	repairLockStaleAfter = 90 * time.Second
)

// repairVerify runs the final integrity check on the freshly built replacement.
// It is a package variable so a test can force verification to fail and prove a
// failed repair leaves the original file untouched.
var repairVerify = func(ctx context.Context, db *sql.DB) error {
	return checkIntegrity(ctx, db, `PRAGMA integrity_check`)
}

// AutoRepairEnabled reports whether automatic (non-manual) repair is permitted.
// RECARTO_AUTO_REPAIR=0 disables it; `recarto repair` still works.
func AutoRepairEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RECARTO_AUTO_REPAIR"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// Repair rebuilds the store file at path from whatever is still readable in it.
//
// It is unconditional — there is no corruption precondition — so it doubles as
// the "give me a clean store, keeping my memories" tool. The caller must have no
// connection pool of its own open on path.
//
// On success the corrupt file (with its -wal/-shm siblings) has been renamed to
// RepairStats.Quarantine and a freshly migrated database containing the salvaged
// rows sits at path. On failure nothing at path has been modified.
func Repair(path string) (RepairStats, error) {
	stats := RepairStats{Tables: map[string]TableStats{}, At: time.Now().Unix()}

	release, err := acquireRepairLock(path)
	if err != nil {
		return stats, err
	}
	defer release()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return stats, fmt.Errorf("create cache dir: %w", err)
	}

	tmp := path + ".repair.tmp"
	removeDBFiles(tmp)

	ctx := context.Background()
	if err := buildReplacement(ctx, path, tmp, &stats); err != nil {
		removeDBFiles(tmp)
		return stats, err
	}

	// Every handle on both the source and the replacement is closed by now, so
	// the file set can be moved. Quarantine first: if that fails nothing has
	// changed, and the freshly built replacement is discarded.
	q, err := quarantineExisting(path, stats.At)
	if err != nil {
		removeDBFiles(tmp)
		return stats, err
	}
	stats.Quarantine = q

	if err := os.Rename(tmp, path); err != nil {
		// Put the original back so a failed swap is a no-op (this is the
		// Windows failure mode: renaming over open handles is refused).
		removeDBFiles(tmp)
		if rerr := restoreQuarantine(q, path); rerr != nil {
			// The rollback failed too: the store is NOT where it was. Keep
			// Quarantine set so the caller can tell the user exactly where their
			// data is.
			return stats, fmt.Errorf(
				"install repaired store: %w; the original store could not be restored and remains at %s (%v)",
				err, q, rerr)
		}
		stats.Quarantine = ""
		return stats, fmt.Errorf("install repaired store: %w", err)
	}

	pruneQuarantines(path)
	return stats, nil
}

// buildReplacement creates a freshly migrated database at tmp, salvages what it
// can out of srcPath into it, verifies it, and closes every handle it opened —
// on both files — before returning. A returned error means tmp is unusable and
// srcPath was never modified beyond SQLite's own WAL recovery on read.
func buildReplacement(ctx context.Context, srcPath, tmp string, stats *RepairStats) error {
	dst, err := openRepairTarget(tmp)
	if err != nil {
		return fmt.Errorf("create replacement store: %w", err)
	}
	// Closed explicitly before the verification re-open below; the deferred
	// close is the error-path safety net (double Close is harmless).
	defer dst.Close()

	if err := Migrate(dst); err != nil {
		return fmt.Errorf("migrate replacement store: %w", err)
	}

	if err := salvageInto(ctx, srcPath, dst, stats); err != nil {
		return err
	}

	for _, q := range orphanCleanup {
		res, err := dst.ExecContext(ctx, q.sql)
		if err != nil {
			return fmt.Errorf("drop orphaned rows: %w", err)
		}
		// A child kept while its parent was lost would be reported as salvaged
		// but is really gone, so the stats follow the deletion.
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			continue
		}
		ts := stats.Tables[q.table]
		ts.Salvaged -= int(n)
		ts.Lost += int(n)
		if ts.Note == "" {
			ts.Note = "orphaned by unsalvageable parent memories"
		}
		stats.Tables[q.table] = ts
	}

	// Rebuild the memory FTS index from the salvaged rows. search_text is stored
	// on the memories row, so the index is fully derivable. Only ACTIVE memories
	// are indexed: every write path (supersede, archive, todo done, feedback
	// resolve) deletes terminal rows from memories_fts, and a rebuild that
	// re-indexed them would resurrect them as lexical candidates.
	if _, err := dst.ExecContext(ctx,
		`INSERT INTO memories_fts(memory_id, search_text)
		 SELECT id, search_text FROM memories WHERE status = 'active'`); err != nil {
		return fmt.Errorf("rebuild memory index: %w", err)
	}

	// Repair metadata is written BEFORE verification and the checkpoint, so it is
	// part of what gets verified and flushed rather than a straggler in a WAL
	// nobody checkpoints.
	if err := writeRepairMeta(ctx, dst, *stats); err != nil {
		return err
	}

	if err := repairVerify(ctx, dst); err != nil {
		return err
	}

	// The replacement is in WAL mode, so its committed pages may live only in
	// <tmp>-wal. Fold them into the main file and drop the WAL before the rename
	// — moving <tmp> alone would install a partial (possibly empty) database.
	if _, err := dst.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint replacement store: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close replacement store: %w", err)
	}
	if fi, err := os.Stat(tmp + "-wal"); err == nil && fi.Size() > 0 {
		return fmt.Errorf("replacement store still has an uncheckpointed WAL (%d bytes)", fi.Size())
	}

	// Final proof on a completely fresh connection — the state a later process
	// sees. This is what catches data stranded in a WAL that was not folded in.
	return verifyFresh(ctx, tmp)
}

// verifyFresh re-opens path from scratch and confirms it is a healthy, migrated
// store whose salvaged rows are visible.
func verifyFresh(ctx context.Context, path string) error {
	db, err := openRepairTarget(path)
	if err != nil {
		return fmt.Errorf("verify repaired store: %w", err)
	}
	defer db.Close()
	if err := checkIntegrity(ctx, db, `PRAGMA quick_check`); err != nil {
		return fmt.Errorf("verify repaired store: %w", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
		return fmt.Errorf("verify repaired store: %w", err)
	}
	var uv int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&uv); err != nil {
		return fmt.Errorf("verify repaired store: %w", err)
	}
	if uv != schemaVersion {
		return fmt.Errorf("verify repaired store: user_version=%d, want %d", uv, schemaVersion)
	}
	return db.Close()
}

// checkIntegrity runs a PRAGMA integrity/quick check and turns any finding into
// an errCorruptCheck (which IsCorrupt recognises). A healthy database answers
// with the single row "ok".
func checkIntegrity(ctx context.Context, db *sql.DB, pragma string) error {
	rows, err := db.QueryContext(ctx, pragma)
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		if s != "ok" {
			msgs = append(msgs, s)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	if len(msgs) > 5 {
		msgs = append(msgs[:5], fmt.Sprintf("(+%d more)", len(msgs)-5))
	}
	return &errCorruptCheck{detail: strings.Join(msgs, "; ")}
}

// listSourceTables enumerates the source's tables. It is a package variable so a
// test can force enumeration to fail and prove salvage still runs.
var listSourceTables = sourceTables

// salvageInto copies the precious tables out of the (corrupt) database at
// srcPath into dst. A source that cannot be opened at all is not an error:
// availability wins, the replacement is simply empty, stats.SourceOpen is false
// and the quarantined original retains everything for later forensics.
func salvageInto(ctx context.Context, srcPath string, dst *sql.DB, stats *RepairStats) error {
	if _, err := os.Stat(srcPath); err != nil {
		return nil // nothing to salvage: a brand-new store
	}
	src, err := openRepairSource(srcPath)
	if err != nil {
		return nil
	}
	defer src.Close()

	// sql.Open is lazy, so "did it open" must be answered by an actual schema
	// query. A FAILING enumeration is not proof that nothing is readable, though:
	// a full sqlite_master scan can die on one damaged page while PRAGMA
	// table_info — which reads a single schema row — still answers for the tables
	// we actually want. So an enumeration failure downgrades to "try every
	// precious table blind" rather than writing off the data; salvageTable
	// already degrades per table.
	present, listErr := listSourceTables(ctx, src)

	for _, t := range preciousTables {
		if listErr == nil && !present[t] {
			// An older store predating this table (sync tables arrived in v20/v21,
			// graph_deltas in v19), or a table whose schema row is unreadable.
			stats.Tables[t] = TableStats{Note: "absent from source schema"}
			continue
		}
		stats.Tables[t] = salvageTable(ctx, src, dst, t)
	}

	stats.SourceOpen = listErr == nil
	if !stats.SourceOpen {
		for _, ts := range stats.Tables {
			if ts.Salvaged > 0 {
				stats.SourceOpen = true
				break
			}
		}
	}
	return src.Close()
}

// salvageTable copies one table, tolerating both schema drift and mid-scan
// failure.
//
// Schema drift matters because repair can run while Migrate itself is failing,
// so the source is not guaranteed to be at the current schema version (memories
// gained columns in v4, the sync tables only exist from v20). Only the columns
// the two schemas share are copied; the destination's own defaults fill the rest.
//
// A cursor that dies part-way through keeps the rows already read — that is the
// whole point of a row-by-row salvage — and the shortfall is recorded in Lost.
func salvageTable(ctx context.Context, src, dst *sql.DB, table string) TableStats {
	var ts TableStats

	srcCols, err := tableColumns(ctx, src, table)
	if err != nil {
		ts.Note = "unreadable source schema: " + err.Error()
		return ts
	}
	dstCols, err := tableColumns(ctx, dst, table)
	if err != nil {
		ts.Note = "missing in replacement schema: " + err.Error()
		return ts
	}
	have := map[string]bool{}
	for _, c := range srcCols {
		have[c] = true
	}
	var cols []string
	for _, c := range dstCols {
		if have[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		ts.Note = "no columns in common with replacement schema"
		return ts
	}

	where := ""
	if w := salvageWhere[table]; w != "" {
		where = " WHERE " + w
	}
	// Best effort: a count that itself hits the damage just leaves Lost at 0.
	total := -1
	if err := src.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %q%s`, table, where)).Scan(&total); err != nil {
		total = -1
	}

	quoted := make([]string, len(cols))
	ph := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf("%q", c)
		ph[i] = "?"
	}

	rows, err := src.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %q%s`, strings.Join(quoted, ", "), table, where))
	if err != nil {
		ts.Note = "unreadable: " + err.Error()
		if total > 0 {
			ts.Lost = total
		}
		return ts
	}
	defer rows.Close()

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		ts.Note = "replacement transaction: " + err.Error()
		return ts
	}
	// INSERT OR IGNORE so a single row violating a UNIQUE/CHECK constraint (a
	// garbled kind, a duplicated primary key) is skipped instead of aborting the
	// whole table.
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT OR IGNORE INTO %q (%s) VALUES (%s)`,
		table, strings.Join(quoted, ", "), strings.Join(ph, ", ")))
	if err != nil {
		_ = tx.Rollback()
		ts.Note = "replacement insert: " + err.Error()
		return ts
	}

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	// rejected counts rows the INSERT OR IGNORE silently dropped (a UNIQUE, CHECK
	// or NOT NULL violation); failed counts rows whose exec errored outright.
	// Both are rows that were read but did NOT land, so neither may be counted as
	// salvaged — otherwise the stats, the repair metadata and `recarto repair`'s
	// output would all overstate what was recovered.
	var rejected, failed int
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			ts.Note = "truncated mid-scan: " + err.Error()
			break
		}
		res, err := stmt.ExecContext(ctx, vals...)
		if err != nil {
			failed++
			continue
		}
		// modernc always reports RowsAffected; if some future driver does not,
		// trust the successful exec rather than under-reporting the salvage.
		if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
			rejected++
			continue
		}
		ts.Salvaged++
	}
	if err := rows.Err(); err != nil && ts.Note == "" {
		ts.Note = "truncated mid-scan: " + err.Error()
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		ts.Salvaged = 0
		ts.Note = "replacement commit: " + err.Error()
		if total > 0 {
			ts.Lost = total
		}
		return ts
	}
	switch {
	case total > ts.Salvaged:
		ts.Lost = total - ts.Salvaged
	case total < 0:
		// The row count was itself unreadable, so the only losses we can name are
		// the ones we watched happen.
		ts.Lost = rejected + failed
	}
	if n := rejected + failed; n > 0 && ts.Note == "" {
		ts.Note = fmt.Sprintf("%d rows rejected by replacement constraints", n)
	}
	return ts
}

// sourceTables lists the tables (including virtual tables) present in db.
func sourceTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no tables")
	}
	return out, nil
}

// tableColumns returns table's column names in declaration order.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %q has no readable columns", table)
	}
	return cols, nil
}

// writeRepairMeta records when the store was last repaired and with what result,
// so `recarto doctor` can surface it and a recurring corruption source is
// visible rather than silently papered over.
func writeRepairMeta(ctx context.Context, db *sql.DB, stats RepairStats) error {
	blob, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"repair_last_ts", strconv.FormatInt(stats.At, 10)},
		{"repair_last_stats", string(blob)},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv[0], kv[1]); err != nil {
			return fmt.Errorf("record repair metadata: %w", err)
		}
	}
	return nil
}

// dbSuffixes are the sidecar files that belong to a SQLite database's inode. A
// stale -wal left next to a NEW file of the same name is actively dangerous, so
// they always travel with the main file.
var dbSuffixes = []string{"", "-wal", "-shm"}

// dbSidecars is dbSuffixes without the main file.
var dbSidecars = []string{"-wal", "-shm"}

// removeDBFiles deletes path and its WAL/SHM siblings, ignoring absence.
func removeDBFiles(path string) {
	for _, sfx := range dbSuffixes {
		_ = os.Remove(path + sfx)
	}
}

// quarantineExisting renames the store file set at path aside to
// <path>.corrupt-<unixts>, returning the new base path ("" when there was no
// file to move). The corrupt file is never deleted here — it is the only
// evidence of what went wrong, and the only chance of a deeper manual recovery.
func quarantineExisting(path string, ts int64) (string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	q := fmt.Sprintf("%s.corrupt-%d", path, ts)
	for i := 1; ; i++ {
		if _, err := os.Stat(q); os.IsNotExist(err) {
			break
		}
		q = fmt.Sprintf("%s.corrupt-%d.%d", path, ts, i)
	}
	if err := os.Rename(path, q); err != nil {
		return "", fmt.Errorf("quarantine corrupt store: %w", err)
	}
	// Moving the sidecars is MANDATORY, not best effort. A stale -wal left beside
	// the freshly installed replacement is not litter: SQLite will happily replay
	// it into the new file and re-corrupt it. And the quarantined WAL may hold the
	// most recent commits, so it is forensic evidence — moved, never deleted.
	// (-shm is the sole exception: it is pure shared-memory scratch for a live
	// connection and is rebuilt on demand, so if it cannot be moved it may be
	// removed instead.)
	var moved []string
	for _, sfx := range dbSidecars {
		if _, err := os.Stat(path + sfx); err != nil {
			continue
		}
		mvErr := os.Rename(path+sfx, q+sfx)
		if mvErr == nil {
			moved = append(moved, sfx)
			continue
		}
		if sfx == "-shm" {
			if os.Remove(path+sfx) == nil {
				continue
			}
		}
		// Roll the whole quarantine back so the caller's "nothing was touched"
		// invariant still holds, then report which sidecar blocked it.
		for _, m := range moved {
			_ = os.Rename(q+m, path+m)
		}
		if rerr := os.Rename(q, path); rerr != nil {
			return "", fmt.Errorf(
				"quarantine corrupt store: could not move %s aside (%v), and rolling back %s to %s also failed: %w",
				path+sfx, mvErr, q, path, rerr)
		}
		return "", fmt.Errorf("quarantine corrupt store: could not move %s aside: %w", path+sfx, mvErr)
	}
	return q, nil
}

// restoreQuarantine moves a quarantined file set back to path — the rollback for
// a swap that failed after the quarantine rename succeeded. Its errors matter:
// if the main file cannot be moved back, the user's only copy of their memories
// is sitting at q while path is empty, and they have to be told.
func restoreQuarantine(q, path string) error {
	if q == "" {
		return nil
	}
	if err := os.Rename(q, path); err != nil {
		return fmt.Errorf("could not restore the original store from %s to %s: %w", q, path, err)
	}
	var errs []error
	for _, sfx := range dbSidecars {
		if _, err := os.Stat(q + sfx); err != nil {
			continue
		}
		if err := os.Rename(q+sfx, path+sfx); err != nil {
			errs = append(errs, fmt.Errorf("could not restore %s: %w", q+sfx, err))
		}
	}
	return errors.Join(errs...)
}

// pruneQuarantines deletes quarantined file sets older than quarantineRetention,
// always keeping the newest one — bounded disk use, still inspectable.
func pruneQuarantines(path string) {
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		return
	}
	type q struct {
		base string
		ts   int64
	}
	var sets []q
	for _, m := range matches {
		if strings.HasSuffix(m, "-wal") || strings.HasSuffix(m, "-shm") {
			continue
		}
		rest := strings.TrimPrefix(m, path+".corrupt-")
		if i := strings.IndexByte(rest, '.'); i >= 0 {
			rest = rest[:i]
		}
		ts, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			continue
		}
		sets = append(sets, q{base: m, ts: ts})
	}
	if len(sets) <= 1 {
		return
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].ts > sets[j].ts })
	cutoff := time.Now().Add(-quarantineRetention).Unix()
	for _, s := range sets[1:] { // sets[0] is the newest and is always kept
		if s.ts < cutoff {
			removeDBFiles(s.base)
		}
	}
}

// repairLockPath is the sidecar lockfile serialising repairers of path.
func repairLockPath(path string) string { return path + ".repairlock" }

// acquireRepairLock takes the exclusive repair lock for path and starts its
// heartbeat. A lock whose mtime is younger than repairLockStaleAfter belongs to
// a live repairer and yields ErrRepairContended; an older one was abandoned by a
// killed process and is claimed.
//
// The returned release stops the heartbeat and only then removes the lockfile,
// so no stray Chtimes can resurrect a released lock.
func acquireRepairLock(path string) (release func(), err error) {
	lock := repairLockPath(path)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid=%d at=%d\n", os.Getpid(), time.Now().Unix())
			_ = f.Close()
			return startRepairHeartbeat(lock), nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		fi, statErr := os.Stat(lock)
		if statErr != nil {
			continue // vanished between OpenFile and Stat: retry the create
		}
		if time.Since(fi.ModTime()) < repairLockStaleAfter {
			return nil, ErrRepairContended
		}
		// Abandoned. Claim it by RENAME rather than remove: rename is atomic, so
		// exactly one of several competitors succeeds and the losers re-evaluate
		// from the top. A plain remove would let two processes each delete "the"
		// lock, both create a fresh one, and run overlapping repairs.
		//
		// Residual race, accepted: an owner that was frozen for longer than
		// repairLockStaleAfter (a paused VM, a stop-the-world pause) could resume
		// heartbeating a lock that has already been claimed. That window is
		// strictly narrower than the mtime-only steal it replaces, and the loser
		// of the rename never proceeds.
		claimed := fmt.Sprintf("%s.stale-%d", lock, os.Getpid())
		if err := os.Rename(lock, claimed); err != nil {
			continue // someone else claimed it first; re-evaluate
		}
		_ = os.Remove(claimed)
	}
	return nil, ErrRepairContended
}

// startRepairHeartbeat keeps lock's mtime fresh for as long as the repair runs,
// and returns the release func that stops it and removes the lockfile.
func startRepairHeartbeat(lock string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(repairHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := time.Now()
				_ = os.Chtimes(lock, now, now)
			}
		}
	}()
	return func() {
		close(stop)
		<-done // no Chtimes can be in flight past this point
		_ = os.Remove(lock)
	}
}

// waitForRepair blocks while a live repair lock exists for path, so an ordinary
// opener never touches the store during the quarantine/rename window.
//
// There is deliberately NO deadline: because the owner heartbeats the lockfile,
// a fresh mtime is positive evidence that a repair is still running, and giving
// up on it would mean opening the store mid-swap. The wait is therefore bounded
// by the repair itself, and a repairer that dies unblocks every waiter within
// repairLockStaleAfter.
func waitForRepair(path string) {
	lock := repairLockPath(path)
	poll := repairHeartbeatInterval / 4
	if poll > 250*time.Millisecond {
		poll = 250 * time.Millisecond
	}
	if poll < time.Millisecond {
		poll = time.Millisecond
	}
	for {
		fi, err := os.Stat(lock)
		if err != nil || time.Since(fi.ModTime()) >= repairLockStaleAfter {
			return
		}
		time.Sleep(poll)
	}
}
