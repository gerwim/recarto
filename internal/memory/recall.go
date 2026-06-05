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
)

// RecallQuery is the input to Engine.Recall.
type RecallQuery struct {
	TaskHint    string
	Files       []string
	Symbols     []string
	Kind        Kind   // "" → any kind
	Branch      string // current branch, for branch-affinity boost
	Baseline    bool   // session-start: also seed candidates with recent cerebrum memories (no query needed)
	Limit       int    // max lexical candidates (0 → default)
	TokenBudget int    // 0 → default
	Worktree    string // current worktree absolute path, for ref-resolvability
}

// RecallResult is a selected memory with its score and per-signal breakdown.
type RecallResult struct {
	Record       *Record
	Score        float64
	Signals      map[string]float64
	DanglingRefs []Ref // code refs absent from the current worktree (nil if none)
}

// Recall scores and ranks memories by structural overlap, lexical (BM25),
// recency, reinforcement, and branch affinity, all multiplied by confidence.
// Task 12 wraps the budgeter around the sorted output.
func (e *Engine) Recall(ctx context.Context, q RecallQuery) ([]RecallResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	lex, err := e.lex.Search(ctx, lexicalQueryText(q), limit)
	if err != nil {
		return nil, err
	}
	lexScore := make(map[string]float64, len(lex))
	for _, r := range lex {
		lexScore[r.ID] = r.Score
	}

	refScore, err := e.structuralScores(ctx, q)
	if err != nil {
		return nil, err
	}

	pinned, err := e.pinnedIDs(ctx, q.Branch)
	if err != nil {
		return nil, err
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
		base, err := e.baselineIDs(ctx, q.Branch, limit)
		if err != nil {
			return nil, err
		}
		for _, id := range base {
			idset[id] = true
		}
	}

	recs, err := e.loadByIDs(ctx, idset)
	if err != nil {
		return nil, err
	}

	now := e.clk.Now().UnixMilli()
	scored := make([]RecallResult, 0, len(recs))
	for _, r := range recs {
		if r.Status != "active" {
			continue
		}
		// The kind filter applies to lexical/structural candidates, but a pinned
		// do_not_repeat is ALWAYS eligible (spec §9.2) and must never be filtered
		// out by q.Kind.
		isPin := r.Kind == KindDoNotRepeat && r.Pinned
		if q.Kind != "" && r.Kind != q.Kind && !isPin {
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
		scored = append(scored, RecallResult{Record: r, Score: score, Signals: sig})
	}

	budget := q.TokenBudget
	if budget <= 0 {
		budget = defaultTokenBudget
	}
	out, used := selectWithinBudget(scored, budget)
	if len(out) == 0 {
		// A recall that surfaces nothing above threshold — whether no candidates
		// matched or the budget excluded them all — is a miss. This counter is the
		// evidence that justifies (or not) the deferred embeddings path (§9.2/§10).
		_ = e.led.Incr(ctx, "lifetime", "recall_misses", 1)
		return nil, nil
	}
	_ = e.led.Incr(ctx, "lifetime", "memories_recalled", int64(len(out)))
	_ = e.led.Incr(ctx, "lifetime", "recall_tokens_injected", int64(used))
	// Stamp recall usage so decay can measure "epochs since last used"
	// (Phase 3 compaction). Best-effort — must not fail the recall.
	_ = e.stampRecalled(ctx, out)
	return out, nil
}

// selectWithinBudget always includes pinned do_not_repeat (a dedicated, budget-
// exempt slice), then greedily adds the highest-scoring remaining records until
// the token budget is exhausted. It returns the selection and its estimated
// token cost. It is a pure function (no side effects) so the caller owns the
// ledger accounting and the empty-result (miss) decision.
func selectWithinBudget(scored []RecallResult, budget int) (out []RecallResult, used int) {
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
	for _, s := range pinned { // pins are always included, budget-exempt
		out = append(out, s)
		used += tokensOf(s.Record)
	}
	for _, s := range rest {
		t := tokensOf(s.Record)
		if used+t > budget {
			continue
		}
		out = append(out, s)
		used += t
	}
	return out, used
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
// learning, decision, and (unpinned) do_not_repeat — most-recently-updated first,
// capped at limit. It seeds an empty-query session-start recall so durable
// knowledge surfaces without a task hint. Ephemeral kinds (action) and bugs are
// intentionally excluded: bugs surface just-in-time on code contact instead.
func (e *Engine) baselineIDs(ctx context.Context, branch string, limit int) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE status='active'
		    AND kind IN ('preference','learning','decision','do_not_repeat')
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
