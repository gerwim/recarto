package memory

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
)

// Recall scoring weights (golden-tested defaults). Structural overlap is the
// strongest signal: an exact file/symbol match is the best relevance evidence.
const (
	wStructural    = 0.45
	wLexical       = 0.25
	wRecency       = 0.15
	wReinforcement = 0.10
	wBranch        = 0.05

	recencyHalfLifeMs  = float64(14 * 24 * 60 * 60 * 1000) // 14 days
	defaultTokenBudget = 1500
	defaultLimit       = 50

	// pinSubBudgetDivisor bounds the "dedicated sub-budget" for pinned do_not_repeat
	// (spec §9.2): pins may consume at most budget/pinSubBudgetDivisor tokens, so a
	// pin-heavy branch cannot drown the query. The single top-ranked pin is always
	// included regardless (the §9.2 floor).
	pinSubBudgetDivisor = 2

	// danglingDemotion multiplies the score of a non-pinned, non-bug memory whose
	// every code ref is absent from the current worktree (spec §5.3).
	danglingDemotion = 0.25
)

// RecallQuery is the input to Engine.Recall.
type RecallQuery struct {
	TaskHint    string
	Files       []string
	Symbols     []string
	Kind        Kind    // "" → any kind
	Branch      string  // current branch, for branch-affinity boost
	Baseline    bool    // session-start: also seed candidates with recent cerebrum memories (no query needed)
	Limit       int     // max returned query-matched results (0 → default)
	TokenBudget int     // 0 → default
	Worktree    string  // current worktree absolute path, for ref-resolvability
	MinScore    float64 // drop candidates scoring below this (0 → no floor)
}

// RecallResult is a selected memory with its score and per-signal breakdown.
type RecallResult struct {
	Record       *Record
	Score        float64
	Signals      map[string]float64
	DanglingRefs []Ref // code refs absent from the current worktree (nil if none)
}

// MatchedOn names the signals that drove this result's score, ordered by weighted
// contribution (desc), keeping any whose contribution is at least 20% of the top
// signal's. It lets a consuming LLM see *why* a memory surfaced. Returns nil when
// nothing contributed.
func (r RecallResult) MatchedOn() []string {
	weights := map[string]float64{
		"structural": wStructural, "lexical": wLexical, "recency": wRecency,
		"reinforcement": wReinforcement, "branch": wBranch,
	}
	type contrib struct {
		name string
		v    float64
	}
	var cs []contrib
	var top float64
	for name, w := range weights {
		v := w * r.Signals[name]
		if v > top {
			top = v
		}
		cs = append(cs, contrib{name, v})
	}
	if top <= 0 {
		return nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].v > cs[j].v })
	var out []string
	for _, c := range cs {
		if c.v >= 0.2*top {
			out = append(out, c.name)
		}
	}
	return out
}

// RecallResultSet wraps the selected results with budget telemetry so a caller
// can tell whether ranked candidates were dropped for space (Truncated) and how
// much of the budget the returned set consumed (TokensUsed).
type RecallResultSet struct {
	Results    []RecallResult
	TokensUsed int
	Truncated  bool
}

// Recall scores and ranks memories by structural overlap, lexical (BM25),
// recency, reinforcement, and branch affinity, all multiplied by confidence.
// Task 12 wraps the budgeter around the sorted output.
func (e *Engine) Recall(ctx context.Context, q RecallQuery) (RecallResultSet, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	// `limit` caps the *returned* set (applied in selectWithinBudget). The
	// candidate pool stays at least defaultLimit wide so ranking quality does not
	// degrade when the caller asks for only a few results.
	candidateN := max(limit, defaultLimit)

	lex, err := e.lex.Search(ctx, lexicalQueryText(q), candidateN)
	if err != nil {
		return RecallResultSet{}, err
	}
	lexScore := make(map[string]float64, len(lex))
	for _, r := range lex {
		lexScore[r.ID] = r.Score
	}

	refScore, err := e.structuralScores(ctx, q)
	if err != nil {
		return RecallResultSet{}, err
	}

	pinned, err := e.pinnedIDs(ctx, q.Branch)
	if err != nil {
		return RecallResultSet{}, err
	}

	idset := map[string]bool{}
	for id := range lexScore {
		idset[id] = true
	}
	for id := range refScore {
		idset[id] = true
	}
	for _, id := range pinned {
		idset[id] = true
	}

	if q.Baseline {
		base, err := e.baselineIDs(ctx, q.Branch, candidateN)
		if err != nil {
			return RecallResultSet{}, err
		}
		for _, id := range base {
			idset[id] = true
		}
	}

	recs, err := e.loadByIDs(ctx, idset)
	if err != nil {
		return RecallResultSet{}, err
	}

	now := e.clk.Now().UnixMilli()
	scored := make([]RecallResult, 0, len(recs))
	for _, r := range recs {
		if r.Status != "active" {
			continue
		}
		// An explicit kind filter is authoritative: pinned do_not_repeat are
		// filtered like any other kind. §9.2's always-include guarantee applies to
		// UNFILTERED recall (q.Kind == ""); pins additionally retain their
		// session-start tier and just-in-time anchored surfacing, so they are not
		// lost when a caller deliberately narrows by kind.
		if q.Kind != "" && r.Kind != q.Kind {
			continue
		}
		sig := map[string]float64{
			"lexical":       lexScore[r.ID],
			"structural":    refScore[r.ID],
			"recency":       math.Exp(-float64(now-r.UpdatedAt) / recencyHalfLifeMs),
			"reinforcement": math.Min(1, math.Log1p(float64(r.ReinforcementCount))/math.Log(11)),
			"branch":        0,
		}
		if r.Branch == "" || r.Branch == q.Branch {
			sig["branch"] = 1
		}
		score := (wStructural*sig["structural"] + wLexical*sig["lexical"] +
			wRecency*sig["recency"] + wReinforcement*sig["reinforcement"] +
			wBranch*sig["branch"]) * r.Confidence

		var dangling []Ref
		if e.res != nil && q.Worktree != "" {
			code := codeRefsOf(r.Refs)
			if len(code) > 0 {
				if d, derr := e.res.Dangling(ctx, q.Worktree, code); derr == nil && len(d) > 0 {
					dangling = d
					privileged := r.Pinned || r.Kind == KindBug
					if len(d) == len(code) && !privileged {
						score *= danglingDemotion
					}
				}
			}
		}
		scored = append(scored, RecallResult{Record: r, Score: score, Signals: sig, DanglingRefs: dangling})
	}

	if q.MinScore > 0 {
		kept := scored[:0]
		for _, s := range scored {
			if s.Score >= q.MinScore {
				kept = append(kept, s)
			}
		}
		scored = kept
	}

	budget := q.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	out, used, truncated := selectWithinBudget(scored, budget, limit)
	if len(out) == 0 {
		// A recall that surfaces nothing above threshold — whether no candidates
		// matched or the budget excluded them all — is a miss. This counter is the
		// evidence that justifies (or not) the deferred embeddings path (§9.2/§10).
		_ = e.led.Incr(ctx, "lifetime", "recall_misses", 1)
		return RecallResultSet{}, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "memories_recalled", int64(len(out)))
	_ = e.led.Incr(ctx, "lifetime", "recall_tokens_injected", int64(used))
	// Stamp recall usage so decay can measure "epochs since last used"
	// (Phase 3 compaction). Best-effort — must not fail the recall.
	_ = e.stampRecalled(ctx, out)
	return RecallResultSet{Results: out, TokensUsed: used, Truncated: truncated}, nil
}

// selectWithinBudget assembles the returned set: pinned do_not_repeat first under
// a dedicated sub-budget (spec §9.2 — the top-ranked pin is always kept; lower-
// ranked pins are dropped once the sub-budget is spent so a pin-heavy branch
// cannot drown the query), then the highest-ranked remaining records up to the
// token budget and the caller's `limit` (max returned query-matched results). It
// returns the selection and its estimated token cost. It is a pure function (no
// side effects) so the caller owns the ledger accounting and the miss decision.
func selectWithinBudget(scored []RecallResult, budget, limit int) (out []RecallResult, used int, truncated bool) {
	var pinned, rest []RecallResult
	for _, s := range scored {
		if s.Record.Kind == KindDoNotRepeat && s.Record.Pinned {
			pinned = append(pinned, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].Score > pinned[j].Score })
	sort.Slice(rest, func(i, j int) bool { return rest[i].Score > rest[j].Score })

	tokensOf := func(r *Record) int { return ledger.EstimateTokens(r.Title + " " + r.Body) }

	// Pins: the top-ranked pin is always included (the §9.2 floor); add further
	// pins by score only while the dedicated sub-budget allows.
	pinBudget := budget / pinSubBudgetDivisor
	for i, s := range pinned {
		t := tokensOf(s.Record)
		if i > 0 && used+t > pinBudget {
			truncated = true
			continue
		}
		out = append(out, s)
		used += t
	}

	// Rest: highest-ranked first, bounded by the token budget and `limit`.
	restCount := 0
	for _, s := range rest {
		if limit > 0 && restCount >= limit {
			truncated = true
			break
		}
		t := tokensOf(s.Record)
		if used+t > budget {
			truncated = true
			continue
		}
		out = append(out, s)
		used += t
		restCount++
	}
	return out, used, truncated
}

// lexicalQueryText assembles the BM25 query from the task hint, file basenames,
// and symbol names.
func lexicalQueryText(q RecallQuery) string {
	parts := []string{q.TaskHint}
	for _, f := range q.Files {
		parts = append(parts, filepath.Base(f))
	}
	parts = append(parts, q.Symbols...)
	return strings.Join(parts, " ")
}

// structuralScores returns memory_id → weighted Jaccard between a memory's refs
// and the query's file/symbol refs.
func (e *Engine) structuralScores(ctx context.Context, q RecallQuery) (map[string]float64, error) {
	type qref struct{ t, v string }
	var qrefs []qref
	for _, f := range q.Files {
		qrefs = append(qrefs, qref{string(RefFile), f})
	}
	for _, s := range q.Symbols {
		qrefs = append(qrefs, qref{string(RefSymbol), s})
	}
	scores := map[string]float64{}
	if len(qrefs) == 0 {
		return scores, nil
	}
	inter := map[string]int{}
	for _, qr := range qrefs {
		rows, err := e.st.DB.QueryContext(ctx,
			`SELECT memory_id FROM memory_refs WHERE ref_type=? AND ref_value=?`, qr.t, qr.v)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			inter[id]++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	for id, n := range inter {
		var total int
		if err := e.st.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_refs WHERE memory_id=?`, id).Scan(&total); err != nil {
			return nil, err
		}
		union := len(qrefs) + total - n
		if union > 0 {
			scores[id] = float64(n) / float64(union)
		}
	}
	return scores, nil
}

// pinnedIDs returns active pinned do_not_repeat ids visible on branch (or repo-global).
func (e *Engine) pinnedIDs(ctx context.Context, branch string) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE pinned=1 AND status='active' AND kind='do_not_repeat'
		    AND (branch IS NULL OR branch=?)`, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// baselineIDs returns active, branch-visible "cerebrum" memories — preference,
// learning, decision, (unpinned) do_not_repeat, and open todos — most-recently-
// updated first, capped at limit. It seeds an empty-query session-start recall so
// durable knowledge and open work surface without a task hint. Bugs are
// excluded: they surface just-in-time on code contact.
func (e *Engine) baselineIDs(ctx context.Context, branch string, limit int) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE status='active'
		    AND kind IN ('preference','learning','decision','do_not_repeat','todo')
		    AND (branch IS NULL OR branch=?)
		  ORDER BY updated_at DESC
		  LIMIT ?`, branch, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loadByIDs loads records (with refs) for the id set. Kind filtering is applied
// by the caller (Recall's scoring loop) so that pinned do_not_repeat can be
// exempted from it.
func (e *Engine) loadByIDs(ctx context.Context, idset map[string]bool) ([]*Record, error) {
	if len(idset) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(idset))
	ph := make([]string, 0, len(idset))
	for id := range idset {
		args = append(args, id)
		ph = append(ph, "?")
	}
	query := selectMemoriesSQL + ` WHERE id IN (` + strings.Join(ph, ",") + `)`
	rows, err := e.st.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	recs, err := scanMemories(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := e.loadRefs(ctx, recs); err != nil {
		return nil, err
	}
	return recs, nil
}
