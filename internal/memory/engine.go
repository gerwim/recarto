package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/embedding"
	"git.gerwim.nl/gerwim/recarto/internal/ids"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Engine is the memory subsystem: write+dedup and recall over the store.
type Engine struct {
	st    *store.Store
	led   *ledger.Ledger
	clk   clock.Clock
	lex   Lexical
	res   RefResolver // optional; nil → dangling-ref check disabled
	emb   Embedder
	floor bool
}

// New returns an Engine with the default FTS5 lexical backend.
func New(st *store.Store, led *ledger.Ledger, clk clock.Clock) *Engine {
	e := &Engine{st: st, led: led, clk: clk, lex: NewFTSLexical(st), floor: os.Getenv("RECARTO_RECALL_FLOOR") != "0"}
	if os.Getenv("RECARTO_SEMANTIC") != "0" {
		e.emb = embedding.Default()
	}
	return e
}

const selectMemoriesSQL = `
SELECT id, kind, title, body, search_text, created_at, updated_at,
       COALESCE(branch,''), COALESCE(worktree,''), author, source, confidence,
       reinforcement_count, pinned, status, COALESCE(superseded_by,''), simhash, extra
  FROM memories`

// insert writes a brand-new memory row + refs + minhash bands + FTS entry,
// optionally superseding a prior record. It runs on the caller's transaction
// connection c so that the write (and the preceding near-dup check) is atomic.
func (e *Engine) insert(ctx context.Context, c store.Conn, id string, nm NewMemory,
	searchText string, sim uint64, bands [lshBands]uint64, now int64) error {
	extraJSON, err := marshalExtra(nm.Extra)
	if err != nil {
		return err
	}
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
		// Drop the superseded row from the FTS index so it stops surfacing as a
		// lexical candidate (recall filters it by status anyway, but this keeps
		// the candidate pool clean).
		if err := e.lex.Delete(ctx, c, nm.SupersedesID); err != nil {
			return err
		}
	}
	return e.lex.Index(ctx, c, id, searchText)
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

// ConflictCandidate is an existing memory the new one may contradict or
// supersede: similar enough to be about the same thing (or anchored to the same
// code), not similar enough to auto-reinforce. Judgment belongs to the
// recording agent — Recarto surfaces, never decides.
type ConflictCandidate struct {
	ID      string
	Title   string
	Snippet string // body prefix, ~160 chars
	Reason  string // "similar wording" | "shares refs"
}

// conflictKinds are the judgment-bearing kinds where an opposite claim is
// costly if it lingers. Bugs and todos are factual/transient and excluded.
var conflictKinds = map[Kind]bool{
	KindPreference: true, KindDecision: true, KindDoNotRepeat: true, KindLearning: true,
}

const (
	// conflictJaccardFloor..dedupThreshold is the "same topic, different claim"
	// middle band: below the floor is unrelated, at the threshold it reinforces.
	conflictJaccardFloor  = 0.25
	maxConflictCandidates = 3
	conflictSnippetLen    = 160
)

// bodySnippet trims a body to the candidate-snippet budget on a rune boundary.
func bodySnippet(body string) string {
	r := []rune(body)
	if len(r) <= conflictSnippetLen {
		return body
	}
	return string(r[:conflictSnippetLen]) + "…"
}

// Record inserts nm, or reinforces a near-duplicate same-kind active memory.
// It returns the affected row id, whether it was a reinforcement, and advisory
// conflict candidates (existing memories that may contradict the new one).
//
// The near-dup check and the insert/reinforce run inside a SINGLE write
// transaction (BEGIN IMMEDIATE), so the check-and-act is atomic across
// goroutines and processes: a concurrent racer reads the winner's just-
// committed row inside its own immediate transaction and reinforces instead of
// inserting a duplicate — the §7.9 convergence guarantee.
func (e *Engine) Record(ctx context.Context, nm NewMemory) (id string, reinforced bool, conflicts []ConflictCandidate, err error) {
	if !nm.Kind.Valid() {
		return "", false, nil, errInvalidKind
	}
	if nm.Kind == KindDoNotRepeat {
		nm.Pinned = true
	}
	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	bands := bandHashes(minHashSignature(set))
	sim := simHash64(set)
	now := e.clk.Now().UnixMilli()

	if txErr := e.st.WriteTx(ctx, func(c store.Conn) error {
		candID, found, mid, ferr := e.findNearDup(ctx, c, nm.Kind, bands, set)
		if ferr != nil {
			return ferr
		}
		if found {
			id, reinforced = candID, true
			if err := e.reinforce(ctx, c, candID, nm, now); err != nil {
				return err
			}
			return e.markDirty(ctx, c, candID, now)
		}
		id, reinforced = ids.New(), false
		if err := e.insert(ctx, c, id, nm, searchText, sim, bands, now); err != nil {
			return err
		}
		if err := e.markDirty(ctx, c, id, now); err != nil {
			return err
		}
		if nm.SupersedesID != "" {
			// insert() just flipped the superseded row to status='superseded' — an
			// explicit, locally-authored state change (not compaction), so it must
			// propagate like any other authored write.
			if err := e.markDirty(ctx, c, nm.SupersedesID, now); err != nil {
				return err
			}
		}
		// Advisory contradiction suspects, judged by the recording agent. Same
		// transaction, so the read is consistent with the insert; never blocks it.
		if conflictKinds[nm.Kind] {
			seen := map[string]bool{}
			for _, m := range mid {
				if !seen[m.ID] {
					seen[m.ID] = true
					conflicts = append(conflicts, m)
				}
			}
			if refCands, rerr := e.refConflicts(ctx, c, nm.Kind, id, nm.Refs); rerr == nil {
				for _, m := range refCands {
					if !seen[m.ID] {
						seen[m.ID] = true
						conflicts = append(conflicts, m)
					}
				}
			}
			if len(conflicts) > maxConflictCandidates {
				conflicts = conflicts[:maxConflictCandidates]
			}
		}
		return nil
	}); txErr != nil {
		return "", false, nil, txErr
	}

	// Best-effort counter bump. It MUST run outside the WriteTx above: the
	// store's write mutex is non-reentrant, so calling led.Incr (which opens
	// its own WriteTx) while holding it would deadlock. A failed stat must not
	// fail an already-committed Record, so the error is intentionally ignored.
	_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
	return id, reinforced, conflicts, nil
}

// midCandidate is an internal pairing of a ConflictCandidate with its Jaccard
// score so we can sort before returning (highest similarity first).
type midCandidate struct {
	cc ConflictCandidate
	j  float64
}

// findNearDup returns the id of an active same-kind memory whose body has
// Jaccard >= dedupThreshold with set (the reinforce target), plus the
// middle-band candidates (conflictJaccardFloor <= j < dedupThreshold) that are
// about the same topic without being the same statement — the contradiction
// suspects Record surfaces. Middle-band candidates are returned sorted by
// Jaccard descending so the highest-similarity suspects survive the cap in Record.
func (e *Engine) findNearDup(ctx context.Context, c store.Conn, kind Kind,
	bands [lshBands]uint64, set map[string]struct{}) (string, bool, []ConflictCandidate, error) {
	seen := map[string]bool{}
	var candidates []string
	for b := 0; b < lshBands; b++ {
		rows, err := c.QueryContext(ctx,
			`SELECT mh.memory_id
			   FROM memory_minhash mh
			   JOIN memories m ON m.id = mh.memory_id
			  WHERE mh.band = ? AND mh.hash = ? AND m.kind = ? AND m.status = 'active'`,
			b, int64(bands[b]), string(kind))
		if err != nil {
			return "", false, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", false, nil, err
			}
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", false, nil, err
		}
		rows.Close()
	}
	var mid []midCandidate
	for _, id := range candidates {
		var title, body string
		if err := c.QueryRowContext(ctx,
			`SELECT title, body FROM memories WHERE id = ?`, id).Scan(&title, &body); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return "", false, nil, err
		}
		j := jaccard(set, shingleSet(body))
		if j >= dedupThreshold {
			return id, true, nil, nil
		}
		if j >= conflictJaccardFloor {
			mid = append(mid, midCandidate{
				cc: ConflictCandidate{ID: id, Title: title, Snippet: bodySnippet(body), Reason: "similar wording"},
				j:  j,
			})
		}
	}
	// Sort highest Jaccard first so the most-similar suspects survive the cap.
	sort.SliceStable(mid, func(i, k int) bool { return mid[i].j > mid[k].j })
	out := make([]ConflictCandidate, len(mid))
	for i, m := range mid {
		out[i] = m.cc
	}
	return "", false, out, nil
}

// refConflicts returns active same-kind memories (excluding selfID) anchored to
// any of the new memory's file/symbol refs — the structural "same topic" signal
// when wording differs entirely.
func (e *Engine) refConflicts(ctx context.Context, c store.Conn, kind Kind, selfID string, refs []Ref) ([]ConflictCandidate, error) {
	var args []any
	var or []string
	for _, r := range refs {
		if r.Type != RefFile && r.Type != RefSymbol {
			continue
		}
		or = append(or, "(r.ref_type = ? AND r.ref_value = ?)")
		args = append(args, string(r.Type), r.Value)
	}
	if len(or) == 0 {
		return nil, nil
	}
	// Most recently updated first: under Record's candidate cap the suspects
	// most likely to still matter survive, deterministically (id breaks ties).
	q := `SELECT DISTINCT m.id, m.title, m.body, m.updated_at
	        FROM memories m JOIN memory_refs r ON r.memory_id = m.id
	       WHERE m.kind = ? AND m.status = 'active' AND m.id != ? AND (` + strings.Join(or, " OR ") + `)
	       ORDER BY m.updated_at DESC, m.id
	       LIMIT 6`
	rows, err := c.QueryContext(ctx, q, append([]any{string(kind), selfID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConflictCandidate
	for rows.Next() {
		var id, title, body string
		var updatedAt int64 // selected only so DISTINCT permits the ORDER BY
		if err := rows.Scan(&id, &title, &body, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, ConflictCandidate{ID: id, Title: title, Snippet: bodySnippet(body), Reason: "shares refs"})
	}
	return out, rows.Err()
}

// reinforce bumps an existing memory's reinforcement_count and confidence,
// refreshes updated_at, and unions in any new refs. It runs on the caller's
// transaction connection c.
//
// The winning row's body/search_text and its FTS index are intentionally NOT
// rewritten here: lexical content from a reinforcing restatement is dropped
// (Phase 1C may revisit), but the unioned refs keep the structural signal —
// the recall workhorse — current.
func (e *Engine) reinforce(ctx context.Context, c store.Conn, id string, nm NewMemory, now int64) error {
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
}
