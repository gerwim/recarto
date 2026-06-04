package graph

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	_ "modernc.org/sqlite/vec" // registers the vec0 virtual table on the modernc driver

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// embedBatch bounds how many symbol docs are embedded per model call / insert
// transaction during indexing.
const embedBatch = 64

// vecJSON renders a vector as the JSON array literal vec0 accepts for inserts and
// KNN MATCH operands.
func vecJSON(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ensureVectorTable creates the node_vectors vec0 table sized to the embedder's
// dimension. If a table already exists at a different dimension (the model
// changed), it is dropped and recreated empty so the next embed pass repopulates
// it. node_id is the vec0 primary key; worktree is a metadata column used to
// scope KNN to one worktree.
func (e *Engine) ensureVectorTable(ctx context.Context) error {
	dim := e.emb.Dim()
	var stored string
	_ = e.st.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='vec_dim'`).Scan(&stored)
	if stored != "" && stored != strconv.Itoa(dim) {
		if _, err := e.st.DB.ExecContext(ctx, `DROP TABLE IF EXISTS node_vectors`); err != nil {
			return err
		}
		stored = ""
	}
	if _, err := e.st.DB.ExecContext(ctx, fmt.Sprintf(`
CREATE VIRTUAL TABLE IF NOT EXISTS node_vectors USING vec0(
  node_id TEXT PRIMARY KEY,
  worktree TEXT,
  embedding float[%d]
)`, dim)); err != nil {
		return err
	}
	if stored != strconv.Itoa(dim) {
		if _, err := e.st.DB.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES('vec_dim',?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(dim)); err != nil {
			return err
		}
	}
	return nil
}

// embedWorktree brings the vector index for a worktree up to date: it drops
// vectors orphaned by re-indexing (their node_id no longer exists), then embeds
// every embeddable-kind symbol that lacks a vector and stores it. It is
// best-effort and only runs when an embedder is set; callers (Reconcile) ignore
// its error so a model hiccup never blocks indexing.
func (e *Engine) embedWorktree(ctx context.Context, worktree string) error {
	if e.emb == nil {
		return nil
	}
	if err := e.ensureVectorTable(ctx); err != nil {
		return err
	}
	// Drop vectors whose node disappeared (re-index assigns fresh node ids).
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx, `
DELETE FROM node_vectors
 WHERE worktree = ?
   AND node_id NOT IN (SELECT id FROM nodes)`, worktree)
		return err
	}); err != nil {
		return err
	}

	rows, err := e.st.DB.QueryContext(ctx, `
SELECT n.id, n.kind, n.name, n.qualified_name, f.path
  FROM nodes n
  JOIN files f ON f.id = n.file_id
 WHERE f.worktree = ?
   AND n.id NOT IN (SELECT node_id FROM node_vectors WHERE worktree = ?)`, worktree, worktree)
	if err != nil {
		return err
	}
	type pending struct{ id, doc string }
	var todo []pending
	for rows.Next() {
		var id, kind, name, qn, path string
		if err := rows.Scan(&id, &kind, &name, &qn, &path); err != nil {
			rows.Close()
			return err
		}
		if !embeddableKinds[kind] {
			continue
		}
		todo = append(todo, pending{id, symbolDoc(kind, name, qn, path)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for i := 0; i < len(todo); i += embedBatch {
		batch := todo[i:min(i+embedBatch, len(todo))]
		docs := make([]string, len(batch))
		for k, p := range batch {
			docs[k] = p.doc
		}
		vecs, err := e.emb.EmbedDocs(ctx, docs)
		if err != nil {
			return err
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("embedder returned %d vectors for %d docs", len(vecs), len(batch))
		}
		if err := e.st.WriteTx(ctx, func(c store.Conn) error {
			for k, p := range batch {
				if _, err := c.ExecContext(ctx,
					`INSERT INTO node_vectors(node_id, worktree, embedding) VALUES (?,?,?)`,
					p.id, worktree, vecJSON(vecs[k])); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// vectorPass embeds the query and returns the nearest symbols by vector
// similarity (cosine/L2 distance, nearest first), scoped to worktree and
// optionally filtered by kind/language. Returns nil when no embedder is set or
// the vector table is empty. Errors are returned so Search can fall back to a
// purely lexical result.
func (e *Engine) vectorPass(ctx context.Context, worktree, query, kind, lang string, limit int) ([]SearchResult, error) {
	if e.emb == nil {
		return nil, nil
	}
	// No table yet (nothing embedded) => no semantic results, not an error.
	var exists string
	if err := e.st.DB.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='node_vectors'`).Scan(&exists); err != nil {
		return nil, nil
	}
	qvec, err := e.emb.EmbedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(qvec) == 0 {
		return nil, nil
	}
	// Over-fetch from KNN so post-KNN kind/language filtering still yields enough.
	k := limit
	if kind != "" || lang != "" {
		k = limit * 4
	}
	args := []any{worktree, vecJSON(qvec), k}
	sql := `
WITH knn AS (
  SELECT node_id, distance
    FROM node_vectors
   WHERE worktree = ? AND embedding MATCH ? AND k = ?
)
SELECT n.id, n.name, n.qualified_name, n.kind, f.language, f.path, n.start_line, knn.distance
  FROM knn
  JOIN nodes n ON n.id = knn.node_id
  JOIN files f ON f.id = n.file_id
 WHERE 1=1`
	if kind != "" {
		sql += " AND n.kind = ?"
		args = append(args, kind)
	}
	if lang != "" {
		sql += " AND f.language = ?"
		args = append(args, lang)
	}
	sql += " ORDER BY knn.distance"

	rows, err := e.st.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var dist float64
		if err := rows.Scan(&r.NodeID, &r.Name, &r.QualifiedName, &r.Kind,
			&r.Language, &r.Path, &r.Line, &dist); err != nil {
			return nil, err
		}
		r.Score = -dist // nearer = higher
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
