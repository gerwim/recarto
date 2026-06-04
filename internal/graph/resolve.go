package graph

import (
	"context"

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
		// Idempotent re-resolve: clear this worktree's call candidates and reset
		// the call edges' denormalized target before recomputing.
		if _, err := c.ExecContext(ctx, `
DELETE FROM edge_candidates WHERE edge_id IN (
  SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
   WHERE f.worktree = ? AND e.kind = 'calls')`, worktree); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `
UPDATE edges SET dst_node_id = NULL
 WHERE kind = 'calls' AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree); err != nil {
			return err
		}

		// Non-call edges: unchanged name-match resolution.
		if err := resolveNonCallEdges(ctx, c, worktree); err != nil {
			return err
		}

		// Call edges: candidate strategies, highest-confidence first. OR IGNORE
		// keeps the first (best) confidence for a given (edge, target) pair.
		for _, strat := range callStrategies {
			if _, err := c.ExecContext(ctx, strat, worktree, worktree); err != nil {
				return err
			}
		}

		// Denormalize: edges.dst_node_id = best candidate per call edge — highest
		// confidence, then locality (same-file, then same-top-level-directory),
		// then a stable id order. Locality is the tiebreak among equal-confidence
		// candidates (e.g. several S5 name matches), mirroring the non-call
		// resolver so a sibling call binds to its own file's definition.
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
     ec.dst_node_id
   LIMIT 1)
 WHERE kind = 'calls' AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree)
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
     n.id
   LIMIT 1)
WHERE dst_node_id IS NULL
  AND dst_name <> ''
  AND kind <> 'calls'
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree, worktree)
	return err
}

// callStrategies are INSERT OR IGNORE passes into edge_candidates, applied in
// priority order (highest confidence first). Each binds (worktree, worktree).
var callStrategies = []string{
	stratSelf,
	stratTypedReceiver,
	stratStatic,
	stratInterface,
	stratNameFallback,
}

// stratInterface (S4, C#): the receiver's bound type T is an interface or base
// class; expand to <method> on each implementing/deriving type, found via the
// extends edges (X extends/implements T) and X's contained methods. Confidence
// 0.5 — a set of possible dispatch targets. Go interfaces are structural (no
// extends edge), so this yields nothing for Go (intended: Go stays S5).
const stratInterface = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.5, 'iface'
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes src ON src.id = ed.src_node_id
  JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  JOIN nodes iface ON iface.name = tb.type_name AND iface.kind IN ('interface','class')
  JOIN edges ext ON ext.kind = 'extends' AND ext.dst_node_id = iface.id
  JOIN edges cm  ON cm.kind = 'contains' AND cm.src_node_id = ext.src_node_id
  JOIN nodes m   ON m.id = cm.dst_node_id AND m.kind = 'method' AND m.name = ed.dst_name
  JOIN files mf  ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?`

// stratStatic (S3): the receiver token is itself a type name T (Foo.Create());
// bind to T.<method>. Confidence 0.9. A receiver that is a variable (no matching
// type-qualified node) simply yields no rows here.
const stratStatic = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'static'
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = ed.dst_recv || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || ed.dst_recv || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?
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
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN edges pc ON pc.kind = 'contains' AND pc.dst_node_id = ed.src_node_id
  JOIN edges cm ON cm.kind = 'contains' AND cm.src_node_id = pc.src_node_id
  JOIN nodes m  ON m.id = cm.dst_node_id
       AND m.kind IN ('method','property','constructor')
       AND m.name = ed.dst_name
 WHERE ed.kind = 'calls' AND (ed.dst_recv = '' OR ed.dst_recv = 'this')
   AND ef.worktree = ?
   AND ? IS NOT NULL`

// stratTypedReceiver (S2): the receiver identifier resolves, via a type_binding
// in scope at the call (the binding's scope contains the calling method's span —
// edges.src_node_id), to type T; bind to T.<method>. Confidence 0.9.
const stratTypedReceiver = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT ed.id, m.id, 0.9, 'typed'
  FROM edges ed
  JOIN files ef  ON ef.id = ed.file_id
  JOIN nodes src ON src.id = ed.src_node_id
  JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
       AND ( m.qualified_name = tb.type_name || '.' || ed.dst_name
             OR m.qualified_name LIKE '%.' || tb.type_name || '.' || ed.dst_name )
  JOIN files mf ON mf.id = m.file_id
 WHERE ed.kind = 'calls' AND ed.dst_recv <> ''
   AND ef.worktree = ? AND mf.worktree = ?
   AND ( ef.language = mf.language
         OR ( ef.language IN ('typescript','tsx','javascript')
              AND mf.language IN ('typescript','tsx','javascript') ) )`

// stratNameFallback (S5): name-only fallback — any same-name callable in the
// language family, capped per edge (locality-ordered), confidence 0.2. Only fires
// for edges with no better candidate yet, so it never dilutes a resolved edge.
const stratNameFallback = `
INSERT OR IGNORE INTO edge_candidates(edge_id, dst_node_id, confidence, method)
SELECT edge_id, dst_node_id, 0.2, 'name' FROM (
  SELECT ed.id AS edge_id, m.id AS dst_node_id,
    ROW_NUMBER() OVER (PARTITION BY ed.id ORDER BY
      CASE WHEN mf.id = ed.file_id THEN 0 ELSE 1 END,
      CASE WHEN substr(mf.path,1,instr(mf.path,'/')) = substr(ef.path,1,instr(ef.path,'/'))
           THEN 0 ELSE 1 END,
      m.id) AS rn
  FROM edges ed
  JOIN files ef ON ef.id = ed.file_id
  JOIN nodes m  ON m.kind IN ('method','property','constructor','function')
               AND m.name = ed.dst_name
  JOIN files mf ON mf.id = m.file_id
  WHERE ed.kind = 'calls' AND ed.dst_name <> ''
    AND ef.worktree = ? AND mf.worktree = ?
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
