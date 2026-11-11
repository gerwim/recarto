package memory

import (
	"context"
	"strings"

	"github.com/gerwim/recarto/internal/store"
)

// LexResult is one lexical match: a memory id and a normalized score in [0,1]
// (higher = more relevant).
type LexResult struct {
	ID    string
	Score float64
}

// Lexical is the swappable lexical-search backend. The default is FTS5; a
// Go-side inverted index is the documented fallback if FTS5 is unavailable
// (spec §15). Index/Delete run inside the caller's write transaction so the
// lexical index stays atomic with the memories row.
type Lexical interface {
	Index(ctx context.Context, c store.Conn, id, searchText string) error
	Delete(ctx context.Context, c store.Conn, id string) error
	Search(ctx context.Context, query string, limit int) ([]LexResult, error)
}

// ftsLexical is the FTS5-backed implementation over the memories_fts vtable.
type ftsLexical struct{ st *store.Store }

// NewFTSLexical returns the default FTS5 lexical backend.
func NewFTSLexical(st *store.Store) Lexical { return &ftsLexical{st: st} }

func (f *ftsLexical) Index(ctx context.Context, c store.Conn, id, searchText string) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO memories_fts(memory_id, search_text) VALUES (?, ?)`, id, searchText)
	return err
}

func (f *ftsLexical) Delete(ctx context.Context, c store.Conn, id string) error {
	_, err := c.ExecContext(ctx, `DELETE FROM memories_fts WHERE memory_id = ?`, id)
	return err
}

// matchQuery builds a safe FTS5 MATCH expression: each query token is quoted
// and OR-combined, so arbitrary punctuation cannot trigger an FTS5 syntax error.
func matchQuery(query string) string {
	toks := tokenize(query)
	if len(toks) == 0 {
		return ""
	}
	quoted := make([]string, len(toks))
	for i, t := range toks {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}

func (f *ftsLexical) Search(ctx context.Context, query string, limit int) ([]LexResult, error) {
	m := matchQuery(query)
	if m == "" {
		return nil, nil
	}
	// NOTE: FTS5 bm25() returns NEGATIVE scores; more negative = more relevant.
	rows, err := f.st.DB.QueryContext(ctx,
		`SELECT memory_id, bm25(memories_fts) AS s
		   FROM memories_fts
		  WHERE memories_fts MATCH ?
		  ORDER BY s ASC
		  LIMIT ?`, m, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	var raw []float64
	for rows.Next() {
		var id string
		var s float64
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		raw = append(raw, -s) // flip so higher = better
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return normalizeScores(ids, raw), nil
}

// normalizeScores min-max normalizes raw scores into [0,1]; if all scores are
// equal (or a single result), every result scores 1.0.
func normalizeScores(ids []string, raw []float64) []LexResult {
	out := make([]LexResult, len(ids))
	if len(ids) == 0 {
		return out
	}
	min, max := raw[0], raw[0]
	for _, v := range raw {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	for i := range ids {
		score := 1.0
		if span > 0 {
			score = (raw[i] - min) / span
		}
		out[i] = LexResult{ID: ids[i], Score: score}
	}
	return out
}
