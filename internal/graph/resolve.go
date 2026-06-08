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
		if _, err := c.ExecContext(ctx, stratNameFallback, worktree, worktree); err != nil {
			return err
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
     -- Stable tiebreak (path, then position) so the winner among equal-confidence,
     -- equal-locality candidates is reproducible across indexes rather than decided
     -- by a random node id — which made two indexes of the same code disagree and
     -- defeated incremental-resolve equivalence. The id stays last for a total order.
     mf.path, n.start_byte, ec.dst_node_id
   LIMIT 1)
 WHERE kind = 'calls' AND file_id IN (SELECT id FROM files WHERE worktree = ?)`, worktree)
		if err != nil {
			return err
		}
		// Rebuild the dst_node_id index dropped above, in one bulk pass over the final
		// candidate set rather than incrementally across every insert.
		if _, err := c.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS idx_edge_candidates_dst ON edge_candidates(dst_node_id)`); err != nil {
			return err
		}
		// Drop the shared prefix temp table so it does not linger on this pooled
		// connection after the transaction commits.
		_, err = c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`)
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
func buildReceiverTypes(ctx context.Context, c store.Conn, worktree string) error {
	if _, err := c.ExecContext(ctx, `DROP TABLE IF EXISTS recv_types`); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, `
CREATE TEMP TABLE recv_types AS
SELECT DISTINCT ed.id AS edge_id, ed.dst_name AS dst_name,
       ef.language AS ef_language, tb.type_name AS type_name
  FROM files ef
  CROSS JOIN edges ed ON ed.file_id = ef.id
       AND ed.kind = 'calls' AND ed.dst_recv <> ''
  CROSS JOIN nodes src ON src.id = ed.src_node_id
  CROSS JOIN type_bindings tb ON tb.file_id = ed.file_id
       AND tb.name = ed.dst_recv
       AND tb.scope_start_byte <= src.start_byte
       AND src.end_byte <= tb.scope_end_byte
 WHERE ef.worktree = ?`, worktree)
	return err
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
}

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
