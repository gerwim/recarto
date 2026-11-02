package graph

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Resolve links reference edges to a defining node within the same worktree.
// Non-call edges (extends/implements/imports/references) bind dst_node_id by
// matching dst_name to a node name/qualified_name, scoped to the language FAMILY
// ({csharp}, {go}, {rust}, {python}, web {typescript,tsx,javascript}) so a C# `.Select(...)` can
// never bind to a React `Select`, preferring same-file then same-directory then a
// stable id order. Call edges are resolved into a confidence-ranked candidate set
// (edge_candidates) by the strategy passes in callStrategies; edges.dst_node_id
// is then denormalized to the best candidate so single-target readers keep
// working. dst_name/dst_recv are retained so trace fallbacks still work where
// resolution is partial.
func (e *Engine) Resolve(ctx context.Context, worktree string, prog Progress) error {
	// Base sub-phase label. Every resolve sub-phase (below, inside the tx) starts
	// with this prefix, so a matcher keyed on "resolving references" still fires and
	// a cancel-on-resolve observer aborts before any DB work opens.
	prog.Phase("resolving references")
	// WriteTxBulk (128 MiB page cache, foreign keys OFF for the transaction) is safe
	// here by the WriteTxBulk contract: this tx only inserts (edge_id, dst_node_id)
	// candidate rows it just SELECTed from edges/nodes in the same tx, deletes
	// edge_candidates rows (an FK child referenced by nothing) and nulls
	// edges.dst_node_id (referenced by nothing). It deletes no parent row that a
	// CASCADE/SET NULL would otherwise clean up, so disabling FK enforcement for the
	// duration cannot orphan anything. A post-resolve `PRAGMA foreign_key_check`
	// (see resolve_test.go) guards this invariant.
	return e.st.WriteTxBulk(ctx, func(c store.Conn) error {
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

		// Call edges: build the shared locality maps once (file path/language, node
		// file+span, worktree call edges), then run the confidence-ranked strategies,
		// folding each candidate the moment it is inserted into the per-edge winner.
		// This replaces the old post-pass that re-scanned the full ~1M-row candidate
		// table just to pick one winner per edge.
		m, err := buildResolveMaps(ctx, c, worktree)
		if err != nil {
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
		if err := buildReceiverTypes(ctx, c, worktree, m); err != nil {
			return err
		}

		// winners holds the running best candidate per call edge, folded via the
		// identical betterCandidate order the old denormalize re-scan applied.
		winners := make(map[string]denormWinner, len(m.edges))

		// Candidate strategies, highest-confidence first. OR IGNORE keeps the first
		// (best) confidence for a given (edge, target) pair, so the ORDER — S1 self,
		// S2 typed, S2b inherited, S3 static, S4 iface, S5 name, S6 ctor — is
		// significant and must be preserved. Each INSERT ... RETURNING yields only the
		// rows it actually inserted (duplicates ignored by OR IGNORE are omitted),
		// which are exactly the rows that survive in the candidate table, so folding
		// them reproduces the full-rescan winner. S2/S4 read recv_types (one bound
		// arg); S1/S2b/S3 scope by (worktree, worktree).
		if err := foldStrategy(ctx, c, m, winners, stratSelf+returningCandidates, worktree, worktree); err != nil {
			return err
		}
		prog.Phase("resolving references (2/8: typed receivers)")
		if err := foldStrategy(ctx, c, m, winners, stratTypedReceiverShared+returningCandidates, worktree); err != nil {
			return err
		}
		if err := foldStrategy(ctx, c, m, winners, stratInheritedShared+returningCandidates, worktree, worktree); err != nil {
			return err
		}
		prog.Phase("resolving references (3/8: static + interface)")
		if err := foldStrategy(ctx, c, m, winners, stratStatic+returningCandidates, worktree, worktree); err != nil {
			return err
		}
		if err := foldStrategy(ctx, c, m, winners, stratInterfaceShared+returningCandidates, worktree); err != nil {
			return err
		}
		prog.Phase("resolving references (4/8: name fallback)")
		if err := resolveNameFallback(ctx, c, worktree, m.files, winners); err != nil {
			return err
		}
		prog.Phase("resolving references (5/8: constructors)")
		if err := stratImplicitCtorFold(ctx, c, worktree, m, winners); err != nil {
			return err
		}

		// Denormalize: edges.dst_node_id = best candidate per call edge — highest
		// confidence, then locality (same-file, then same-top-level-directory),
		// then a stable id order. The winner was folded above as candidates were
		// produced; here we just write it back. Relies on the clear above having
		// NULLed every call edge, so edges with no candidate keep that NULL.
		prog.Phase("resolving references (6/8: picking best targets)")
		if err := applyWinners(ctx, c, winners); err != nil {
			return err
		}
		// Rebuild the dst_node_id index dropped above, in one bulk pass over the final
		// candidate set rather than incrementally across every insert.
		prog.Phase("resolving references (7/8: rebuilding index)")
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
		prog.Phase("resolving references (8/8: analyzing)")
		if _, err := c.ExecContext(ctx, `PRAGMA analysis_limit=1000`); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `ANALYZE`); err != nil {
			return err
		}
		// Drop the shared prefix temp table so it does not linger on this pooled
		// connection after the transaction commits.
		_, err = c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`)
		return err
	})
}

// resolveNonCallEdges binds dst_node_id for every non-call reference edge by
// matching dst_name to a node name/qualified_name within the language family
// (see nonCallTarget for the preference order).
func resolveNonCallEdges(ctx context.Context, c store.Conn, worktree string) error {
	const inWorktree = `file_id IN (SELECT id FROM files WHERE worktree = ?)`
	if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (`+nonCallTarget+`)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND dst_recv = ''
  AND kind <> 'calls'
  AND `+inWorktree, worktree, worktree); err != nil {
		return err
	}
	// A Python import or base naming its module also hangs on files other than its
	// target's — a re-export, which modules exist — whose reindex does not unset
	// it: re-derive them all, resolved or not.
	return resolvePythonModules(ctx, c, worktree, inWorktree, worktree)
}

// buildPyModules (re)creates the py_mod temp table resolvePythonModules reads:
// the module names a Python file of the worktree answers to — its dotted path
// and each dotted suffix, so an import from under a source root (flask.app, for
// src/flask/app.py) finds it too; a package's is its __init__.py — that named
// holds, or holds a package of. "Is file f module m, or under package m?" is a
// seek on f's few names; "the files of module m" one on the module index.
func buildPyModules(ctx context.Context, c store.Conn, worktree string, named map[string]bool) error {
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS py_mod`,
		`CREATE TEMP TABLE py_mod (file_id TEXT NOT NULL, module TEXT NOT NULL, PRIMARY KEY (file_id, module)) WITHOUT ROWID`,
		`CREATE INDEX py_mod_module ON py_mod(module)`,
	} {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	type pyFile struct{ id, path string }
	var files []pyFile
	if err := scan(ctx, c, `SELECT id, path FROM files WHERE worktree = ? AND language = 'python'`,
		func(rows *sql.Rows) error {
			var f pyFile
			if err := rows.Scan(&f.id, &f.path); err != nil {
				return err
			}
			files = append(files, f)
			return nil
		}, worktree); err != nil {
		return err
	}
	stmt, err := c.PrepareContext(ctx, `INSERT OR IGNORE INTO py_mod(file_id, module) VALUES (?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range files {
		for _, m := range pyModuleNames(f.path) {
			if !pyNamed(named, m) {
				continue
			}
			if _, err := stmt.ExecContext(ctx, f.id, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// pyNamed reports whether named holds module m or a package of it.
func pyNamed(named map[string]bool, m string) bool {
	for i := range len(m) {
		if m[i] == '.' && named[m[:i]] {
			return true
		}
	}
	return named[m]
}

// pyModuleNames lists the module names a Python file answers to: its dotted
// path and each dotted suffix (see buildPyModules).
func pyModuleNames(path string) []string {
	segs := strings.Split(strings.TrimSuffix(strings.TrimSuffix(path, ".py"), "/__init__"), "/")
	names := make([]string, len(segs))
	for i := range segs {
		names[i] = strings.Join(segs[i:], ".")
	}
	return names
}

// resolvePythonModules resolves the Python imports and bases that name their
// module (dst_recv) — `from m import X`, `class C(m.X)` — within that module: a
// module-level X (never a method: `from django import forms` is no
// InlineAdminFormSet.forms) in m.py, else in a file of package m/ (a star
// re-export) unless m.X is a submodule (`from django.contrib import messages` is
// no context_processors.messages), possibly under a source root, never in the
// importing file; otherwise not at all (wsgiref's ServerHandler is not the
// worktree's). Candidates are ranked once per (module, name), not per edge:
// Django has 2.4k `models.Model` bases.
//
// If m is in the worktree without X, it re-exports X (`from .base import X` in
// an __init__, or a module importing it for its users): an import then takes
// the target of m's own import of X, itself resolved by module — twice, to
// follow a re-export of a re-export — while a variable m re-exports
// (django.urls' path) stays unresolved, being no symbol. A base takes the one
// module-level Python class X outside its file, if there is exactly one — a
// wrong class would mislead every call it inherits, at full confidence — and
// does not follow import edges: that would make its inherited calls depend on
// another file's imports, which neither the incremental resolver's names nor
// its full-resolve triggers track (which modules exist, they do: see
// structureChanged). scope selects the edges, which are derived in the py_edges
// temp table and written back where their target changes: a full Resolve
// re-derives thousands, and rewriting the unchanged ones would crowd the page
// cache (it slowed Django's name fallback by 0.9s).
func resolvePythonModules(ctx context.Context, c store.Conn, worktree, scope string, scopeArgs ...any) error {
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS py_edges`, `DROP TABLE IF EXISTS py_pairs`, `DROP TABLE IF EXISTS py_ranked`,
		`CREATE TEMP TABLE py_edges (id TEXT PRIMARY KEY, kind TEXT, dst_recv TEXT, dst_name TEXT, file_id TEXT, bound TEXT, target TEXT)`,
	} {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, `
INSERT INTO py_edges (id, kind, dst_recv, dst_name, file_id, bound)
SELECT id, kind, dst_recv, dst_name, file_id, dst_node_id FROM edges
 WHERE dst_recv <> '' AND kind <> 'calls' AND `+scope, scopeArgs...); err != nil {
		return err
	}
	// The (module, name) pairs to resolve, which decide the modules that matter:
	// those they name, and (as pkg.X) may name. An edit of no Python file has none.
	if _, err := c.ExecContext(ctx, `CREATE TEMP TABLE py_pairs AS SELECT DISTINCT dst_recv, dst_name FROM py_edges`); err != nil {
		return err
	}
	named := map[string]bool{}
	if err := scan(ctx, c, `SELECT dst_recv, dst_name FROM py_pairs`, func(rows *sql.Rows) error {
		var recv, name string
		if err := rows.Scan(&recv, &name); err != nil {
			return err
		}
		named[recv], named[recv+"."+name] = true, true
		return nil
	}); err != nil {
		return err
	}
	if len(named) > 0 {
		if err := derivePythonModules(ctx, c, worktree, named); err != nil {
			return err
		}
		for _, stmt := range []string{
			`DELETE FROM py_edges WHERE target IS bound`,
			`UPDATE edges SET dst_node_id = (SELECT target FROM py_edges pe WHERE pe.id = edges.id)
			  WHERE id IN (SELECT id FROM py_edges)`,
		} {
			if _, err := c.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	for _, t := range []string{"py_edges", "py_pairs", "py_ranked", "py_mod"} {
		if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
			return err
		}
	}
	return nil
}

// derivePythonModules sets py_edges.target by the rules of resolvePythonModules,
// over the modules named holds.
func derivePythonModules(ctx context.Context, c store.Conn, worktree string, named map[string]bool) error {
	if err := buildPyModules(ctx, c, worktree, named); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `
CREATE TEMP TABLE py_ranked AS
SELECT p.dst_recv, p.dst_name, n.id AS node_id, n.file_id, f.path, n.start_byte,
       MIN(pm.module <> p.dst_recv) AS submodule
  FROM py_pairs p
  JOIN nodes n ON n.name = p.dst_name AND n.parent_name = ''
  JOIN py_mod pm ON pm.file_id = n.file_id
       AND (pm.module = p.dst_recv
            OR (pm.module BETWEEN p.dst_recv || '.' AND p.dst_recv || '/'
                AND NOT EXISTS (SELECT 1 FROM py_mod sm WHERE sm.module = p.dst_recv || '.' || p.dst_name)))
  JOIN files f ON f.id = n.file_id
 GROUP BY p.dst_recv, p.dst_name, n.id`); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `CREATE INDEX py_ranked_key ON py_ranked(dst_recv, dst_name)`); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `
UPDATE py_edges SET target = (
  SELECT r.node_id FROM py_ranked r
   WHERE r.dst_recv = py_edges.dst_recv AND r.dst_name = py_edges.dst_name AND r.file_id <> py_edges.file_id
   ORDER BY r.submodule, r.path, r.start_byte, r.node_id
   LIMIT 1)`); err != nil {
		return err
	}
	// Re-exports: m's own import of X, as just derived when it is one of py_edges,
	// else as bound. Only a module of the worktree re-exports: `from typing
	// import Any` follows none.
	const imports = `
WITH pending AS MATERIALIZED (
  SELECT DISTINCT dst_recv, dst_name FROM py_edges
   WHERE target IS NULL AND kind = 'imports'
     AND EXISTS (SELECT 1 FROM py_mod pm WHERE pm.module = py_edges.dst_recv)
), via AS MATERIALIZED (
  SELECT p.dst_recv, p.dst_name, (
    SELECT CASE WHEN pe.id IS NULL THEN re.dst_node_id ELSE pe.target END
      FROM py_mod pm
      CROSS JOIN edges re ON re.file_id = pm.file_id
      LEFT JOIN py_edges pe ON pe.id = re.id
      JOIN files mf ON mf.id = pm.file_id
     WHERE pm.module = p.dst_recv AND re.kind = 'imports' AND re.dst_name = p.dst_name
       AND CASE WHEN pe.id IS NULL THEN re.dst_node_id ELSE pe.target END IS NOT NULL
     ORDER BY mf.path
     LIMIT 1) AS target
    FROM pending p
)
UPDATE py_edges SET target = v.target
  FROM via v
 WHERE v.target IS NOT NULL AND v.dst_recv = py_edges.dst_recv AND v.dst_name = py_edges.dst_name
   AND py_edges.target IS NULL AND py_edges.kind = 'imports'`
	for range 2 {
		if _, err := c.ExecContext(ctx, imports); err != nil {
			return err
		}
	}
	_, err := c.ExecContext(ctx, `
UPDATE py_edges SET target = (
  SELECT MIN(n.id)
    FROM nodes n
    JOIN files f ON f.id = n.file_id
   WHERE f.worktree = ? AND f.language = 'python' AND f.id <> py_edges.file_id
     AND n.name = py_edges.dst_name AND n.kind = 'class' AND n.parent_name = ''
  HAVING COUNT(*) = 1)
WHERE target IS NULL AND kind = 'extends'
  AND EXISTS (SELECT 1 FROM py_mod pm WHERE pm.module = py_edges.dst_recv)`, worktree)
	return err
}

// nonCallTarget picks a non-call edge's target (the worktree is its one bound
// parameter): a node named dst_name in the edge's language family, preferring
// the edge's own file, its top-level directory, then a stable order. An extends
// edge never targets its own class. Python imports and bases that name their
// module (dst_recv, the only non-call edges carrying one) are resolvePythonModules';
// the rest of Python's imports name a module (`import json`), which is no node,
// and its bases only a class (`class L(list)` is not BaseFinder.list).
const nonCallTarget = `
  SELECT n.id
    FROM nodes n
    JOIN files f  ON f.id = n.file_id
    JOIN files ef ON ef.id = edges.file_id
   WHERE f.worktree = ?
     AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
     AND ( ef.language = f.language
           OR ( ef.language IN ('typescript','tsx','javascript')
                AND f.language IN ('typescript','tsx','javascript') ) )
     AND NOT (edges.kind = 'extends' AND n.id IS edges.src_node_id)
     AND NOT (ef.language = 'python' AND (edges.kind = 'imports' OR (edges.kind = 'extends' AND n.kind <> 'class')))
   ORDER BY
     CASE WHEN f.id = edges.file_id THEN 0 ELSE 1 END,
     -- same top-level directory next. instr(path,'/')=0 for a root-level file, and
     -- substr(s,1,0)='' in SQLite, so two root-level files compare equal here —
     -- correct "same directory" semantics.
     CASE WHEN substr(f.path,1,instr(f.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
          THEN 0 ELSE 1 END,
     -- Stable tiebreak (see the call-edge denormalize): reproducible across indexes.
     f.path, n.start_byte, n.id
   LIMIT 1`

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
func buildReceiverTypes(ctx context.Context, c store.Conn, worktree string, m *resolveMaps) error {
	if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`); err != nil {
		return err
	}
	// Explicit TEXT columns matching the affinities the old CREATE ... AS SELECT
	// inferred (edges.id, edges.dst_name, files.language, type_bindings.type_name),
	// so the downstream joins on rt.* behave identically. file_id is the call's
	// file, which S2b's class lookup prefers; local_class is 1 when that is a
	// Python file declaring a class of the type's name, which shadows same-named
	// ones elsewhere (S2, S4) — known per row here, it costs them no node lookup
	// per candidate (Django has dozens of classes named Book).
	if _, err := c.ExecContext(ctx, `CREATE TEMP TABLE recv_types (
  edge_id     TEXT,
  dst_name    TEXT,
  ef_language TEXT,
  type_name   TEXT,
  file_id     TEXT,
  local_class INTEGER)`); err != nil {
		return err
	}

	// Load the remaining worktree slice of the graph into memory. The single write
	// connection cannot hold an open cursor while inserting, so every result set
	// is fully drained and closed before the next query or the insert loop runs.
	// File language (m.files) and node spans (m.nodes) come from the shared maps
	// built once per Resolve, so this no longer re-scans files/nodes.

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

	// The classes each Python file declares, for local_class.
	classes := map[fileName]bool{}
	if err := scan(ctx, c, `
SELECT n.file_id, n.name FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.language = 'python' AND n.kind = 'class'`,
		func(rows *sql.Rows) error {
			var k fileName
			if err := rows.Scan(&k.file, &k.name); err != nil {
				return err
			}
			classes[k] = true
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
		`INSERT INTO recv_types(edge_id, dst_name, ef_language, type_name, file_id, local_class) VALUES (?,?,?,?,?,?)`)
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
		sp, ok := m.nodes[e.src.String]
		if !ok {
			continue
		}
		bs := bindings[fileName{e.fileID, e.dstRecv}]
		if len(bs) == 0 {
			continue
		}
		lang := m.files[e.fileID].lang
		for k := range seen {
			delete(seen, k)
		}
		for _, b := range bs {
			if b.start <= sp.start && sp.end <= b.end {
				if _, dup := seen[b.typeName]; dup {
					continue
				}
				seen[b.typeName] = struct{}{}
				local := lang == "python" && classes[fileName{e.fileID, b.typeName[strings.LastIndex(b.typeName, ".")+1:]}]
				if _, err := stmt.ExecContext(ctx, e.id, e.dstName, lang, b.typeName, e.fileID, local); err != nil {
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
// in the language family (no method for a bare call where bareCallNeverMethod),
// capped at 10 per edge in locality order, with a confidence that reflects
// ambiguity (a unique match is 0.45, several are 0.2).
//
// It is the name-only fallback and by far the largest candidate producer, so the
// SQL form's PARTITION-BY window sort over ~786k rows was the largest resolve step
// (~19-23s). Computing it over in-memory maps — same global hyper-common-name skip,
// same language family, same per-edge locality order and 10-cap — emits a
// byte-identical candidate set far faster; the floor becomes the unavoidable
// ~786k-row insert. Must run after S1-S4 (it reads their candidates to skip edges
// already resolved) and before the denormalize.
func resolveNameFallback(ctx context.Context, c store.Conn, worktree string,
	files map[string]fileInfo, winners map[string]denormWinner) error {
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

	// Worktree file path + language for the locality and language-family tests come
	// from the shared `files` map (all files; only worktree ones are looked up here).

	// Callable nodes in the worktree, indexed by name (the S5 match target set;
	// the SQL requires mf.worktree = worktree, satisfied by the join filter here).
	type matchNode struct {
		id, fileID string
		start      int64
		method     bool
	}
	byName := map[string][]matchNode{}
	if err := scan(ctx, c, `
SELECT n.id, n.name, n.file_id, n.start_byte, n.kind = 'method'
  FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?
   AND n.kind IN ('method','property','constructor','function')`,
		func(rows *sql.Rows) error {
			var name string
			var m matchNode
			if err := rows.Scan(&m.id, &name, &m.fileID, &m.start, &m.method); err != nil {
				return err
			}
			byName[name] = append(byName[name], m)
			return nil
		}, worktree); err != nil {
		return err
	}

	// Eligible call edges, materialized so the cursor is closed before inserting
	// (one connection cannot read and write concurrently). A Python super() call
	// is not one: it binds through its class's bases (S2b) or not at all, never
	// to a same-named method of the class itself or of an unrelated one.
	type callEdge struct {
		id, dstName, fileID string
		bare                bool // no receiver
	}
	var calls []callEdge
	if err := scan(ctx, c, `
SELECT ed.id, ed.dst_name, ed.file_id, ed.dst_recv = ''
  FROM edges ed JOIN files f ON f.id = ed.file_id
 WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_name <> '' AND ed.dst_recv <> 'super()'`,
		func(rows *sql.Rows) error {
			var e callEdge
			if err := rows.Scan(&e.id, &e.dstName, &e.fileID, &e.bare); err != nil {
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
		noMethods := e.bare && bareCallNeverMethod(ef.lang)
		cand = cand[:0]
		for _, m := range byName[e.dstName] {
			mf := files[m.fileID]
			if !sameLangFamily(ef.lang, mf.lang) || (noMethods && m.method) {
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
		// Fold this edge's winner (cand[0]) into the shared map. S5 fires only on
		// edges with no S1–S4 candidate (the `resolved` skip above) and every S5
		// candidate for an edge shares one confidence, so cand[0] — sorted by the
		// exact denormalize tiebreak — is the denormalize winner. Folded here so the
		// old full re-scan of the candidate table is unnecessary.
		w := denormWinner{
			dstID: cand[0].id, conf: conf,
			sameFile: cand[0].sameFile, sameDir: cand[0].sameDir,
			path: cand[0].path, start: cand[0].start,
		}
		if cur, seen := winners[e.id]; !seen || betterCandidate(w, cur) {
			winners[e.id] = w
		}
		n := min(len(cand), 10)
		for _, r := range cand[:n] {
			if _, err := stmt.ExecContext(ctx, e.id, r.id, conf); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileInfo is a file's path and language, as held by resolveMaps.files.
type fileInfo struct{ path, lang string }

// nodeSpanLoc is a node's file plus byte span, as held by resolveMaps.nodes: the
// file id and start_byte serve the denormalize locality keys; start/end serve
// buildReceiverTypes' scope-containment test.
type nodeSpanLoc struct {
	fileID     string
	start, end int64
}

// edgeLoc is a worktree call edge's file id and top-level directory — the "ef"
// side of the denormalize locality comparison — as held by resolveMaps.edges.
type edgeLoc struct {
	fileID string
	top    string
}

// resolveMaps are the locality lookups the whole call-edge resolution shares,
// built once per full Resolve so each stage stops re-scanning files/nodes:
//   - files: file id -> path + language, ALL worktrees (a candidate's target file
//     may live in any worktree, matching the unfiltered mf join of the old SQL).
//   - nodes: node id -> file + byte span, ALL nodes.
//   - edges: worktree call edge id -> file id + top-level dir; only these edges can
//     be denormalized, so this bounds the fold to the worktree.
type resolveMaps struct {
	files map[string]fileInfo
	nodes map[string]nodeSpanLoc
	edges map[string]edgeLoc
}

// buildResolveMaps loads the shared locality maps in three drained scans (the single
// write connection cannot hold a cursor open across other statements).
func buildResolveMaps(ctx context.Context, c store.Conn, worktree string) (*resolveMaps, error) {
	m := &resolveMaps{
		files: map[string]fileInfo{},
		nodes: make(map[string]nodeSpanLoc, 1<<17),
		edges: map[string]edgeLoc{},
	}
	if err := scan(ctx, c, `SELECT id, path, language FROM files`,
		func(rows *sql.Rows) error {
			var id string
			var fi fileInfo
			if err := rows.Scan(&id, &fi.path, &fi.lang); err != nil {
				return err
			}
			m.files[id] = fi
			return nil
		}); err != nil {
		return nil, err
	}
	if err := scan(ctx, c, `SELECT id, file_id, start_byte, end_byte FROM nodes`,
		func(rows *sql.Rows) error {
			var id string
			var n nodeSpanLoc
			if err := rows.Scan(&id, &n.fileID, &n.start, &n.end); err != nil {
				return err
			}
			m.nodes[id] = n
			return nil
		}); err != nil {
		return nil, err
	}
	if err := scan(ctx, c, `
SELECT ed.id, ed.file_id
  FROM edges ed JOIN files f ON f.id = ed.file_id
 WHERE f.worktree = ? AND ed.kind = 'calls'`,
		func(rows *sql.Rows) error {
			var id, fileID string
			if err := rows.Scan(&id, &fileID); err != nil {
				return err
			}
			m.edges[id] = edgeLoc{fileID: fileID, top: topDir(m.files[fileID].path)}
			return nil
		}, worktree); err != nil {
		return nil, err
	}
	return m, nil
}

// returningCandidates is appended to each SQL candidate strategy so its
// INSERT OR IGNORE streams back the rows it actually inserted — exactly the rows
// that survive in the candidate table — for the winner fold.
const returningCandidates = "\nRETURNING edge_id, dst_node_id, confidence"

// foldStrategy runs an `INSERT OR IGNORE ... RETURNING edge_id, dst_node_id,
// confidence` candidate strategy and folds every inserted row into winners via
// foldCandidate. Draining the RETURNING cursor is what makes SQLite perform the
// insert, so scan (which drains fully) both writes the candidates and collects
// the winners in one pass.
func foldStrategy(ctx context.Context, c store.Conn, m *resolveMaps,
	winners map[string]denormWinner, strat string, args ...any) error {
	return scan(ctx, c, strat, func(rows *sql.Rows) error {
		var edgeID, dstID string
		var conf float64
		if err := rows.Scan(&edgeID, &dstID, &conf); err != nil {
			return err
		}
		foldCandidate(m, winners, edgeID, dstID, conf)
		return nil
	}, args...)
}

// foldCandidate updates winners[edgeID] with (dstID, conf) if it ranks strictly
// higher than the incumbent under betterCandidate — the identical order the old
// full-rescan denormalize applied. Candidates whose edge is not a worktree call
// edge, or whose target node is unknown, are dropped (the old rescan skipped them
// the same way).
func foldCandidate(m *resolveMaps, winners map[string]denormWinner, edgeID, dstID string, conf float64) {
	el, ok := m.edges[edgeID]
	if !ok {
		return
	}
	n, ok := m.nodes[dstID]
	if !ok {
		return
	}
	mfPath := m.files[n.fileID].path
	cand := denormWinner{
		dstID:    dstID,
		conf:     conf,
		sameFile: n.fileID == el.fileID,
		sameDir:  topDir(mfPath) == el.top,
		path:     mfPath,
		start:    n.start,
	}
	if cur, seen := winners[edgeID]; !seen || betterCandidate(cand, cur) {
		winners[edgeID] = cand
	}
}

// stratImplicitCtorFold is the full-Resolve form of S6, rewritten to drive from a
// precomputed unresolved-edge temp table rather than a per-(edge × same-name type)
// NOT EXISTS probe (which cost ~56s on graphql-platform). unresolved_calls captures
// exactly the pre-S6 unresolved worktree call edges — the same set the old
// statement's NOT EXISTS tested against the materialized pre-S6 candidate table —
// so the NOT EXISTS now runs once per call edge instead of once per join row.
// Preserved exactly: the strict ef.language = mf.language equality (S6 has no
// TS/JS-family widening, unlike S2/S3/S5), the class/record/struct kind set,
// confidence 0.45, and S6's position last. Its inserted rows are folded like every
// other strategy.
func stratImplicitCtorFold(ctx context.Context, c store.Conn, worktree string,
	m *resolveMaps, winners map[string]denormWinner) error {
	if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS unresolved_calls`); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `
CREATE TEMP TABLE unresolved_calls AS
  SELECT ed.id AS edge_id, ed.dst_name AS dst_name, ed.file_id AS file_id
    FROM files ef JOIN edges ed ON ed.file_id = ef.id
   WHERE ef.worktree = ? AND ed.kind = 'calls' AND ed.dst_name <> ''
     AND `+ctorCallSite+`
     AND NOT EXISTS (SELECT 1 FROM edge_candidates ec0 WHERE ec0.edge_id = ed.id)`, worktree); err != nil {
		return err
	}
	if err := foldStrategy(ctx, c, m, winners, `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT u.edge_id, m.id, 0.45, 'ctor'
  FROM unresolved_calls u
  JOIN files ef ON ef.id = u.file_id
  CROSS JOIN nodes m ON m.kind IN ('class','record','struct') AND m.name = u.dst_name
  JOIN files mf ON mf.id = m.file_id
 WHERE mf.worktree = ? AND ef.language = mf.language`+returningCandidates, worktree); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, `DROP TABLE unresolved_calls`)
	return err
}

// applyWinners writes each call edge's folded winner back to edges.dst_node_id.
// The winners were picked (via betterCandidate) as candidates were produced, so
// this is only the write-back half of the former denormalize: stage the winners in
// a temp table in edge-id order (so its PK fills sequentially instead of churning
// random-UUID pages), then apply them with one set-based UPDATE. Edges with no
// candidate are absent from winners and keep the NULL the clear left.
func applyWinners(ctx context.Context, c store.Conn, winners map[string]denormWinner) error {
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

// bareCallNeverMethod reports whether a call without a receiver can never name a
// method in lang: Rust and Python methods take self.m() / Self::m(). The SQL
// strategies spell the same set as ('rust','python').
func bareCallNeverMethod(lang string) bool { return lang == "rust" || lang == "python" }

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
	stratInherited,
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
   AND ` + ctorCallSite + `
   AND NOT EXISTS (SELECT 1 FROM edge_candidates ec0 WHERE ec0.edge_id = ed.id)`

// ctorCallSite drops C# member calls: C# constructs only via `new` (no receiver),
// so `list.Add(x)` is never `class Add`. Python `mod.Cls()`, Rust `m::Point(..)`
// and Go `pkg.T(x)` do construct through a receiver and keep matching.
const ctorCallSite = `NOT (ef.language = 'csharp' AND ed.dst_recv <> '')`

// stratInterface (S4, C#/Rust/Python): the receiver's bound type T is an interface, base
// class or trait; expand to <method> on each implementing/deriving type (a Rust
// impl block), found via the extends edges (X extends/implements T) and X's
// contained methods, in the call's language family (a C# T is never a Python
// call's). Confidence 0.5 — a set of possible dispatch targets. Go interfaces are structural (no
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
  CROSS JOIN nodes iface ON iface.name = tb.type_name AND iface.kind IN ('interface','class','trait')
  CROSS JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  CROSS JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  CROSS JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = ed.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )
   -- Python: a class the call's own file declares shadows same-named ones elsewhere
   AND NOT ( ef.language = 'python' AND iface.file_id <> ed.file_id
             AND EXISTS (SELECT 1 FROM nodes lc WHERE lc.file_id = ed.file_id AND lc.kind = 'class' AND lc.name = iface.name) )`

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
              AND mf.language IN ('typescript','tsx','javascript') ) )
   -- Python: a class the call's own file declares shadows same-named ones elsewhere
   AND NOT ( rt.local_class AND m.file_id <> rt.file_id )`

const stratInterfaceShared = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT rt.edge_id, m.id, 0.5, 'iface'
  FROM recv_types rt
  CROSS JOIN nodes iface ON iface.name = rt.type_name AND iface.kind IN ('interface','class','trait')
  CROSS JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  CROSS JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  CROSS JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = rt.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE mf.worktree = ?
   AND ( rt.ef_language = mf.language
         OR ( rt.ef_language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )
   -- Python: a class the call's own file declares shadows same-named ones elsewhere
   AND NOT ( rt.local_class AND iface.file_id <> rt.file_id )`

// stratInherited (S2b, Python): the receiver names a class T — its bound type,
// as for S2, or the receiver itself, as for S3 — that does not declare the
// method; bind to the declaration on T's nearest ancestor along the resolved
// extends edges, the one Python's attribute lookup finds (overrides below T are
// S4's). super().m() binds the same way from the class the call is written in,
// skipping that class's own m. Confidence 0.9, as for S2: the type is as
// certain. T is a Python class of the call's worktree, and one declared in the
// call's own file shadows same-named classes elsewhere (self's class always is)
// — the rule S2, S3 and S4 apply to Python calls too. The full Resolve reads
// S2's recv_types; the incremental one binds inline.
const stratInherited = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
WITH RECURSIVE calls AS MATERIALIZED (
  SELECT ed.id AS edge_id, ed.dst_name, ed.dst_recv, ed.file_id, src.start_byte, src.end_byte
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_recv <> ''
  LEFT JOIN nodes src ON src.id = ed.src_node_id
 WHERE ef.worktree = ? AND ef.language = 'python'
), recv AS MATERIALIZED (
  SELECT c.edge_id, c.dst_name, c.file_id, tb.type_name
    FROM calls c
    CROSS JOIN type_bindings tb ON tb.file_id = c.file_id AND tb.name = c.dst_recv
         AND tb.scope_start_byte <= c.start_byte AND c.end_byte <= tb.scope_end_byte
  UNION
  SELECT edge_id, dst_name, file_id, dst_recv FROM calls WHERE dst_recv GLOB '[A-Z]*'` + stratInheritedTail

const stratInheritedShared = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
WITH RECURSIVE calls AS MATERIALIZED (
  SELECT ed.id AS edge_id, ed.dst_name, ed.dst_recv, ed.file_id, src.start_byte, src.end_byte
    FROM files ef JOIN edges ed ON ed.file_id = ef.id
    LEFT JOIN nodes src ON src.id = ed.src_node_id
   WHERE ef.worktree = ? AND ef.language = 'python' AND ed.kind = 'calls'
     AND (ed.dst_recv GLOB '[A-Z]*' OR ed.dst_recv = 'super()')
), recv AS MATERIALIZED (
  SELECT edge_id, dst_name, file_id, type_name FROM recv_types WHERE ef_language = 'python'
  UNION
  SELECT edge_id, dst_name, file_id, dst_recv FROM calls WHERE dst_recv GLOB '[A-Z]*'` + stratInheritedTail

// stratInheritedTail is S2b after calls (with each caller's span) and
// recv(edge_id, dst_name, file_id, type_name): the classes each type names (the
// call file's own, else all), the class each super() call is written in, their
// ancestors, and per class and method name the nearest declaration at or above
// the floor (0, or 1 for super(), which skips the class itself). Each step runs
// once per distinct class or name, not per call: a test class's thousand
// self.assert*() calls climb its hierarchy once.
const stratInheritedTail = `
), supers AS MATERIALIZED (
  SELECT edge_id, dst_name, root_id FROM (
    SELECT c.edge_id, c.dst_name, k.id AS root_id,
           ROW_NUMBER() OVER (PARTITION BY c.edge_id ORDER BY k.end_byte - k.start_byte) AS innermost
      FROM calls c
      CROSS JOIN nodes k ON k.file_id = c.file_id AND k.kind = 'class'
           AND k.start_byte <= c.start_byte AND c.end_byte <= k.end_byte
     WHERE c.dst_recv = 'super()')
   WHERE innermost = 1
), pairs AS MATERIALIZED (
  SELECT DISTINCT type_name, file_id FROM recv
), local AS MATERIALIZED (
  SELECT p.type_name, p.file_id, t.id AS root_id
    FROM pairs p
    CROSS JOIN nodes t ON t.file_id = p.file_id AND t.name = p.type_name AND t.kind = 'class'
), roots AS MATERIALIZED (
  SELECT type_name, file_id, root_id FROM local
  UNION ALL
  SELECT p.type_name, p.file_id, t.id
    FROM pairs p
    JOIN files pf ON pf.id = p.file_id
    CROSS JOIN nodes t ON t.name = p.type_name AND t.kind = 'class'
    JOIN files tf ON tf.id = t.file_id AND tf.language = 'python' AND tf.worktree = pf.worktree
   WHERE NOT EXISTS (SELECT 1 FROM local l WHERE l.type_name = p.type_name AND l.file_id = p.file_id)
), anc(root_id, node_id, depth) AS (
  SELECT root_id, root_id, 0 FROM (SELECT root_id FROM roots UNION SELECT root_id FROM supers)
  UNION
  SELECT anc.root_id, ext.dst_node_id, anc.depth + 1
    FROM anc
    CROSS JOIN edges ext ON ext.src_node_id = anc.node_id AND ext.kind = 'extends'
   WHERE ext.dst_node_id IS NOT NULL AND anc.depth < 8
), asks AS MATERIALIZED (
  SELECT DISTINCT ro.root_id, r.dst_name, 0 AS floor
    FROM recv r
    CROSS JOIN roots ro ON ro.type_name = r.type_name AND ro.file_id = r.file_id
  UNION
  SELECT DISTINCT root_id, dst_name, 1 FROM supers
), found AS MATERIALIZED (
  SELECT a.root_id, a.dst_name, a.floor, m.id AS dst_node_id, anc.depth,
         MIN(anc.depth) OVER (PARTITION BY a.root_id, a.dst_name, a.floor) AS nearest
    FROM asks a
    CROSS JOIN anc ON anc.root_id = a.root_id AND anc.depth >= a.floor
    CROSS JOIN nodes cls ON cls.id = anc.node_id
    CROSS JOIN nodes m ON m.name = a.dst_name AND m.parent_name = cls.name AND m.kind = 'method'
    CROSS JOIN edges cm ON cm.dst_node_id = m.id AND cm.kind = 'contains' AND cm.src_node_id = anc.node_id
    JOIN files mf ON mf.id = m.file_id
   WHERE mf.worktree = ?
)
SELECT r.edge_id, f.dst_node_id, 0.9, 'typed'
  FROM recv r
  CROSS JOIN roots ro ON ro.type_name = r.type_name AND ro.file_id = r.file_id
  CROSS JOIN found f ON f.root_id = ro.root_id AND f.dst_name = r.dst_name AND f.floor = 0
 WHERE f.depth = f.nearest AND f.depth > 0
UNION ALL
SELECT s.edge_id, f.dst_node_id, 0.9, 'typed'
  FROM supers s
  CROSS JOIN found f ON f.root_id = s.root_id AND f.dst_name = s.dst_name AND f.floor = 1
 WHERE f.depth = f.nearest`

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
              AND mf.language IN ('typescript','tsx','javascript') ) )
   -- Python: a class the call's own file declares shadows same-named ones elsewhere
   AND NOT ( ef.language = 'python' AND m.file_id <> ed.file_id
             AND EXISTS (SELECT 1 FROM nodes lc WHERE lc.file_id = ed.file_id AND lc.kind = 'class' AND lc.name = m.parent_name) )`

// stratSelf (S1): a bare M() or this.M() inside a class binds to a sibling method
// of the same class, linked via contains edges (class -> caller, class -> target).
// Confidence 0.95. (C#; Go self-calls use an explicit receiver and go via S2.)
// Rust and Python are excluded for bare calls: a bare f() there can never name a
// method (that takes self.f() / Self::f(), resolved by S2 via the self bindings).
// The second bound parameter is unused here but kept so every callStrategies
// entry has the same (worktree, worktree) arity; "? IS NOT NULL" is always true.
const stratSelf = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.95, 'self'
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND (ed.dst_recv = 'this' OR (ed.dst_recv = '' AND ef.language NOT IN ('rust','python')))
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
              AND mf.language IN ('typescript','tsx','javascript') ) )
   -- Python: a class the call's own file declares shadows same-named ones elsewhere
   AND NOT ( ef.language = 'python' AND m.file_id <> ed.file_id
             AND EXISTS (SELECT 1 FROM nodes lc WHERE lc.file_id = ed.file_id AND lc.kind = 'class' AND lc.name = m.parent_name) )`

// stratNameFallback (S5): name-only fallback — any same-name callable in the
// language family (no method for a bare Rust/Python call), capped per edge
// (locality-ordered). Confidence reflects
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
                AND ed.dst_recv <> 'super()' -- see resolveNameFallback
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
               -- see bareCallNeverMethod
               AND NOT (m.kind = 'method' AND ed.dst_recv = '' AND ef.language IN ('rust','python'))
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
		// A vanished file's symbols journal as removed (same identity rules as
		// writeFileGraph; the file's id row still exists at this point).
		{
			now := e.clk.Now().UnixMilli()
			rows, err := c.QueryContext(ctx, `
SELECT n.qualified_name, n.kind FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path = ?`, worktree, path)
			if err != nil {
				return err
			}
			var syms [][2]string
			for rows.Next() {
				var qn, kind string
				if err := rows.Scan(&qn, &kind); err != nil {
					rows.Close()
					return err
				}
				syms = append(syms, [2]string{qn, kind})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, s := range syms {
				if _, err := c.ExecContext(ctx,
					`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
					 VALUES (?,?,?,?,?,?,?)`, ids.New(), worktree, path, s[0], s[1], "removed", now); err != nil {
					return err
				}
			}
		}
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts WHERE rowid IN (
  SELECT r.fts_rowid FROM node_fts_rows r
  JOIN nodes n ON n.id = r.node_id JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts_stem WHERE rowid IN (
  SELECT r.fts_rowid FROM node_fts_rows r
  JOIN nodes n ON n.id = r.node_id JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
DELETE FROM nodes_fts_tri WHERE rowid IN (
  SELECT r.fts_rowid FROM node_fts_rows r
  JOIN nodes n ON n.id = r.node_id JOIN files f ON f.id = n.file_id
  WHERE f.worktree = ? AND f.path = ?)`, worktree, path); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx,
			`DELETE FROM files WHERE worktree = ? AND path = ?`, worktree, path)
		return err
	})
}
