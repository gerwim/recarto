package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Engine is the memory subsystem: write+dedup and recall over the store.
type Engine struct {
	st  *store.Store
	led *ledger.Ledger
	clk clock.Clock
	lex Lexical
}

// New returns an Engine with the default FTS5 lexical backend.
func New(st *store.Store, led *ledger.Ledger, clk clock.Clock) *Engine {
	return &Engine{st: st, led: led, clk: clk, lex: NewFTSLexical(st)}
}

const selectMemoriesSQL = `
SELECT id, kind, title, body, search_text, created_at, updated_at,
       COALESCE(branch,''), COALESCE(worktree,''), author, source, confidence,
       reinforcement_count, pinned, status, COALESCE(superseded_by,''), simhash, extra
  FROM memories`

// insert writes a brand-new memory row + refs + minhash bands + FTS entry,
// optionally superseding a prior record, all in one transaction.
func (e *Engine) insert(ctx context.Context, id string, nm NewMemory,
	searchText string, sim uint64, bands [lshBands]uint64, now int64) error {
	extraJSON, err := marshalExtra(nm.Extra)
	if err != nil {
		return err
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`INSERT INTO memories
			   (id, kind, title, body, search_text, created_at, updated_at,
			    branch, worktree, author, source, confidence, reinforcement_count,
			    pinned, status, superseded_by, simhash, extra)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, string(nm.Kind), nm.Title, nm.Body, searchText, now, now,
			nullStr(nm.Branch), nm.Worktree, nm.Author, nm.Source, 1.0, 0,
			boolToInt(nm.Pinned), "active", nil, int64(sim), extraJSON,
		); err != nil {
			return err
		}
		for _, r := range nm.Refs {
			if _, err := c.ExecContext(ctx,
				`INSERT OR IGNORE INTO memory_refs(memory_id, ref_type, ref_value)
				 VALUES (?,?,?)`, id, string(r.Type), r.Value); err != nil {
				return err
			}
		}
		for b := 0; b < lshBands; b++ {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memory_minhash(memory_id, band, hash) VALUES (?,?,?)`,
				id, b, int64(bands[b])); err != nil {
				return err
			}
		}
		if nm.SupersedesID != "" {
			if _, err := c.ExecContext(ctx,
				`UPDATE memories SET status='superseded', superseded_by=?, updated_at=?
				  WHERE id=? AND status='active'`, id, now, nm.SupersedesID); err != nil {
				return err
			}
		}
		return e.lex.Index(ctx, c, id, searchText)
	})
}

// GetByID returns a stored record (with refs) by id, or (nil, nil) if absent.
func (e *Engine) GetByID(ctx context.Context, id string) (*Record, error) {
	rows, err := e.st.DB.QueryContext(ctx, selectMemoriesSQL+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs[0], nil
}

func scanMemories(rows *sql.Rows) ([]*Record, error) {
	var out []*Record
	for rows.Next() {
		var r Record
		var kind, status, extra string
		var sim int64
		var pinned int
		if err := rows.Scan(&r.ID, &kind, &r.Title, &r.Body, &r.SearchText,
			&r.CreatedAt, &r.UpdatedAt, &r.Branch, &r.Worktree, &r.Author, &r.Source,
			&r.Confidence, &r.ReinforcementCount, &pinned, &status, &r.SupersededBy,
			&sim, &extra); err != nil {
			return nil, err
		}
		r.Kind = Kind(kind)
		r.Status = status
		r.Pinned = pinned != 0
		r.SimHash = uint64(sim)
		if extra != "" && extra != "{}" {
			_ = json.Unmarshal([]byte(extra), &r.Extra)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (e *Engine) loadRefs(ctx context.Context, recs []*Record) error {
	for _, r := range recs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT ref_type, ref_value FROM memory_refs WHERE memory_id = ?`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t, v string
			if err := rows.Scan(&t, &v); err != nil {
				rows.Close()
				return err
			}
			r.Refs = append(r.Refs, Ref{Type: RefType(t), Value: v})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullStr maps "" → SQL NULL so branch-global memories store NULL, not "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func marshalExtra(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// errInvalidKind is returned by Record for an unknown kind.
var errInvalidKind = errors.New("memory: invalid kind")

// dedupThreshold is the Jaccard similarity (over 2-word shingles) at/above which
// a new memory reinforces an existing same-kind record instead of inserting a
// new row. 0.55 sits mid-valley between the measured distinct-fact ceiling
// (~0.29) and the realistic re-statement floor (~0.60), giving ~0.25 margin each
// side. Research-backed (2-shingle Jaccard on short identifier-heavy bodies);
// lexical dedup deliberately does NOT catch full paraphrase (that would need the
// embeddings path rejected for v1).
const dedupThreshold = 0.55

// Record inserts nm, or reinforces a near-duplicate same-kind active memory.
// It returns the affected row id and whether it was a reinforcement.
func (e *Engine) Record(ctx context.Context, nm NewMemory) (id string, reinforced bool, err error) {
	if !nm.Kind.Valid() {
		return "", false, errInvalidKind
	}
	if nm.Kind == KindDoNotRepeat {
		nm.Pinned = true
	}
	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	bands := bandHashes(minHashSignature(set))
	sim := simHash64(set)
	now := e.clk.Now().UnixMilli()

	candID, found, err := e.findNearDup(ctx, nm.Kind, bands, set)
	if err != nil {
		return "", false, err
	}
	if found {
		if err := e.reinforce(ctx, candID, nm, now); err != nil {
			return "", false, err
		}
		_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
		return candID, true, nil
	}

	newID := ids.New()
	if err := e.insert(ctx, newID, nm, searchText, sim, bands, now); err != nil {
		return "", false, err
	}
	_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
	return newID, false, nil
}

// findNearDup returns the id of an active same-kind memory whose body has
// Jaccard >= dedupThreshold with set, using MinHash-LSH bands to gather
// candidates and exact shingle Jaccard to confirm.
func (e *Engine) findNearDup(ctx context.Context, kind Kind,
	bands [lshBands]uint64, set map[string]struct{}) (string, bool, error) {
	seen := map[string]bool{}
	var candidates []string
	for b := 0; b < lshBands; b++ {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT mh.memory_id
			   FROM memory_minhash mh
			   JOIN memories m ON m.id = mh.memory_id
			  WHERE mh.band = ? AND mh.hash = ? AND m.kind = ? AND m.status = 'active'`,
			b, int64(bands[b]), string(kind))
		if err != nil {
			return "", false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", false, err
			}
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", false, err
		}
		rows.Close()
	}
	for _, id := range candidates {
		var body string
		if err := e.st.DB.QueryRowContext(ctx,
			`SELECT body FROM memories WHERE id = ?`, id).Scan(&body); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return "", false, err
		}
		if jaccard(set, shingleSet(body)) >= dedupThreshold {
			return id, true, nil
		}
	}
	return "", false, nil
}

// reinforce bumps an existing memory's reinforcement_count and confidence,
// refreshes updated_at, and unions in any new refs.
func (e *Engine) reinforce(ctx context.Context, id string, nm NewMemory, now int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories
			    SET reinforcement_count = reinforcement_count + 1,
			        updated_at = ?,
			        confidence = MIN(1.0, confidence + 0.1)
			  WHERE id = ?`, now, id); err != nil {
			return err
		}
		for _, r := range nm.Refs {
			if _, err := c.ExecContext(ctx,
				`INSERT OR IGNORE INTO memory_refs(memory_id, ref_type, ref_value)
				 VALUES (?,?,?)`, id, string(r.Type), r.Value); err != nil {
				return err
			}
		}
		return nil
	})
}
