package graph

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
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
// file in exactly one of three ways:
//
//	(a) a candidate target node m, always matched by m.name = edge.dst_name;
//	(b) an `extends` edge: interface dispatch (stratInterface) expands it, and
//	    Python's inherited lookup (stratInherited) climbs it;
//	(c) a Python import's re-export (resolvePythonModules): another file's
//	    import edge of the same dst_name, so every module-naming import of a
//	    name a dirty file imports so, before or after the edit, is re-resolved.
//
// Case (a) is covered by re-resolving every edge whose dst_name matches a name
// (or a qualified name of a node bearing it) whose nodes in a dirty file changed
// across the edit: their identity, ordinal, contains parent or same-file order
// (nameSeqs). A node whose identity survived keeps its id (reuseNodeIDs), so no
// edge or candidate elsewhere was unlinked from it, and every attribute and the
// order a resolver reads it by is unchanged; additions and removals change the
// sequence (so a now-stale S5 candidate count is recomputed, not left stale).
// Case (b): interface dispatch reaches a method only through the contains edges
// of the very node that carries the extends edge, one level deep — so when the
// extends edges of a dirty declaration changed (it gained or lost a base, or a
// base moved onto or off it, even between two declarations of one class), every
// edge named after that declaration's methods is re-resolved too
// (structureChanged's derived).
// Python's inherited lookup climbs the hierarchy transitively, which names
// cannot bound, so a changed Python extends edge falls back to a full Resolve
// (see extendsSigForPaths). When it has NOT changed, only a base's target can move,
// as a dirty file adds or drops a class of its name: a second class Base fails
// Python's unique-class fallback, a nearer one takes a plain base over. Such an
// edge is in scope by (a), and the calls that may resolve through it re-resolve
// with it (moveBases). Nor can which Python modules exist be bounded by names,
// which a base elsewhere resolves by (resolvePythonModules' unique-class
// fallback), or an import as pkg.X (its submodule rule): creating or deleting a
// Python file whose module something names so resolves fully too (see
// structureChanged). Over-inclusion is always safe (re-resolving an unaffected
// edge reproduces its current binding); only under-inclusion would regress, and
// the construction above admits none.
//
// dirtyPaths is the worktree-relative paths re-indexed or deleted this reconcile;
// old is what they held BEFORE the edit (captured by the caller pre-reindex, see
// captureDirty). The post-edit state is read here.
func (e *Engine) ResolveIncremental(ctx context.Context, worktree string, dirtyPaths []string, old dirtyState, derived []string) error {
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
			`DROP TABLE IF EXISTS _affected_imports`,
			`CREATE TEMP TABLE _affected_imports(name TEXT PRIMARY KEY)`,
			`DROP TABLE IF EXISTS _resolve_scope`,
			`CREATE TEMP TABLE _resolve_scope(edge_id TEXT PRIMARY KEY)`,
			`DROP TABLE IF EXISTS _cand`,
			`CREATE TEMP TABLE _cand(edge_id TEXT NOT NULL, dst_node_id TEXT NOT NULL, confidence REAL NOT NULL,
			   method TEXT NOT NULL, PRIMARY KEY (edge_id, dst_node_id)) WITHOUT ROWID`,
		} {
			if _, err := c.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}

		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _dirty_paths(path) VALUES (?)`, dirtyPaths); err != nil {
			return err
		}
		// Names whose nodes changed identity, parent or same-name order (see
		// nameSeqs), with their qualified names; every other name kept its node
		// ids, so its edges elsewhere still resolve exactly as before.
		newSeqs, err := nameSeqs(ctx, c, worktree, dirtyPaths)
		if err != nil {
			return err
		}
		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _affected_names(name) VALUES (?)`, changedNames(old.seqs, newSeqs)); err != nil {
			return err
		}
		// A dirty class that gained or lost an extends edge (structureChanged)
		// changes which calls its methods answer by interface dispatch. Methods it
		// dropped changed their sequence above; these are the ones it still has.
		if len(derived) > 0 {
			derivedJSON, err := json.Marshal(derived)
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _affected_names(name)
  SELECT m.name FROM files f
    JOIN nodes x  ON x.file_id = f.id
    JOIN edges cm ON cm.src_node_id = x.id AND cm.kind = 'contains'
    JOIN nodes m  ON m.id = cm.dst_node_id AND m.kind = 'method'
   WHERE f.worktree = ? AND f.path IN (SELECT path FROM _dirty_paths)
     AND x.qualified_name IN (SELECT value FROM json_each(?))`, worktree, string(derivedJSON)); err != nil {
				return err
			}
		}
		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _affected_imports(name) VALUES (?)`, old.imports); err != nil {
			return err
		}
		// ... and the names they import by module, which an import elsewhere may
		// take as a re-export (resolvePythonModules)
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _affected_imports(name)
  SELECT e.dst_name FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND f.path IN (SELECT path FROM _dirty_paths)
     AND e.kind = 'imports' AND e.dst_recv <> ''`, worktree); err != nil {
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
		// Driven from the name tables (idx_edges_dstname), not the worktree's files.
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM _affected_names a
  CROSS JOIN edges e ON e.dst_name = a.name
  JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ?`, worktree); err != nil {
			return err
		}
		// ... and (c) every module-naming import of a name a dirty file imports so:
		// only imports follow a re-export, so no call or base re-resolves for it
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM _affected_imports a
  CROSS JOIN edges e ON e.dst_name = a.name
  JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.kind = 'imports' AND e.dst_recv <> ''`, worktree); err != nil {
			return err
		}

		if err := noteBases(ctx, c, old.bases); err != nil {
			return err
		}

		// Re-resolve exactly the affected edges — the same passes as full Resolve,
		// scoped to _resolve_scope. Their candidates are derived afresh into _cand
		// and only the difference applied (after the call passes): most survive an
		// edit unchanged, and rewriting them all is most of a scoped resolve's I/O.
		// Reset the affected edges' bindings before re-deriving them — but NOT the
		// structural `contains` edges (dst_name = ''), which are bound at write time
		// from the file's own nesting and are never recomputed by name (full Resolve
		// leaves them untouched too). Nulling them would orphan every self-call,
		// whose strategy walks class→method via contains. Call edges are left as
		// they are: no strategy reads their binding, and the denormalize below
		// rewrites only those whose best candidate moved.
		if _, err := c.ExecContext(ctx,
			`UPDATE edges SET dst_node_id = NULL
			  WHERE id IN (SELECT edge_id FROM _resolve_scope)
			    AND kind <> 'calls' AND dst_name <> ''`); err != nil {
			return err
		}

		// Non-call edges (scoped): same name-match bind as resolveNonCallEdges, but
		// only over affected edges (which were just nulled, so the IS NULL guard
		// selects exactly them). The unary + keeps SQLite off idx_edges_dst, which
		// would walk every unresolved edge in the store.
		if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (`+nonCallTarget+`)
WHERE +dst_node_id IS NULL
  AND dst_name <> ''
  AND dst_recv = ''
  AND kind <> 'calls'
  AND id IN (SELECT edge_id FROM _resolve_scope)`, worktree); err != nil {
			return err
		}
		if err := resolvePythonModules(ctx, c, worktree, `id IN (SELECT edge_id FROM _resolve_scope)`); err != nil {
			return err
		}
		if err := moveBases(ctx, c, worktree); err != nil {
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
		for _, stmt := range []string{`
DELETE FROM edge_candidates
 WHERE edge_id IN (SELECT edge_id FROM _resolve_scope)
   AND NOT EXISTS (SELECT 1 FROM _cand n
                    WHERE n.edge_id = edge_candidates.edge_id AND n.dst_node_id = edge_candidates.dst_node_id
                      AND n.confidence = edge_candidates.confidence AND n.method = edge_candidates.method)`, `
INSERT OR REPLACE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT n.edge_id, n.dst_node_id, n.confidence, n.method FROM _cand n
 WHERE NOT EXISTS (SELECT 1 FROM edge_candidates ec
                    WHERE ec.edge_id = n.edge_id AND ec.dst_node_id = n.dst_node_id
                      AND ec.confidence = n.confidence AND ec.method = n.method)`,
		} {
			if _, err := c.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}

		// Denormalize the best candidate onto edges.dst_node_id (scoped), writing
		// only the edges whose winner changed.
		_, err = c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = (`+scopedBest+`)
 WHERE kind = 'calls' AND id IN (SELECT edge_id FROM _resolve_scope)
   AND dst_node_id IS NOT (`+scopedBest+`)`)
		if err != nil {
			return err
		}

		for _, ddl := range []string{
			`DROP TABLE IF EXISTS _resolve_scope`,
			`DROP TABLE IF EXISTS _cand`,
			`DROP TABLE IF EXISTS _affected_names`,
			`DROP TABLE IF EXISTS _affected_imports`,
			`DROP TABLE IF EXISTS _dirty_paths`,
			`DROP TABLE IF EXISTS _old_bases`,
			`DROP TABLE IF EXISTS _base_before`,
		} {
			if _, err := c.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		return nil
	})
}

// scopedBest is the denormalize pick for one call edge (correlated on edges):
// highest confidence, then same file, same top-level directory, stable order.
const scopedBest = `
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
   LIMIT 1`

// dirtyState is what the dirty files held before a reconcile rewrote them, which
// ResolveIncremental scopes by: their name sequences (nameSeqs), the names they
// imported by module (importsForPaths), and the extends edges the rewrite
// re-creates or unlinks, with their targets (baseTargets).
type dirtyState struct {
	seqs    map[string][]seqElem
	imports []string
	bases   map[baseKey]baseTarget
}

// baseKey identifies an extends edge across a reindex: its file, its class's
// qualified name, and the base it names (name and module).
type baseKey struct{ path, class, name, module string }

// baseTarget identifies an extends edge's target across a reindex, by its file
// and qualified name; the zero value is no target.
type baseTarget struct{ path, name string }

// captureDirty reads paths' dirtyState from the current index; call it before
// reindexing them.
func (e *Engine) captureDirty(ctx context.Context, worktree string, paths []string) (dirtyState, error) {
	var d dirtyState
	var err error
	if d.seqs, err = nameSeqs(ctx, e.st.DB, worktree, paths); err != nil {
		return d, err
	}
	if d.imports, err = e.importsForPaths(ctx, worktree, paths); err != nil {
		return d, err
	}
	d.bases, err = e.baseTargets(ctx, worktree, paths)
	return d, err
}

// baseTargets returns the target of every extends edge in the given paths, which
// their reindex re-creates unresolved, or pointing into them, which it unlinks
// by deleting the target.
func (e *Engine) baseTargets(ctx context.Context, worktree string, paths []string) (map[baseKey]baseTarget, error) {
	out := map[baseKey]baseTarget{}
	if len(paths) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT sf.path, COALESCE(src.qualified_name, ''), e.dst_name, e.dst_recv,
       COALESCE(tf.path, ''), COALESCE(t.qualified_name, '')
  FROM files sf
  CROSS JOIN edges e ON e.file_id = sf.id AND e.kind = 'extends'
  LEFT JOIN nodes src ON src.id = e.src_node_id
  LEFT JOIN nodes t ON t.id = e.dst_node_id
  LEFT JOIN files tf ON tf.id = t.file_id
 WHERE sf.worktree = ? AND sf.path IN (`+placeholders(len(paths))+`)
UNION
SELECT sf.path, COALESCE(src.qualified_name, ''), e.dst_name, e.dst_recv, tf.path, t.qualified_name
  FROM files tf
  CROSS JOIN nodes t ON t.file_id = tf.id
  CROSS JOIN edges e ON e.dst_node_id = t.id AND e.kind = 'extends'
  JOIN files sf ON sf.id = e.file_id
  LEFT JOIN nodes src ON src.id = e.src_node_id
 WHERE tf.worktree = ? AND tf.path IN (`+placeholders(len(paths))+`)`, append(args, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k baseKey
		var t baseTarget
		if err := rows.Scan(&k.path, &k.class, &k.name, &k.module, &t.path, &t.name); err != nil {
			return nil, err
		}
		out[k] = t
	}
	return out, rows.Err()
}

// noteBases records in _base_before the target each extends edge in
// _resolve_scope had before this resolve: old's, for one the reindex re-created
// or unlinked, else its own. An edge a dirty file did not have before is no
// move: a new non-Python extends edge is structureChanged's derived scope, a
// Python one a full Resolve.
func noteBases(ctx context.Context, c store.Conn, old map[baseKey]baseTarget) error {
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS _old_bases`,
		`CREATE TEMP TABLE _old_bases(path TEXT, class TEXT, name TEXT, module TEXT, tpath TEXT, tname TEXT,
		   PRIMARY KEY (path, class, name, module)) WITHOUT ROWID`,
		`DROP TABLE IF EXISTS _base_before`,
		`CREATE TEMP TABLE _base_before(edge_id TEXT PRIMARY KEY, tpath TEXT, tname TEXT)`,
	} {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	if len(old) > 0 {
		stmt, err := c.PrepareContext(ctx, `INSERT OR REPLACE INTO _old_bases VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for k, t := range old {
			if _, err := stmt.ExecContext(ctx, k.path, k.class, k.name, k.module, t.path, t.name); err != nil {
				return err
			}
		}
	}
	_, err := c.ExecContext(ctx, `
INSERT INTO _base_before(edge_id, tpath, tname)
SELECT e.id, COALESCE(ob.tpath, tf.path, ''), COALESCE(ob.tname, t.qualified_name, '')
  FROM _resolve_scope rs
  CROSS JOIN edges e ON e.id = rs.edge_id AND e.kind = 'extends'
  JOIN files sf ON sf.id = e.file_id
  LEFT JOIN nodes src ON src.id = e.src_node_id
  LEFT JOIN nodes t ON t.id = e.dst_node_id
  LEFT JOIN files tf ON tf.id = t.file_id
  LEFT JOIN _old_bases ob ON ob.path = sf.path AND ob.class = COALESCE(src.qualified_name, '')
       AND ob.name = e.dst_name AND ob.module = e.dst_recv
 WHERE ob.path IS NOT NULL OR sf.path NOT IN (SELECT path FROM _dirty_paths)`)
	return err
}

// moveBases re-resolves the calls an extends edge whose target moved may resolve
// through: one on a receiver typed as its class or a subclass, or written in one
// with super(), which climbs the base (stratInherited), and one on a receiver
// typed as the base's name, which may dispatch to the class's override
// (stratInterface). A receiver's type is any binding of its name in the call's
// file, wherever in scope. Their call edges join _resolve_scope before the call
// passes run, which set every scoped call's target afresh.
func moveBases(ctx context.Context, c store.Conn, worktree string) error {
	for _, ddl := range []string{`DROP TABLE IF EXISTS _moved`, `DROP TABLE IF EXISTS _moved_calls`} {
		if _, err := c.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, `
CREATE TEMP TABLE _moved AS
SELECT e.src_node_id AS class_id, e.dst_name AS base
  FROM _base_before b
  CROSS JOIN edges e ON e.id = b.edge_id
  LEFT JOIN nodes t ON t.id = e.dst_node_id
  LEFT JOIN files tf ON tf.id = t.file_id
 WHERE e.src_node_id IS NOT NULL
   AND (b.tpath <> COALESCE(tf.path, '') OR b.tname <> COALESCE(t.qualified_name, ''))`); err != nil {
		return err
	}
	var moved bool
	if err := c.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM _moved)`).Scan(&moved); err != nil || !moved {
		if err == nil {
			_, err = c.ExecContext(ctx, `DROP TABLE _moved`)
		}
		return err
	}
	if _, err := c.ExecContext(ctx, `
CREATE TEMP TABLE _moved_calls AS
WITH RECURSIVE down(id) AS (
  SELECT class_id FROM _moved
  UNION
  SELECT ext.src_node_id FROM down CROSS JOIN edges ext ON ext.dst_node_id = down.id AND ext.kind = 'extends'
   WHERE ext.src_node_id IS NOT NULL
), types(name) AS (
  SELECT DISTINCT n.name FROM down CROSS JOIN nodes n ON n.id = down.id
), overrides(base, name) AS (
  SELECT DISTINCT mv.base, m.name
    FROM _moved mv CROSS JOIN edges cm ON cm.src_node_id = mv.class_id AND cm.kind = 'contains'
    JOIN nodes m ON m.id = cm.dst_node_id AND m.kind = 'method'
)
SELECT ed.id AS edge_id
  FROM types CROSS JOIN type_bindings tb ON tb.type_name = types.name
  JOIN files f ON f.id = tb.file_id AND f.worktree = ?
  CROSS JOIN edges ed ON ed.file_id = tb.file_id AND ed.dst_recv = tb.name AND ed.kind = 'calls'
UNION
SELECT ed.id FROM files f CROSS JOIN edges ed ON ed.file_id = f.id AND ed.kind = 'calls'
 WHERE f.worktree = ? AND ed.dst_recv IN (SELECT name FROM types)
UNION
SELECT ed.id
  FROM down CROSS JOIN nodes k ON k.id = down.id
  CROSS JOIN edges ed ON ed.file_id = k.file_id AND ed.kind = 'calls' AND ed.dst_recv = 'super()'
  JOIN nodes src ON src.id = ed.src_node_id
 WHERE k.start_byte <= src.start_byte AND src.end_byte <= k.end_byte
UNION
SELECT ed.id
  FROM overrides o CROSS JOIN type_bindings tb ON tb.type_name = o.base
  JOIN files f ON f.id = tb.file_id AND f.worktree = ?
  CROSS JOIN edges ed ON ed.file_id = tb.file_id AND ed.dst_recv = tb.name AND ed.kind = 'calls' AND ed.dst_name = o.name`,
		worktree, worktree, worktree); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM _moved_calls WHERE edge_id IN (SELECT edge_id FROM _resolve_scope)`,
		`INSERT OR IGNORE INTO _resolve_scope(edge_id) SELECT edge_id FROM _moved_calls`,
		`DROP TABLE _moved`, `DROP TABLE _moved_calls`,
	} {
		if _, err := c.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// incrementalCallStrategies are the full callStrategies (resolve.go), each
// mechanically rewritten to drive from the affected-edge scratch table rather than
// from every call edge in the worktree. The rewrite swaps the driving table, sends
// the pass's output to _cand (diffed into edge_candidates afterwards) and tests
// hyper-common names per edge rather than against a whole-store aggregate; all join
// predicates, the worktree/language filters, and the per-edge logic are otherwise
// the full strategy, so each scoped pass yields exactly the rows the full pass
// would for those edges.
var incrementalCallStrategies = func() []string {
	out := make([]string, len(callStrategies))
	for i, s := range callStrategies {
		out[i] = scopeStrategy(s)
	}
	if !strings.Contains(strings.Join(out, ""), hyperCommonPerEdge) {
		panic("graph: hyper-common-name filter not found — incremental resolve out of sync with stratNameFallback")
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
	// The hyper-common-name set aggregates every node in the store; for a handful
	// of scoped edges a per-edge indexed count is the same test, minus the scan.
	// Candidates go to _cand, which ResolveIncremental diffs into edge_candidates.
	scoped = strings.ReplaceAll(scoped, "edge_candidates", "_cand")
	return strings.Replace(scoped, hyperCommonAll, hyperCommonPerEdge, 1)
}

const hyperCommonAll = `AND ed.dst_name NOT IN (
                      SELECT name FROM nodes
                       WHERE kind IN ('method','property','constructor','function')
                       GROUP BY name HAVING COUNT(*) > 50 )`

const hyperCommonPerEdge = `AND (SELECT COUNT(*) FROM nodes
                       WHERE name = ed.dst_name
                         AND kind IN ('method','property','constructor','function')) <= 50`

// maxIncrementalDirty caps how many changed files still take the incremental
// resolve path. Above it, a reconcile is treated as a sweeping change (a branch
// switch, a bulk format) and a single full Resolve is both simpler and no slower
// than re-resolving a near-worktree-sized affected set. The bound also keeps the
// pre-state capture's parametrized IN lists small.
const maxIncrementalDirty = 200

// importsForPaths returns the names the given worktree-relative paths import by
// module in the CURRENT index (which an import elsewhere may resolve through, as
// a re-export). Called pre-reindex to record what a dirty file imported (see
// ResolveIncremental). Paths are bounded by maxIncrementalDirty, so a single
// parametrized IN is fine.
func (e *Engine) importsForPaths(ctx context.Context, worktree string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT DISTINCT e.dst_name FROM edges e JOIN files f ON f.id = e.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
   AND e.kind = 'imports' AND e.dst_recv <> '' AND e.dst_name <> ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var imports []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		imports = append(imports, s)
	}
	return imports, rows.Err()
}

// seqElem is one node in a nameSeqs sequence: its identity, ordinal and contains
// parent (compared), and its qualified name (scoped by when it changed).
type seqElem struct{ ident, qn string }

// nameSeqs returns, per "path\x00name", the nodes bearing that name in paths, in
// nodeOrderSQL order. writeFileGraph keeps a node's id exactly when its identity
// and ordinal survive (reuseNodeIDs), and a resolver reads a target node only by
// its name, identity, contains parent and order among same-named nodes of its
// file — so where a name's sequence is unchanged across a reindex, every edge
// elsewhere that may target it resolves exactly as before.
func nameSeqs(ctx context.Context, q store.Conn, worktree string, paths []string) (map[string][]seqElem, error) {
	out := map[string][]seqElem{}
	if len(paths) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := q.QueryContext(ctx, `
SELECT f.path, n.id, n.kind, n.qualified_name, n.name,
       COALESCE((SELECT cm.src_node_id FROM edges cm
                  WHERE cm.dst_node_id = n.id AND cm.kind = 'contains' LIMIT 1), '')
  FROM files f JOIN nodes n ON n.file_id = f.id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
 ORDER BY f.path, `+nodeOrderSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct{ path, name, parent string }
	var all []row
	var elems []seqElem
	idents := map[string]string{} // node id -> identity + ordinal
	seen := map[string]int{}      // path + identity -> nodes numbered so far
	for rows.Next() {
		var r row
		var id, kind, qn string
		if err := rows.Scan(&r.path, &id, &kind, &qn, &r.name, &r.parent); err != nil {
			return nil, err
		}
		ident := nodeIdentity(kind, qn, r.name)
		k := r.path + "\x00" + ident
		idents[id] = ident + "\x00" + strconv.Itoa(seen[k])
		seen[k]++
		all = append(all, r)
		elems = append(elems, seqElem{ident: idents[id], qn: qn})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, r := range all {
		el := elems[i]
		el.ident += "\x01" + idents[r.parent]
		key := r.path + "\x00" + r.name
		out[key] = append(out[key], el)
	}
	return out, nil
}

// changedNames returns the names whose nameSeqs sequence differs between before
// and after, plus every qualified name those sequences hold.
func changedNames(before, after map[string][]seqElem) []string {
	set := map[string]struct{}{}
	for _, d := range [2][2]map[string][]seqElem{{before, after}, {after, before}} {
		for k, a := range d[0] {
			b := d[1][k]
			if slices.EqualFunc(a, b, func(x, y seqElem) bool { return x.ident == y.ident }) {
				continue
			}
			_, name, _ := strings.Cut(k, "\x00")
			set[name] = struct{}{}
			for _, el := range slices.Concat(a, b) {
				set[el.qn] = struct{}{}
			}
		}
	}
	delete(set, "")
	return slices.Collect(maps.Keys(set))
}

// extendsSigForPaths returns a set identifying the `extends`/`implements` edges
// the given paths currently contribute, keyed by "<deriving qualified_name>\x00<node
// id>\x00<base name>\x00<base module>" (the module a Python base names; else
// empty), and the Python files among them, keyed "\x00\x00<path>".
// structureChanged compares the set before and after a reconcile to tell whether
// a dirty file changed the interface/base structure or the Python modules others
// name — the cases the name-scoped incremental resolve cannot bound (see
// ResolveIncremental). The key names the DECLARATION that carries the edge,
// because that is what interface dispatch reads: a base moved between two
// declarations of one class or between two Rust impls leaves the class and base
// names equal while dispatch changes. The node id survives a reindex exactly
// when the declaration's identity and ordinal do (reuseNodeIDs), so an edit that
// merely moves code keeps the key and re-scopes nothing.
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
SELECT COALESCE(src.qualified_name, ''), COALESCE(src.id, ''), e.dst_name, e.dst_recv, f.language = 'python'
  FROM edges e
  JOIN files f   ON f.id = e.file_id
  LEFT JOIN nodes src ON src.id = e.src_node_id
 WHERE f.worktree = ? AND e.kind = 'extends'
   AND f.path IN (`+placeholders(len(paths))+`)
UNION ALL
SELECT '', '', '', path, 1 FROM files
 WHERE worktree = ? AND language = 'python' AND path IN (`+placeholders(len(paths))+`)`,
		append(args, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, id, d, m string
		var py bool
		if err := rows.Scan(&s, &id, &d, &m, &py); err != nil {
			return nil, err
		}
		if s != "" {
			s += "\x00" + id
		}
		if !py && s != "" {
			// Only interface dispatch (stratInterface) reads a non-Python extends
			// edge, one level deep, from the deriving node's own methods: keyed
			// so structureChanged can scope it instead of resolving fully.
			sig[derivedKey+s+"\x00"+d] = struct{}{}
			continue
		}
		sig[s+"\x00"+d+"\x00"+m] = struct{}{}
	}
	return sig, rows.Err()
}

const derivedKey = "\x01"

// structureChanged reports whether a reconcile must resolve fully, given the
// dirty files' extendsSigForPaths before and after: an `extends` edge of theirs
// changed, or a Python file appeared or went whose module a base elsewhere
// names (the unique-class fallback of resolvePythonModules) or anything names
// as pkg.X (its submodule rule) — which modules exist is no name the
// incremental resolve could scope by. A module nothing names yet, as a new one,
// stays incremental. A changed non-Python extends edge stays incremental too:
// derived returns its deriving classes' qualified names, whose methods the
// incremental resolve then re-scopes (see ResolveIncremental).
func (e *Engine) structureChanged(ctx context.Context, worktree string, before, after map[string]struct{}) (full bool, derived []string, err error) {
	var mods []string
	for _, d := range [][2]map[string]struct{}{{before, after}, {after, before}} {
		for k := range d[0] {
			if _, ok := d[1][k]; ok {
				continue
			}
			if qn, ok := strings.CutPrefix(k, derivedKey); ok {
				qn, _, _ = strings.Cut(qn, "\x00")
				derived = append(derived, qn)
				continue
			}
			path, ok := strings.CutPrefix(k, "\x00\x00")
			if !ok {
				return true, nil, nil
			}
			mods = append(mods, pyModuleNames(path)...)
		}
	}
	if len(mods) == 0 {
		return false, derived, nil
	}
	args := []any{worktree}
	for range 2 {
		for _, m := range mods {
			args = append(args, m)
		}
	}
	var named bool
	err = e.st.DB.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM edges e JOIN files f ON f.id = e.file_id
 WHERE f.worktree = ? AND e.kind <> 'calls' AND e.dst_recv <> ''
   AND ( (e.kind = 'extends' AND e.dst_recv IN (`+placeholders(len(mods))+`))
         OR e.dst_recv || '.' || e.dst_name IN (`+placeholders(len(mods))+`) ))`, args...).Scan(&named)
	return named, derived, err
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
