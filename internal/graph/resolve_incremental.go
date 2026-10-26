package graph

import (
	"context"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Incremental resolve. A full Resolve (resolve.go) re-derives every call edge's
// candidate set across the whole worktree — on a large repo that is seconds of
// CPU, paid on the FIRST graph query after ANY single-file edit (the lazy
// reconcile on the read path). ResolveIncremental does the same work but only for
// the edges whose resolution could actually have changed, turning a 60s+ reconcile
// after a one-file edit into a sub-second one.
//
// Correctness — the affected-edge set is provably complete. An edge's resolution
// is a pure function of its own row plus the nodes/edges/type_bindings it joins
// against (resolve.go). For an edge in an UNCHANGED file, only rows living in a
// changed ("dirty") file can have moved, and every resolver join reaches another
// file in exactly one of two ways:
//
//	(a) a candidate target node m, always matched by m.name = edge.dst_name; or
//	(b) interface dispatch (stratInterface), which expands an `extends` edge.
//
// Case (a) is covered by re-resolving every edge whose dst_name matches a node
// name (or qualified_name) that was present in a dirty file BEFORE or AFTER the
// edit — additions land in the post-state names, removals/renames in the
// pre-state names (so a now-stale S5 candidate count is recomputed, not left
// stale). Case (b) cannot be bounded by names alone (a partial class can declare
// `: IFoo` and the method in different files), so the caller falls back to a full
// Resolve whenever a dirty file's `extends` edge set changed (see ExtendsChanged);
// when it has NOT changed, the interface→implementer structure is identical and
// (a) is sufficient. Over-inclusion is always safe (re-resolving an unaffected
// edge reproduces its current binding); only under-inclusion would regress, and
// the construction above admits none.
//
// dirtyPaths is the worktree-relative paths re-indexed or deleted this reconcile;
// oldNames is the node names+qualified_names those paths held BEFORE the edit
// (captured by the caller pre-reindex). The post-edit names are read here.
func (e *Engine) ResolveIncremental(ctx context.Context, worktree string, dirtyPaths, oldNames []string) error {
	if len(dirtyPaths) == 0 {
		return nil
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		// Per-attempt scratch tables (WriteTx may retry on SQLITE_BUSY): DROP first
		// so a retried body starts clean, regardless of connection reuse.
		for _, ddl := range []string{
			`DROP TABLE IF EXISTS _dirty_paths`,
			`CREATE TEMP TABLE _dirty_paths(path TEXT PRIMARY KEY)`,
			`DROP TABLE IF EXISTS _affected_names`,
			`CREATE TEMP TABLE _affected_names(name TEXT PRIMARY KEY)`,
			`DROP TABLE IF EXISTS _resolve_scope`,
			`CREATE TEMP TABLE _resolve_scope(edge_id TEXT PRIMARY KEY)`,
		} {
			if _, err := c.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}

		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _dirty_paths(path) VALUES (?)`, dirtyPaths); err != nil {
			return err
		}
		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _affected_names(name) VALUES (?)`, oldNames); err != nil {
			return err
		}
		// Post-edit names (and qualified names) of nodes now in the dirty files.
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _affected_names(name)
  SELECT n.name FROM nodes n JOIN files f ON f.id = n.file_id
   WHERE f.worktree = ? AND f.path IN (SELECT path FROM _dirty_paths)`, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _affected_names(name)
  SELECT n.qualified_name FROM nodes n JOIN files f ON f.id = n.file_id
   WHERE f.worktree = ? AND f.path IN (SELECT path FROM _dirty_paths)`, worktree); err != nil {
			return err
		}

		// Affected edges: (A) every edge in a dirty file — re-inserted unresolved by
		// the reconcile — and (C) every edge anywhere whose dst_name matches an
		// added-or-removed name.
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND f.path IN (SELECT path FROM _dirty_paths)`, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.dst_name IN (SELECT name FROM _affected_names)`, worktree); err != nil {
			return err
		}

		// Reset only the affected edges, then re-resolve exactly them — the same
		// passes as full Resolve, scoped to _resolve_scope.
		if _, err := c.ExecContext(ctx,
			`DELETE FROM edge_candidates WHERE edge_id IN (SELECT edge_id FROM _resolve_scope)`); err != nil {
			return err
		}
		// Reset the affected edges' bindings before re-deriving them — but NOT the
		// structural `contains` edges (dst_name = ''), which are bound at write time
		// from the file's own nesting and are never recomputed by name (full Resolve
		// leaves them untouched too). Nulling them would orphan every self-call,
		// whose strategy walks class→method via contains.
		if _, err := c.ExecContext(ctx,
			`UPDATE edges SET dst_node_id = NULL
			  WHERE id IN (SELECT edge_id FROM _resolve_scope)
			    AND (kind = 'calls' OR dst_name <> '')`); err != nil {
			return err
		}

		// Non-call edges (scoped): same name-match bind as resolveNonCallEdges, but
		// only over affected edges (which were just nulled, so the IS NULL guard
		// selects exactly them).
		if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (`+nonCallTarget+`)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND kind <> 'calls'
  AND id IN (SELECT edge_id FROM _resolve_scope)`, worktree); err != nil {
			return err
		}

		// Call edges: the same confidence-ranked strategy passes as full Resolve,
		// each rewritten to drive from _resolve_scope instead of scanning every call
		// edge in the worktree (scopeStrategy).
		for _, strat := range incrementalCallStrategies {
			if _, err := c.ExecContext(ctx, strat, worktree, worktree); err != nil {
				return err
			}
		}

		// Denormalize the best candidate onto edges.dst_node_id (scoped).
		_, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (
  SELECT ec.dst_node_id FROM edge_candidates ec
    JOIN nodes n  ON n.id = ec.dst_node_id
    JOIN files mf ON mf.id = n.file_id
    JOIN files ef ON ef.id = edges.file_id
   WHERE ec.edge_id = edges.id
   ORDER BY ec.confidence DESC,
     CASE WHEN mf.id = edges.file_id THEN 0 ELSE 1 END,
     CASE WHEN substr(mf.path,1,instr(mf.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
          THEN 0 ELSE 1 END,
     mf.path, n.start_byte, ec.dst_node_id
   LIMIT 1)
 WHERE kind = 'calls' AND id IN (SELECT edge_id FROM _resolve_scope)`)
		if err != nil {
			return err
		}

		for _, ddl := range []string{
			`DROP TABLE IF EXISTS _resolve_scope`,
			`DROP TABLE IF EXISTS _affected_names`,
			`DROP TABLE IF EXISTS _dirty_paths`,
		} {
			if _, err := c.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		return nil
	})
}

// incrementalCallStrategies are the full callStrategies (resolve.go), each
// mechanically rewritten to drive from the affected-edge scratch table rather than
// from every call edge in the worktree. The rewrite swaps only the driving table;
// all join predicates, the worktree/language filters, and the per-edge logic are
// byte-for-byte the full strategy, so each scoped pass yields exactly the rows the
// full pass would for those edges.
var incrementalCallStrategies = func() []string {
	out := make([]string, len(callStrategies))
	for i, s := range callStrategies {
		out[i] = scopeStrategy(s)
	}
	return out
}()

// fromAnchor is the leading driver shared by every full call strategy: scan a
// worktree's files, then their call edges. incrAnchor replaces it so the pass
// instead drives from _resolve_scope (the affected edges) and joins back to the
// edge's file. The trailing "AND ed.kind = 'calls' AND <recv>" that followed the
// anchor attaches to the files-ef join after the swap — semantically identical for
// inner joins, which is all these are.
const fromAnchor = `  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id`

const incrAnchor = `  FROM _resolve_scope rs
  CROSS JOIN edges ed ON ed.id = rs.edge_id
  CROSS JOIN files ef ON ef.id = ed.file_id`

func scopeStrategy(full string) string {
	scoped := strings.Replace(full, fromAnchor, incrAnchor, 1)
	if scoped == full {
		// The anchor must appear in every call strategy; a miss means resolve.go
		// drifted and the scoped pass would silently scan nothing. Fail loudly at
		// init rather than under-resolve at runtime.
		panic("graph: scopeStrategy anchor not found — incremental resolve out of sync with callStrategies")
	}
	return scoped
}

// maxIncrementalDirty caps how many changed files still take the incremental
// resolve path. Above it, a reconcile is treated as a sweeping change (a branch
// switch, a bulk format) and a single full Resolve is both simpler and no slower
// than re-resolving a near-worktree-sized affected set. The bound also keeps the
// pre-state capture's parametrized IN lists small.
const maxIncrementalDirty = 200

// namesForPaths returns the distinct node names AND qualified names held by the
// given worktree-relative paths in the CURRENT index. Called pre-reindex to record
// what a dirty file contained, so edges that resolved INTO a since-removed symbol
// are still re-resolved (see ResolveIncremental). Paths are bounded by
// maxIncrementalDirty, so a single parametrized IN is fine.
func (e *Engine) namesForPaths(ctx context.Context, worktree string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT n.name FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
UNION
SELECT n.qualified_name FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)`,
		append(append([]any{}, args...), args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// extendsSigForPaths returns a set identifying the `extends`/`implements` edges
// the given paths currently contribute, keyed by "<deriving qualified_name>\x00
// <base name>\x00<base module>" (the module a Python base names; else empty).
// Comparing this set before and after a reconcile detects whether a dirty file
// changed the interface/base structure — the one case the name-scoped
// incremental resolve cannot bound (see ResolveIncremental), forcing a full
// Resolve.
func (e *Engine) extendsSigForPaths(ctx context.Context, worktree string, paths []string) (map[string]struct{}, error) {
	sig := map[string]struct{}{}
	if len(paths) == 0 {
		return sig, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT COALESCE(src.qualified_name, ''), e.dst_name, e.dst_recv
  FROM edges e
  JOIN files f   ON f.id = e.file_id
  LEFT JOIN nodes src ON src.id = e.src_node_id
 WHERE f.worktree = ? AND e.kind = 'extends'
   AND f.path IN (`+placeholders(len(paths))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, d, m string
		if err := rows.Scan(&s, &d, &m); err != nil {
			return nil, err
		}
		sig[s+"\x00"+d+"\x00"+m] = struct{}{}
	}
	return sig, rows.Err()
}

// sameStringSet reports whether two string-keyed sets are equal.
func sameStringSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// placeholders returns "?,?,…" with n marks for a parametrized IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// insertValues runs stmt (a single-placeholder INSERT) once per value. The lists
// here are small (the names/paths of the handful of files touched by an edit), so
// a prepared loop is simpler than batching and not a bottleneck.
func insertValues(ctx context.Context, c store.Conn, stmt string, vals []string) error {
	if len(vals) == 0 {
		return nil
	}
	ps, err := c.PrepareContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer ps.Close()
	for _, v := range vals {
		if _, err := ps.ExecContext(ctx, v); err != nil {
			return err
		}
	}
	return nil
}
