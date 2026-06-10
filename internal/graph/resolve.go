package graph

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Resolve links reference edges to a defining node within the same worktree.
// Non-call edges (extends/implements/imports/references) bind dst_node_id by
// matching dst_name to a node name/qualified_name, scoped to the language FAMILY
// ({csharp}, {go}, web {typescript,tsx,javascript}) so a C# `.Select(...)` can
// never bind to a React `Select`, preferring same-file then same-directory then a
// stable id order. Call edges are resolved into a confidence-ranked candidate set
// (edge_candidates) by the strategy passes in callStrategies; edges.dst_node_id
// is then denormalized to the best candidate so single-target readers keep
// working. dst_name/dst_recv are retained so trace fallbacks still work where
// resolution is partial.
func (e *Engine) Resolve(ctx context.Context, worktree string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		// A single-worktree store (the common `recarto index` case) owns every
		// edge_candidate and call edge, so the clear below can use a table-wide
		// truncate instead of probing edge_candidates once per call edge — the
		// per-edge filtered delete cost ~25s on graphql-platform's re-resolve.
		var hasOthers int
		if err := c.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM files WHERE worktree <> ?)`, worktree).Scan(&hasOthers); err != nil {
			return err
		}
		singleWorktree := hasOthers == 0

		// idx_edge_candidates_dst (dst_node_id) exists for cascade-on-node-delete and
		// the candidate/trace readers, but NO resolve query uses it: the strategies
		// dedup via the PRIMARY KEY (edge_id, dst_node_id), and S5's NOT EXISTS, the
		// denormalize, and the clear all key on edge_id. Maintaining it across the
		// ~900k candidate inserts (and the clear's deletes) is pure overhead, so drop
		// it for the duration and rebuild it once at the end. The DROP/CREATE live in
		// this write tx, so WAL readers keep the committed index until COMMIT then see
		// the rebuilt one; a rolled-back attempt restores it. Incremental resolve
		// touches few rows and leaves the index in place.
		if _, err := c.ExecContext(ctx, `DROP INDEX IF EXISTS idx_edge_candidates_dst`); err != nil {
			return err
		}

		// Idempotent re-resolve: clear this worktree's call candidates and reset
		// the call edges' denormalized target before recomputing. The
		// `dst_node_id IS NOT NULL` guard skips rewriting already-NULL rows — every
		// call edge on a first index — turning a 300k-row no-op UPDATE into nothing.
		if singleWorktree {
			if _, err := c.ExecContext(ctx, `DELETE FROM edge_candidates`); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx,
				`UPDATE edges SET dst_node_id = NULL WHERE kind = 'calls' AND dst_node_id IS NOT NULL`); err != nil {
				return err
			}
		} else {
			if _, err := c.ExecContext(ctx, `
DELETE FROM edge_candidates WHERE edge_id IN (
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.kind = 'calls')`, worktree); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = NULL
 WHERE kind = 'calls' AND dst_node_id IS NOT NULL
   AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree); err != nil {
				return err
			}
		}

		// Non-call edges: unchanged name-match resolution.
		if err := resolveNonCallEdges(ctx, c, worktree); err != nil {
			return err
		}

		// The typed-receiver (S2) and interface (S4) strategies share an identical,
		// expensive prefix: resolving each receiver call's receiver token — via a
		// type_binding whose scope contains the calling method — to a bound type.
		// That scope-containment join over every receiver call dominates resolve
		// (~14s each on graphql-platform), so compute it ONCE into a temp table the
		// two strategies read, instead of once per strategy. DISTINCT collapses
		// duplicate (edge,type) rows from overlapping in-scope bindings; the
		// downstream INSERT OR IGNORE would dedup the resulting candidates anyway,
		// so the candidate set is byte-for-byte identical.
		if err := buildReceiverTypes(ctx, c, worktree); err != nil {
			return err
		}

		// Call edges: candidate strategies, highest-confidence first. OR IGNORE keeps
		// the first (best) confidence for a given (edge, target) pair, so the ORDER —
		// S1 self, S2 typed, S3 static, S4 iface, S5 name — is significant and must be
		// preserved. S2/S4 read recv_types (one bound arg); the rest scope by
		// (worktree, worktree).
		if _, err := c.ExecContext(ctx, stratSelf, worktree, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, stratTypedReceiverShared, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, stratStatic, worktree, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, stratInterfaceShared, worktree); err != nil {
			return err
		}
		if err := resolveNameFallback(ctx, c, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, stratImplicitCtor, worktree, worktree); err != nil {
			return err
		}

		// Denormalize: edges.dst_node_id = best candidate per call edge — highest
		// confidence, then locality (same-file, then same-top-level-directory),
		// then a stable id order. Locality is the tiebreak among equal-confidence
		// candidates (e.g. several S5 name matches), mirroring the non-call
		// resolver so a sibling call binds to its own file's definition. Computed in
		// one in-memory pass; relies on the clear above having NULLed every call edge.
		if err := denormalizeBestCandidate(ctx, c, worktree); err != nil {
			return err
		}
		// Rebuild the dst_node_id index dropped above, in one bulk pass over the final
		// candidate set rather than incrementally across every insert.
		if _, err := c.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS idx_edge_candidates_dst ON edge_candidates(dst_node_id)`); err != nil {
			return err
		}
		// Refresh planner statistics over the rewritten graph. The store never ran
		// ANALYZE before, so every query was costed from guesses — which is one bad
		// estimate away from a table scan (the read paths that matter are CROSS
		// JOIN-pinned, but ad-hoc shapes like query_graph filters are not).
		// analysis_limit bounds the per-index scan so this stays sub-second even on
		// a multi-hundred-MB store (SQLite's recommended production setting).
		// Incremental resolve skips this: stats from the last full pass stay valid.
		if _, err := c.ExecContext(ctx, `PRAGMA analysis_limit=1000`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `ANALYZE`); err != nil {
			return err
		}
		// Drop the shared prefix temp table so it does not linger on this pooled
		// connection after the transaction commits.
		_, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`)
		return err
	})
}

// resolveNonCallEdges binds dst_node_id for every non-call reference edge by
// matching dst_name to a node name/qualified_name within the language family,
// preferring same-file then same-top-level-directory then a stable id order.
func resolveNonCallEdges(ctx context.Context, c store.Conn, worktree string) error {
	_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT n.id
    FROM nodes n
    JOIN files f  ON f.id = n.file_id
    JOIN files ef ON ef.id = edges.file_id
   WHERE f.worktree = ?
     AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
     AND ( ef.language = f.language
           OR ( ef.language IN ('typescript','tsx','javascript')
                AND f.language IN ('typescript','tsx','javascript') ) )
   ORDER BY
     CASE WHEN f.id = edges.file_id THEN 0 ELSE 1 END,
     -- same top-level directory next. instr(path,'/')=0 for a root-level file, and
     -- substr(s,1,0)='' in SQLite, so two root-level files compare equal here —
     -- correct "same directory" semantics.
     CASE WHEN substr(f.path,1,instr(f.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
          THEN 0 ELSE 1 END,
     -- Stable tiebreak (see the call-edge denormalize): reproducible across indexes.
     f.path, n.start_byte, n.id
   LIMIT 1)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND kind <> 'calls'
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree, worktree)
	return err
}

// buildReceiverTypes (re)creates the recv_types temp table: for every receiver
// call in the worktree, the bound type(s) of its receiver token, resolved via a
// type_binding whose scope contains the calling method. This is the shared,
// dominant-cost prefix of the typed-receiver (S2) and interface (S4) strategies,
// materialized once so neither recomputes it. DISTINCT collapses duplicate
// (edge, type) rows that overlapping in-scope bindings would yield — harmless,
// because both strategies' INSERT OR IGNORE dedups candidates by the
// (edge_id, dst_node_id) primary key, so the candidate set is unchanged.
//
// The membership test is an interval-CONTAINMENT join
// (tb.scope_start_byte <= src.start_byte AND src.end_byte <= tb.scope_end_byte),
// which SQLite cannot index: a hot (file, receiver-name) bucket holds thousands
// of bindings, so the per-call seek scans the whole bucket and the SQL form cost
// ~18s on graphql-platform — the single largest resolve step. Computing it in Go
// over in-memory maps (the whole graph fits in RAM) collapses that to ~1s: bucket
// the bindings by (file_id, name) once, then test each receiver call against only
// its own bucket. The materialized rows are identical, so S2/S4 (still SQL, still
// reading recv_types) emit a byte-for-byte identical candidate set.
func buildReceiverTypes(ctx context.Context, c store.Conn, worktree string) error {
	if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`); err != nil {
		return err
	}
	// Explicit TEXT columns matching the affinities the old CREATE ... AS SELECT
	// inferred (edges.id, edges.dst_name, files.language, type_bindings.type_name),
	// so the downstream joins on rt.* behave identically.
	if _, err := c.ExecContext(ctx, `CREATE TEMP TABLE recv_types (
  edge_id     TEXT,
  dst_name    TEXT,
  ef_language TEXT,
  type_name   TEXT)`); err != nil {
		return err
	}

	// Load the worktree slice of the graph into memory. The single write
	// connection cannot hold an open cursor while inserting, so every result set
	// is fully drained and closed before the next query or the insert loop runs.

	// file_id -> language (worktree files only); also the worktree file set.
	fileLang := map[string]string{}
	if err := scan(ctx, c, `SELECT id, language FROM files WHERE worktree = ?`,
		func(rows *sql.Rows) error {
			var id, lang string
			if err := rows.Scan(&id, &lang); err != nil {
				return err
			}
			fileLang[id] = lang
			return nil
		}, worktree); err != nil {
		return err
	}

	// node id -> [start_byte, end_byte], to test scope containment of the caller.
	type span struct{ start, end int64 }
	nodeSpan := make(map[string]span, 1<<17)
	if err := scan(ctx, c, `SELECT id, start_byte, end_byte FROM nodes`,
		func(rows *sql.Rows) error {
			var id string
			var s span
			if err := rows.Scan(&id, &s.start, &s.end); err != nil {
				return err
			}
			nodeSpan[id] = s
			return nil
		}); err != nil {
		return err
	}

	// (file_id, receiver name) -> bindings in the worktree.
	type binding struct {
		typeName   string
		start, end int64
	}
	type fileName struct{ file, name string }
	bindings := map[fileName][]binding{}
	if err := scan(ctx, c, `
SELECT tb.file_id, tb.name, tb.type_name, tb.scope_start_byte, tb.scope_end_byte
  FROM type_bindings tb JOIN files f ON f.id = tb.file_id
 WHERE f.worktree = ?`,
		func(rows *sql.Rows) error {
			var k fileName
			var b binding
			if err := rows.Scan(&k.file, &k.name, &b.typeName, &b.start, &b.end); err != nil {
				return err
			}
			bindings[k] = append(bindings[k], b)
			return nil
		}, worktree); err != nil {
		return err
	}

	// Receiver call edges in the worktree, materialized so the cursor is closed
	// before inserting (one connection cannot read and write concurrently).
	type recvEdge struct {
		id, dstName, dstRecv, fileID string
		src                          sql.NullString
	}
	var edges []recvEdge
	if err := scan(ctx, c, `
SELECT ed.id, ed.src_node_id, ed.dst_name, ed.dst_recv, ed.file_id
  FROM edges ed JOIN files f ON f.id = ed.file_id
 WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_recv <> ''`,
		func(rows *sql.Rows) error {
			var e recvEdge
			if err := rows.Scan(&e.id, &e.src, &e.dstName, &e.dstRecv, &e.fileID); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}, worktree); err != nil {
		return err
	}

	stmt, err := c.PrepareContext(ctx,
		`INSERT INTO recv_types(edge_id, dst_name, ef_language, type_name) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	seen := map[string]struct{}{} // DISTINCT type per edge; reused across edges
	for i := range edges {
		e := &edges[i]
		if !e.src.Valid {
			continue // file-scope call: no enclosing definition to scope-test
		}
		sp, ok := nodeSpan[e.src.String]
		if !ok {
			continue
		}
		bs := bindings[fileName{e.fileID, e.dstRecv}]
		if len(bs) == 0 {
			continue
		}
		lang := fileLang[e.fileID]
		for k := range seen {
			delete(seen, k)
		}
		for _, b := range bs {
			if b.start <= sp.start && sp.end <= b.end {
				if _, dup := seen[b.typeName]; dup {
					continue
				}
				seen[b.typeName] = struct{}{}
				if _, err := stmt.ExecContext(ctx, e.id, e.dstName, lang, b.typeName); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// scan runs query and invokes fn for each row, draining and closing the cursor
// before returning — the pattern the single write connection requires (it cannot
// hold an open Rows while a later query or insert runs on the same connection).
func scan(ctx context.Context, c store.Conn, query string, fn func(*sql.Rows) error, args ...any) error {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// resolveNameFallback is the Go form of stratNameFallback (S5): for every call
// edge that no higher-confidence strategy matched, bind it to same-name callables
// in the language family, capped at 10 per edge in locality order, with a
// confidence that reflects ambiguity (a unique match is 0.45, several are 0.2).
//
// It is the name-only fallback and by far the largest candidate producer, so the
// SQL form's PARTITION-BY window sort over ~786k rows was the largest resolve step
// (~19-23s). Computing it over in-memory maps — same global hyper-common-name skip,
// same language family, same per-edge locality order and 10-cap — emits a
// byte-identical candidate set far faster; the floor becomes the unavoidable
// ~786k-row insert. Must run after S1-S4 (it reads their candidates to skip edges
// already resolved) and before the denormalize.
func resolveNameFallback(ctx context.Context, c store.Conn, worktree string) error {
	// Names shared by more than 50 callables anywhere in the store are skipped: a
	// name-only match on them is pure noise, and their full (edge x same-name)
	// product is what made the SQL form blow up. The SQL counts globally (no
	// worktree filter), so this must too.
	common := map[string]struct{}{}
	if err := scan(ctx, c, `
SELECT name FROM nodes
 WHERE kind IN ('method','property','constructor','function')
 GROUP BY name HAVING COUNT(*) > 50`,
		func(rows *sql.Rows) error {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			common[n] = struct{}{}
			return nil
		}); err != nil {
		return err
	}

	// Edges already carrying a candidate (resolved by S1-S4) are excluded — S5 is
	// the fallback only where nothing better matched (the SQL's NOT EXISTS).
	resolved := map[string]struct{}{}
	if err := scan(ctx, c, `SELECT DISTINCT edge_id FROM edge_candidates`,
		func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			resolved[id] = struct{}{}
			return nil
		}); err != nil {
		return err
	}

	// Worktree files: path + language for the locality and language-family tests.
	type fileInfo struct{ path, lang string }
	files := map[string]fileInfo{}
	if err := scan(ctx, c, `SELECT id, path, language FROM files WHERE worktree = ?`,
		func(rows *sql.Rows) error {
			var id string
			var fi fileInfo
			if err := rows.Scan(&id, &fi.path, &fi.lang); err != nil {
				return err
			}
			files[id] = fi
			return nil
		}, worktree); err != nil {
		return err
	}

	// Callable nodes in the worktree, indexed by name (the S5 match target set;
	// the SQL requires mf.worktree = worktree, satisfied by the join filter here).
	type matchNode struct {
		id, fileID string
		start      int64
	}
	byName := map[string][]matchNode{}
	if err := scan(ctx, c, `
SELECT n.id, n.name, n.file_id, n.start_byte
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?
   AND n.kind IN ('method','property','constructor','function')`,
		func(rows *sql.Rows) error {
			var name string
			var m matchNode
			if err := rows.Scan(&m.id, &name, &m.fileID, &m.start); err != nil {
				return err
			}
			byName[name] = append(byName[name], m)
			return nil
		}, worktree); err != nil {
		return err
	}

	// Eligible call edges, materialized so the cursor is closed before inserting
	// (one connection cannot read and write concurrently).
	type callEdge struct{ id, dstName, fileID string }
	var calls []callEdge
	if err := scan(ctx, c, `
SELECT ed.id, ed.dst_name, ed.file_id
  FROM edges ed JOIN files f ON f.id = ed.file_id
 WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_name <> ''`,
		func(rows *sql.Rows) error {
			var e callEdge
			if err := rows.Scan(&e.id, &e.dstName, &e.fileID); err != nil {
				return err
			}
			calls = append(calls, e)
			return nil
		}, worktree); err != nil {
		return err
	}

	stmt, err := c.PrepareContext(ctx,
		`INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method) VALUES (?,?,?,'name')`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// ranked is a candidate match with its locality keys precomputed relative to
	// the calling edge, ready for the S5 ordering.
	type ranked struct {
		id                string
		sameFile, sameDir bool
		path              string
		start             int64
	}
	var cand []ranked
	for i := range calls {
		e := &calls[i]
		if _, skip := common[e.dstName]; skip {
			continue
		}
		if _, done := resolved[e.id]; done {
			continue
		}
		ef := files[e.fileID]
		efTop := topDir(ef.path)
		cand = cand[:0]
		for _, m := range byName[e.dstName] {
			mf := files[m.fileID]
			if !sameLangFamily(ef.lang, mf.lang) {
				continue
			}
			cand = append(cand, ranked{
				id:       m.id,
				sameFile: m.fileID == e.fileID,
				sameDir:  topDir(mf.path) == efTop,
				path:     mf.path,
				start:    m.start,
			})
		}
		if len(cand) == 0 {
			continue
		}
		// Confidence keys on the FULL match count (uncapped), mirroring the SQL's
		// COUNT(*) OVER (PARTITION BY edge) — only the emitted rows are capped at 10.
		conf := 0.2
		if len(cand) == 1 {
			conf = 0.45
		}
		sort.Slice(cand, func(a, b int) bool {
			x, y := cand[a], cand[b]
			if x.sameFile != y.sameFile {
				return x.sameFile // same-file (rank 0) sorts first
			}
			if x.sameDir != y.sameDir {
				return x.sameDir // same-top-dir next
			}
			if x.path != y.path {
				return x.path < y.path
			}
			if x.start != y.start {
				return x.start < y.start
			}
			return x.id < y.id // unique id: a total, reproducible order
		})
		n := min(len(cand), 10)
		for _, r := range cand[:n] {
			if _, err := stmt.ExecContext(ctx, e.id, r.id, conf); err != nil {
				return err
			}
		}
	}
	return nil
}

// denormalizeBestCandidate sets edges.dst_node_id to the single best candidate
// per call edge — the Go form of the final UPDATE ... (SELECT ... ORDER BY ...
// LIMIT 1). The SQL ran one correlated subquery, each with its own ORDER BY sort,
// for all ~300k worktree call edges (~10s); a single in-memory pass over the
// candidate set picks each edge's winner by the identical key
// (confidence DESC, same-file, same-top-dir, mf.path, dst start_byte, dst id) and
// writes only the resolved edges.
//
// It relies on the preceding clear having reset every worktree call edge's
// dst_node_id to NULL (Resolve always clears first): edges with no candidate keep
// that NULL, so only the resolved edges need writing — fewer updates than the SQL,
// same result. Candidate dst nodes and their files may live in any worktree (the
// SQL's mf join is unfiltered), so all nodes/files are loaded, not just the
// worktree slice.
func denormalizeBestCandidate(ctx context.Context, c store.Conn, worktree string) error {
	// file id -> path (all worktrees: a candidate's target file is unfiltered).
	filePath := map[string]string{}
	if err := scan(ctx, c, `SELECT id, path FROM files`,
		func(rows *sql.Rows) error {
			var id, p string
			if err := rows.Scan(&id, &p); err != nil {
				return err
			}
			filePath[id] = p
			return nil
		}); err != nil {
		return err
	}

	// node id -> (file id, start_byte), for the target node's locality keys.
	type nodeLoc struct {
		fileID string
		start  int64
	}
	nodes := make(map[string]nodeLoc, 1<<17)
	if err := scan(ctx, c, `SELECT id, file_id, start_byte FROM nodes`,
		func(rows *sql.Rows) error {
			var id string
			var n nodeLoc
			if err := rows.Scan(&id, &n.fileID, &n.start); err != nil {
				return err
			}
			nodes[id] = n
			return nil
		}); err != nil {
		return err
	}

	// Worktree call edges -> their file id and top-level dir (the ef side of the
	// locality comparison). Only these edges are candidates for an update.
	type edgeLoc struct {
		fileID string
		top    string
	}
	edges := map[string]edgeLoc{}
	if err := scan(ctx, c, `
SELECT ed.id, ed.file_id
  FROM edges ed JOIN files f ON f.id = ed.file_id
 WHERE f.worktree = ? AND ed.kind = 'calls'`,
		func(rows *sql.Rows) error {
			var id, fileID string
			if err := rows.Scan(&id, &fileID); err != nil {
				return err
			}
			edges[id] = edgeLoc{fileID: fileID, top: topDir(filePath[fileID])}
			return nil
		}, worktree); err != nil {
		return err
	}

	// winners holds the running best candidate per edge; a candidate replaces the
	// incumbent when it ranks strictly higher under the denormalize order.
	winners := make(map[string]denormWinner, len(edges))
	if err := scan(ctx, c, `SELECT edge_id, dst_node_id, confidence FROM edge_candidates`,
		func(rows *sql.Rows) error {
			var edgeID, dstID string
			var conf float64
			if err := rows.Scan(&edgeID, &dstID, &conf); err != nil {
				return err
			}
			el, ok := edges[edgeID]
			if !ok {
				return nil // candidate for a call edge outside this worktree
			}
			n, ok := nodes[dstID]
			if !ok {
				return nil
			}
			mfPath := filePath[n.fileID]
			cand := denormWinner{
				dstID:    dstID,
				conf:     conf,
				sameFile: n.fileID == el.fileID,
				sameDir:  topDir(mfPath) == el.top,
				path:     mfPath,
				start:    n.start,
			}
			cur, seen := winners[edgeID]
			if !seen || betterCandidate(cand, cur) {
				winners[edgeID] = cand
			}
			return nil
		}); err != nil {
		return err
	}

	// Stage the winners in a temp table, then apply them with one bulk UPDATE.
	// ~190k individual UPDATE round-trips proved slower than the original single
	// SQL statement; a temp table + one set-based UPDATE keeps the in-memory pick
	// while letting SQLite do the write in a single pass. Insert in edge-id order
	// so the temp PK fills sequentially instead of churning random-UUID pages.
	if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS denorm_best`); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `CREATE TEMP TABLE denorm_best (
  edge_id     TEXT PRIMARY KEY,
  dst_node_id TEXT NOT NULL)`); err != nil {
		return err
	}
	ids := make([]string, 0, len(winners))
	for edgeID := range winners {
		ids = append(ids, edgeID)
	}
	sort.Strings(ids)
	stmt, err := c.PrepareContext(ctx, `INSERT INTO denorm_best(edge_id, dst_node_id) VALUES (?,?)`)
	if err != nil {
		return err
	}
	for _, edgeID := range ids {
		if _, err := stmt.ExecContext(ctx, edgeID, winners[edgeID].dstID); err != nil {
			stmt.Close()
			return err
		}
	}
	stmt.Close()
	if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (SELECT b.dst_node_id FROM denorm_best b WHERE b.edge_id = edges.id)
 WHERE id IN (SELECT edge_id FROM denorm_best)`); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, `DROP TABLE IF EXISTS denorm_best`)
	return err
}

// denormWinner is a call edge's best candidate so far, with the locality keys
// precomputed relative to that edge for the denormalize ordering.
type denormWinner struct {
	dstID             string
	conf              float64
	sameFile, sameDir bool
	path              string
	start             int64
}

// betterCandidate reports whether a outranks b under the denormalize order:
// higher confidence, then same-file, then same-top-dir, then path / start_byte /
// id ascending — the stable total order the SQL ORDER BY ... LIMIT 1 applied so a
// re-index of unchanged code picks the same winner every time.
func betterCandidate(a, b denormWinner) bool {
	if a.conf != b.conf {
		return a.conf > b.conf
	}
	if a.sameFile != b.sameFile {
		return a.sameFile
	}
	if a.sameDir != b.sameDir {
		return a.sameDir
	}
	if a.path != b.path {
		return a.path < b.path
	}
	if a.start != b.start {
		return a.start < b.start
	}
	return a.dstID < b.dstID
}

// topDir returns the leading path segment up to and including the first '/', or
// "" for a root-level path — matching the SQL substr(path,1,instr(path,'/')) used
// for the "same top-level directory" locality tiebreak (two root-level files both
// yield "" and so compare equal, the intended same-directory semantics).
func topDir(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i+1]
	}
	return ""
}

// sameLangFamily reports whether two file languages resolve within the same
// family: an exact match, or both within the web family {typescript,tsx,javascript}
// — the same scoping the SQL strategies apply so a C# match never binds to a
// React symbol of the same name.
func sameLangFamily(a, b string) bool {
	if a == b {
		return true
	}
	return isWebLang(a) && isWebLang(b)
}

func isWebLang(l string) bool {
	return l == "typescript" || l == "tsx" || l == "javascript"
}

// callStrategies are the canonical INSERT OR IGNORE passes into edge_candidates,
// applied in priority order (highest confidence first); each binds
// (worktree, worktree). The full Resolve runs S2/S4 from a shared recv_types prefix
// instead (stratTypedReceiverShared/stratInterfaceShared) — a whole-worktree
// optimization — but the INCREMENTAL resolver (resolve_incremental.go) scopes each
// of these by edge via scopeStrategy, where the per-edge set is tiny and prefix
// sharing buys nothing, so it keeps driving from (files -> call edges). The two
// forms are result-identical.
var callStrategies = []string{
	stratSelf,
	stratTypedReceiver,
	stratStatic,
	stratInterface,
	stratNameFallback,
	stratImplicitCtor,
}

// stratImplicitCtor (S6, runs last): a constructor invocation of a type with no
// explicit constructor — C# `new GeoPoint(...)` on a positional record or a
// default-constructed class. The creation site emits a calls edge named after
// the type, but every earlier strategy binds only to CALLABLE nodes, so without
// an explicit constructor the edge stayed unresolved and the type reported "no
// callers" even though an LSP counts the `new` among its usages. Bind such
// still-unresolved edges to the same-language type declaration itself (the type
// stands in for its implicit constructor). The NOT EXISTS keeps this strictly a
// fallback: any callable binding from S1–S5 suppresses it, so ordinary method
// calls are untouched. SQLite materializes the SELECT before inserting (the
// target table is read by the subquery), so the pass is deterministic.
const stratImplicitCtor = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.45, 'ctor'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_name <> ''
  CROSS JOIN nodes m ON m.kind IN ('class','record','struct') AND m.name = ed.dst_name
  JOIN files mf ON mf.id = m.file_id
 WHERE ef.worktree = ? AND mf.worktree = ?
   AND ef.language = mf.language
   AND NOT EXISTS (SELECT 1 FROM edge_candidates ec0 WHERE ec0.edge_id = ed.id)`

// stratInterface (S4, C#): the receiver's bound type T is an interface or base
// class; expand to <method> on each implementing/deriving type, found via the
// extends edges (X extends/implements T) and X's contained methods. Confidence
// 0.5 — a set of possible dispatch targets. Go interfaces are structural (no
// extends edge), so this yields nothing for Go (intended: Go stays S5).
const stratInterface = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.5, 'iface'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_recv <> ''
  CROSS JOIN nodes src ON src.id = ed.src_node_id
  CROSS JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  CROSS JOIN nodes iface ON iface.name = tb.type_name AND iface.kind IN ('interface','class')
  CROSS JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  CROSS JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  CROSS JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = ed.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE ef.worktree = ? AND mf.worktree = ?`

// stratTypedReceiverShared (S2) and stratInterfaceShared (S4) are the full-Resolve
// forms of the two receiver strategies. They read the recv_types temp table
// (buildReceiverTypes) — the materialized, deduplicated (edge -> bound type) prefix
// both otherwise recompute — so the dominant scope-containment join runs once for
// the whole worktree rather than once per strategy. Each binds a single (worktree)
// for the target file. Their candidate output is byte-identical to the inline
// stratTypedReceiver / stratInterface above.
const stratTypedReceiverShared = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT rt.edge_id, m.id, 0.9, 'typed'
  FROM recv_types rt
  CROSS JOIN nodes m ON m.name = rt.dst_name
       AND m.parent_name = substr(rt.type_name, length(rtrim(rt.type_name, replace(rt.type_name,'.',''))) + 1)
       AND m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = rt.type_name || '.' || rt.dst_name
             OR m.qualified_name LIKE '%.' || rt.type_name || '.' || rt.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE mf.worktree = ?
   AND ( rt.ef_language = mf.language
         OR ( rt.ef_language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`

const stratInterfaceShared = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT rt.edge_id, m.id, 0.5, 'iface'
  FROM recv_types rt
  CROSS JOIN nodes iface ON iface.name = rt.type_name AND iface.kind IN ('interface','class')
  CROSS JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  CROSS JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  CROSS JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = rt.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE mf.worktree = ?`

// stratStatic (S3): the receiver token is itself a type name T (Foo.Create());
// bind to T.<method>. Confidence 0.9. A receiver that is a variable (no matching
// type-qualified node) simply yields no rows here.
// The join order is fully pinned with CROSS JOIN, driving from the worktree's
// files into their call edges (via idx_edges_file) and only then to same-name
// nodes. Without this the SQLite planner flips, at large repo scale, to building
// an AUTOMATIC transient index over all edges and driving from every node — a
// catastrophic plan (≈250x slower) that the pinned order avoids. Inner joins are
// commutative, so pinning changes only the plan, never the result.
const stratStatic = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'static'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_recv <> ''
  -- m.name = ed.dst_name drives the join via idx_nodes_name (the qualified_name
  -- last segment is always the node name, so this is a result-preserving prefilter
  -- that avoids a full nodes scan for the leading-wildcard LIKE below).
  CROSS JOIN nodes m ON m.name = ed.dst_name
       -- Indexed pre-filter (idx_nodes_name_parent): the LIKE below can only match
       -- when m's enclosing scope equals the receiver's last segment, so requiring
       -- it up front is result-preserving but turns a per-row wildcard scan into an
       -- index seek. The LIKE/equals still confirms the full (possibly multi-segment)
       -- receiver suffix.
       AND m.parent_name = substr(ed.dst_recv, length(rtrim(ed.dst_recv, replace(ed.dst_recv,'.',''))) + 1)
       AND m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = ed.dst_recv || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || ed.dst_recv || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`

// stratSelf (S1): a bare M() or this.M() inside a class binds to a sibling method
// of the same class, linked via contains edges (class -> caller, class -> target).
// Confidence 0.95. (C#; Go self-calls use an explicit receiver and go via S2.)
// The second bound parameter is unused here but kept so every callStrategies
// entry has the same (worktree, worktree) arity; "? IS NOT NULL" is always true.
const stratSelf = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.95, 'self'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND (ed.dst_recv = '' OR ed.dst_recv = 'this')
  CROSS JOIN edges pc ON pc.kind = 'contains' AND pc.dst_node_id = ed.src_node_id
  CROSS JOIN edges cm ON cm.kind = 'contains' AND cm.src_node_id = pc.src_node_id
  CROSS JOIN nodes m ON m.id = cm.dst_node_id
       AND m.kind IN ('method','property','constructor')
       AND m.name = ed.dst_name
 WHERE ef.worktree = ?
   AND ? IS NOT NULL`

// stratTypedReceiver (S2): the receiver identifier resolves, via a type_binding
// in scope at the call (the binding's scope contains the calling method's span —
// edges.src_node_id), to type T; bind to T.<method>. Confidence 0.9.
// Join order fully pinned (see stratStatic) to avoid the planner's large-scale
// plan flip; drives files -> call edges -> scope binding -> same-name nodes.
const stratTypedReceiver = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'typed'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_recv <> ''
  CROSS JOIN nodes src ON src.id = ed.src_node_id
  CROSS JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  -- m.name = ed.dst_name drives the join via idx_nodes_name (see stratStatic).
  CROSS JOIN nodes m ON m.name = ed.dst_name
       -- Indexed pre-filter on the declaring scope; see stratStatic.
       AND m.parent_name = substr(tb.type_name, length(rtrim(tb.type_name, replace(tb.type_name,'.',''))) + 1)
       AND m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = tb.type_name || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || tb.type_name || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`

// stratNameFallback (S5): name-only fallback — any same-name callable in the
// language family, capped per edge (locality-ordered). Confidence reflects
// AMBIGUITY: a UNIQUE same-name match is reasonably reliable (the only thing the
// call could lexically be — e.g. a Go package function) → 0.45 (medium); several
// same-name matches are genuinely ambiguous → 0.2 (low). Only fires for edges
// with no better candidate, so it never dilutes a resolved edge.
const stratNameFallback = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT edge_id, dst_node_id, CASE WHEN cnt = 1 THEN 0.45 ELSE 0.2 END, 'name' FROM (
  SELECT ed.id AS edge_id, m.id AS dst_node_id,
    COUNT(*) OVER (PARTITION BY ed.id) AS cnt,
    ROW_NUMBER() OVER (PARTITION BY ed.id ORDER BY
      CASE WHEN mf.id = ed.file_id THEN 0 ELSE 1 END,
      CASE WHEN substr(mf.path,1,instr(mf.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
           THEN 0 ELSE 1 END,
      -- Stable tiebreak so the capped 10 ambiguous matches are a reproducible set
      -- (random m.id otherwise picked a different 10 each index).
      mf.path, m.start_byte, m.id) AS rn
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
                AND ed.kind = 'calls' AND ed.dst_name <> ''
                -- Skip the name-only fallback for hyper-common names (ToString,
                -- Equals, Id, ...) that match more than 50 callables: it would pick
                -- 10 arbitrary 0.2-confidence targets (pure noise), and the full
                -- (edge x same-name) product for those names is exactly what makes S5
                -- materialize a multi-GB sort and blow up on large codebases.
                AND ed.dst_name NOT IN (
                      SELECT name FROM nodes
                       WHERE kind IN ('method','property','constructor','function')
                       GROUP BY name HAVING COUNT(*) > 50 )
  CROSS JOIN nodes m ON m.kind IN ('method','property','constructor','function')
               AND m.name = ed.dst_name
  JOIN files mf ON mf.id = m.file_id
  WHERE ef.worktree = ? AND mf.worktree = ?
    AND ( ef.language = mf.language
          OR ( ef.language IN ('typescript','tsx','javascript')
               AND mf.language IN ('typescript','tsx','javascript') ) )
    AND NOT EXISTS (SELECT 1 FROM edge_candidates ec WHERE ec.edge_id = ed.id)
) WHERE rn <= 10`

// deleteFile removes a file's graph rows for the given worktree. nodes/edges
// cascade via FK; nodes_fts rows are dropped manually (FTS5 is unlinked).
func (e *Engine) deleteFile(ctx context.Context, worktree, path string) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts WHERE node_id IN (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts_stem WHERE node_id IN (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts_tri WHERE node_id IN (
  SELECT n.id FROM nodes n JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx,
			`DELETE FROM files WHERE worktree = ? AND path = ?`, worktree, path)
		return err
	})
}
