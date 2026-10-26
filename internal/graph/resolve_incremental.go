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
// file in exactly one of three ways:
//
//	(a) a candidate target node m, always matched by m.name = edge.dst_name;
//	(b) an `extends` edge: interface dispatch (stratInterface) expands it, and
//	    Python's inherited lookup (stratInherited) climbs it;
//	(c) a Python import's re-export (resolvePythonModules): another file's
//	    import edge of the same dst_name, so every module-naming import of a
//	    name a dirty file imports so, before or after the edit, is re-resolved.
//
// Case (a) is covered by re-resolving every edge whose dst_name matches a node
// name (or qualified_name) that was present in a dirty file BEFORE or AFTER the
// edit — additions land in the post-state names, removals/renames in the
// pre-state names (so a now-stale S5 candidate count is recomputed, not left
// stale). Case (b) cannot be bounded by names alone (a partial class can declare
// `: IFoo` and the method in different files), so the caller falls back to a full
// Resolve whenever a dirty file's `extends` edge set changed (see
// extendsSigForPaths). When it has NOT changed, only a base's target can move,
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
func (e *Engine) ResolveIncremental(ctx context.Context, worktree string, dirtyPaths []string, old dirtyState) error {
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
		} {
			if _, err := c.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}

		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _dirty_paths(path) VALUES (?)`, dirtyPaths); err != nil {
			return err
		}
		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _affected_names(name) VALUES (?)`, old.names); err != nil {
			return err
		}
		if err := insertValues(ctx, c, `INSERT OR IGNORE INTO _affected_imports(name) VALUES (?)`, old.imports); err != nil {
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
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.dst_name IN (SELECT name FROM _affected_names)`, worktree); err != nil {
			return err
		}
		// ... and (c) every module-naming import of a name a dirty file imports so:
		// only imports follow a re-export, so no call or base re-resolves for it
		if _, err := c.ExecContext(ctx, `
INSERT OR IGNORE INTO _resolve_scope(edge_id)
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.kind = 'imports' AND e.dst_recv <> ''
     AND e.dst_name IN (SELECT name FROM _affected_imports)`, worktree); err != nil {
			return err
		}

		if err := noteBases(ctx, c, old.bases); err != nil {
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

// dirtyState is what the dirty files held before a reconcile rewrote them, which
// ResolveIncremental scopes by: their node names and qualified names, the names
// they imported by module (namesForPaths), and the extends edges the rewrite
// re-creates or unlinks, with their targets (baseTargets).
type dirtyState struct {
	names, imports []string
	bases          map[baseKey]baseTarget
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
	if d.names, d.imports, err = e.namesForPaths(ctx, worktree, paths); err != nil {
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
// or unlinked, else its own.
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
       AND ob.name = e.dst_name AND ob.module = e.dst_recv`)
	return err
}

// moveBases re-resolves the calls an extends edge whose target moved may resolve
// through: one on a receiver typed as its class or a subclass, or written in one
// with super(), which climbs the base (stratInherited), and one on a receiver
// typed as the base's name, which may dispatch to the class's override
// (stratInterface). A receiver's type is any binding of its name in the call's
// file, wherever in scope. Their call edges join _resolve_scope, candidates
// dropped, before the call passes run (which set every scoped call's target
// afresh).
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
		`DELETE FROM edge_candidates WHERE edge_id IN (SELECT edge_id FROM _moved_calls)`,
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
// given worktree-relative paths in the CURRENT index and, separately, the names
// they import by module (which an import elsewhere may resolve through, as a
// re-export). Called pre-reindex to record what a dirty file contained, so
// edges that resolved INTO a since-removed symbol are still re-resolved (see
// ResolveIncremental). Paths are bounded by maxIncrementalDirty, so a single
// parametrized IN is fine.
func (e *Engine) namesForPaths(ctx context.Context, worktree string, paths []string) (names, imports []string, err error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, worktree)
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := e.st.DB.QueryContext(ctx, `
SELECT 0, n.name FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
UNION
SELECT 0, n.qualified_name FROM nodes n JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
UNION
SELECT 1, e.dst_name FROM edges e JOIN files f ON f.id = e.file_id
 WHERE f.worktree = ? AND f.path IN (`+placeholders(len(paths))+`)
   AND e.kind = 'imports' AND e.dst_recv <> ''`,
		append(append(append([]any{}, args...), args...), args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var imported bool
		var s string
		if err := rows.Scan(&imported, &s); err != nil {
			return nil, nil, err
		}
		switch {
		case s == "":
		case imported:
			imports = append(imports, s)
		default:
			names = append(names, s)
		}
	}
	return names, imports, rows.Err()
}

// extendsSigForPaths returns a set identifying the `extends`/`implements` edges
// the given paths currently contribute, keyed by "<deriving qualified_name>\x00
// <base name>\x00<base module>" (the module a Python base names; else empty),
// and the Python files among them, keyed "\x00\x00<path>". structureChanged
// compares the set before and after a reconcile to tell whether a dirty file
// changed the interface/base structure or the Python modules others name — the
// cases the name-scoped incremental resolve cannot bound (see
// ResolveIncremental), forcing a full Resolve.
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
   AND f.path IN (`+placeholders(len(paths))+`)
UNION ALL
SELECT '', '', path FROM files
 WHERE worktree = ? AND language = 'python' AND path IN (`+placeholders(len(paths))+`)`,
		append(args, args...)...)
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

// structureChanged reports whether a reconcile must resolve fully, given the
// dirty files' extendsSigForPaths before and after: an `extends` edge of theirs
// changed, or a Python file appeared or went whose module a base elsewhere
// names (the unique-class fallback of resolvePythonModules) or anything names
// as pkg.X (its submodule rule) — which modules exist is no name the
// incremental resolve could scope by. A module nothing names yet, as a new one,
// stays incremental.
func (e *Engine) structureChanged(ctx context.Context, worktree string, before, after map[string]struct{}) (bool, error) {
	var mods []string
	for _, d := range [][2]map[string]struct{}{{before, after}, {after, before}} {
		for k := range d[0] {
			if _, ok := d[1][k]; ok {
				continue
			}
			path, ok := strings.CutPrefix(k, "\x00\x00")
			if !ok {
				return true, nil
			}
			mods = append(mods, pyModuleNames(path)...)
		}
	}
	if len(mods) == 0 {
		return false, nil
	}
	args := []any{worktree}
	for range 2 {
		for _, m := range mods {
			args = append(args, m)
		}
	}
	var named bool
	err := e.st.DB.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM edges e JOIN files f ON f.id = e.file_id
 WHERE f.worktree = ? AND e.kind <> 'calls' AND e.dst_recv <> ''
   AND ( (e.kind = 'extends' AND e.dst_recv IN (`+placeholders(len(mods))+`))
         OR e.dst_recv || '.' || e.dst_name IN (`+placeholders(len(mods))+`) ))`, args...).Scan(&named)
	return named, err
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
