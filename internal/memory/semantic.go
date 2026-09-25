package memory

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/embedding"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Embedder turns text into an L2-normalized vector. ID identifies the vector
// space, so changing models lazily invalidates persisted vectors.
type Embedder interface {
	ID() string
	Embed(text string) []float32
}

// SetEmbedder installs the semantic backend; nil disables semantic recall.
func (e *Engine) SetEmbedder(emb Embedder) { e.emb = emb }

type semanticHits struct {
	ranked []string
	cos    map[string]float64
}

func (e *Engine) semanticArm(ctx context.Context, q RecallQuery, n int) *semanticHits {
	if e.emb == nil || q.LexicalOnly || strings.TrimSpace(q.TaskHint) == "" {
		return nil
	}
	qv := e.emb.Embed(strings.TrimSpace(q.TaskHint))
	if qv == nil {
		return nil
	}
	vecs, err := e.memoryVectors(ctx, q.Kind)
	if err != nil {
		return nil
	}
	hits := &semanticHits{cos: make(map[string]float64, len(vecs))}
	for id, vec := range vecs {
		if len(vec) != len(qv) {
			continue
		}
		var dot float64
		for i, x := range qv {
			dot += float64(x) * float64(vec[i])
		}
		hits.cos[id] = dot
	}
	hits.ranked = make([]string, 0, len(hits.cos))
	for id := range hits.cos {
		hits.ranked = append(hits.ranked, id)
	}
	sort.Slice(hits.ranked, func(i, j int) bool {
		a, b := hits.ranked[i], hits.ranked[j]
		if hits.cos[a] != hits.cos[b] {
			return hits.cos[a] > hits.cos[b]
		}
		return a < b
	})
	if n >= 0 && len(hits.ranked) > n {
		hits.ranked = hits.ranked[:n]
	}
	return hits
}

type vectorSource struct {
	id        string
	updatedAt int64
	title     string
	body      string
	vec       []byte
}

func (e *Engine) memoryVectors(ctx context.Context, kind Kind) (map[string][]float32, error) {
	// ponytail: this brute-force scan re-reads every active vector (~5–8 ms at
	// 1k memories); add an in-process cache or vec0 KNN only past ~5k memories.
	query := `SELECT m.id, m.updated_at, v.model, v.source_at, v.vec
		FROM memories m LEFT JOIN memory_vectors v ON v.memory_id=m.id
		WHERE m.status='active'`
	args := []any{}
	if kind != "" {
		query += ` AND m.kind=?`
		args = append(args, kind)
	}
	rows, err := e.st.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	type storedVector struct {
		id        string
		updatedAt int64
		model     *string
		sourceAt  *int64
		vec       []byte
	}
	var stored []storedVector
	for rows.Next() {
		var row storedVector
		var model *string
		var sourceAt *int64
		if err := rows.Scan(&row.id, &row.updatedAt, &model, &sourceAt, &row.vec); err != nil {
			rows.Close()
			return nil, err
		}
		row.model, row.sourceAt = model, sourceAt
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	vecs := make(map[string][]float32, len(stored))
	var stale []vectorSource
	for _, row := range stored {
		if row.model != nil && row.sourceAt != nil && *row.model == e.emb.ID() && *row.sourceAt == row.updatedAt {
			if vec := decodeVec(row.vec); vec != nil {
				vecs[row.id] = vec
			}
			continue
		}
		stale = append(stale, vectorSource{id: row.id, updatedAt: row.updatedAt})
	}
	var persist []vectorSource
	for start := 0; start < len(stale); start += 500 {
		end := min(start+500, len(stale))
		chunk := stale[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, item := range chunk {
			placeholders[i] = "?"
			args[i] = item.id
		}
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT id, title, body, updated_at FROM memories WHERE status='active' AND id IN (`+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		var current []vectorSource
		for rows.Next() {
			var item vectorSource
			if err := rows.Scan(&item.id, &item.title, &item.body, &item.updatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			current = append(current, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(current) == 0 {
			continue
		}
		for _, item := range current {
			vec := e.emb.Embed(item.title + "\n" + item.body)
			vecs[item.id] = vec
			item.vec = encodeVec(vec)
			persist = append(persist, item)
		}
	}
	if len(persist) > 0 {
		// Best effort: the current recall uses its in-memory vectors even when a
		// concurrent delete or a transient write failure prevents persistence.
		_ = e.st.WriteTx(ctx, func(c store.Conn) error {
			for _, item := range persist {
				if _, err := c.ExecContext(ctx, `INSERT INTO memory_vectors(memory_id, model, source_at, vec)
					SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM memories WHERE id=?)
					ON CONFLICT(memory_id) DO UPDATE SET model=excluded.model, source_at=excluded.source_at, vec=excluded.vec`,
					item.id, e.emb.ID(), item.updatedAt, item.vec, item.id); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return vecs, nil
}

func encodeVec(vec []float32) []byte {
	if len(vec) == 0 {
		return []byte{}
	}
	out := make([]byte, len(vec)*4)
	for i, x := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

func decodeVec(data []byte) []float32 {
	if len(data) == 0 || len(data)%4 != 0 {
		return nil
	}
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out
}

var _ Embedder = (*embedding.Model)(nil)
