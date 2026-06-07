package memory

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Compaction tuning. These are policy defaults (not empirically optimized) and
// are overridable per call (BudgetTokens) or, for the rest, by editing here.
const (
	// DefaultBudgetTokens is the live-set token budget. Deliberately generous:
	// normal/quiet repos stay well under it and never evict — eviction is a
	// safety valve against runaway growth, not routine behavior.
	DefaultBudgetTokens = 250_000

	halfLifeStdEpochs = 30.0 // preference / learning / todo
	archiveGraceEpochs   = 30   // archived -> hard-deleted after this many epochs

	defaultDedupBuckets = 1000 // dedup sweep work budget per pass (cursor resumes)

	minIntervalMs = int64(60 * 60 * 1000) // 1h: gate so epoch ~= active sessions
	leaseTTLMs    = int64(5 * 60 * 1000)  // 5m: single-flight lease lifetime
)

// meta keys owned by compaction.
const (
	metaActivityEpoch = "compact_activity_epoch"
	metaLastRunAt     = "compact_last_run_at"
	metaDedupCursor   = "dedup_sweep_cursor" // "band,hash" of last processed bucket
	metaLease         = "compact_lease"      // unix-ms until which a pass holds the lease
)

// getMetaInt reads an integer meta value via q (a *sql.DB or a tx Conn), or def
// if the key is absent or unparseable.
func getMetaInt(ctx context.Context, q store.Conn, key string, def int64) (int64, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if perr != nil {
		return def, nil
	}
	return n, nil
}

// setMetaInt upserts an integer meta value on the given connection.
func setMetaInt(ctx context.Context, c store.Conn, key string, v int64) error {
	return setMetaStr(ctx, c, key, strconv.FormatInt(v, 10))
}

// getMetaStr reads a string meta value, or def if absent.
func getMetaStr(ctx context.Context, q store.Conn, key, def string) (string, error) {
	var s string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	return s, nil
}

// setMetaStr upserts a string meta value on the given connection. It is the
// single owner of the meta UPSERT SQL (setMetaInt formats then delegates here).
func setMetaStr(ctx context.Context, c store.Conn, key, v string) error {
	_, err := c.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, v)
	return err
}

// Epoch returns the current activity epoch (>= 1). The epoch is a logical clock
// that advances once per gated compaction pass (~once per active session), so
// "staleness" is measured in engagement rather than wall-clock time.
func (e *Engine) Epoch(ctx context.Context) (int64, error) {
	return getMetaInt(ctx, e.st.DB, metaActivityEpoch, 1)
}

// evictable reports whether a kind may be archived by decay. do_not_repeat is
// pinned; decision and bug carry durable rationale/fix history and are exempt
// (they can still be deduped). preference/learning decay, and so do todos:
// an untouched todo fades (reinforcing it via a near-dup record refreshes it).
func evictable(k Kind) bool {
	switch k {
	case KindPreference, KindLearning, KindTodo:
		return true
	}
	return false
}

// halfLifeEpochs is the decay half-life (in activity epochs) for a kind. Only
// evictable kinds (preference/learning/todo) are ever scored; they share one
// half-life.
func halfLifeEpochs(Kind) float64 {
	return halfLifeStdEpochs
}

// relevance scores an eviction candidate; lower = more evictable. Decay is
// exponential in epochs-since-last-used, boosted by reinforcement and scaled by
// confidence. It only RANKS what to evict when over budget — it never by itself
// triggers eviction.
func relevance(k Kind, ageEpochs float64, reinforcement int, confidence float64) float64 {
	if ageEpochs < 0 {
		ageEpochs = 0
	}
	decay := math.Exp(-ageEpochs / halfLifeEpochs(k))
	return decay * (1 + math.Log1p(float64(reinforcement))) * confidence
}

// idsWithStatus returns the ids of rows in the given status (read on the pooled DB).
func (e *Engine) idsWithStatus(ctx context.Context, status string) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT id FROM memories WHERE status=?`, status)
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

// deleteMemoryRows removes the lexical index entries for ids then deletes the
// memory rows (CASCADE clears memory_refs + memory_minhash). Runs on the
// caller's transaction connection.
func (e *Engine) deleteMemoryRows(ctx context.Context, c store.Conn, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if err := e.lex.Delete(ctx, c, id); err != nil {
			return err
		}
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	_, err := c.ExecContext(ctx,
		`DELETE FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

// gcSuperseded hard-deletes all superseded rows. Returns the count removed (or,
// for dryRun, the count that would be removed).
func (e *Engine) gcSuperseded(ctx context.Context, dryRun bool) (int, error) {
	ids, err := e.idsWithStatus(ctx, "superseded")
	if err != nil {
		return 0, err
	}
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.deleteMemoryRows(ctx, c, ids)
	}); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// candidate is an in-memory snapshot of an active row used for budgeting/eviction.
type candidate struct {
	id            string
	kind          Kind
	tokens        int
	lastUsedEpoch int64
	reinforcement int
	confidence    float64
}

// loadActive scans every active row once, returning each as a candidate and the
// total live-set token count. Token cost mirrors recall's tokensOf exactly
// (EstimateTokens(title + " " + body)).
func (e *Engine) loadActive(ctx context.Context) (cands []candidate, liveTokens int, err error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id, kind, title, body, last_used_epoch, reinforcement_count, confidence
		   FROM memories WHERE status='active'`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var c candidate
		var kind, title, body string
		if err := rows.Scan(&c.id, &kind, &title, &body, &c.lastUsedEpoch,
			&c.reinforcement, &c.confidence); err != nil {
			return nil, 0, err
		}
		c.kind = Kind(kind)
		c.tokens = ledger.EstimateTokens(title + " " + body)
		liveTokens += c.tokens
		cands = append(cands, c)
	}
	return cands, liveTokens, rows.Err()
}

// LiveTokens is the estimated token size of the active memory set.
func (e *Engine) LiveTokens(ctx context.Context) (int, error) {
	_, n, err := e.loadActive(ctx)
	return n, err
}

// archiveRows flips ids to status='archived', stamps archived_epoch, and drops
// their FTS entries (so they stop being lexical candidates).
func (e *Engine) archiveRows(ctx context.Context, c store.Conn, ids []string, epoch, now int64) error {
	for _, id := range ids {
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET status='archived', archived_epoch=?, updated_at=?
			  WHERE id=? AND status='active'`, epoch, now, id); err != nil {
			return err
		}
		if err := e.lex.Delete(ctx, c, id); err != nil {
			return err
		}
	}
	return nil
}

// evictToBudget archives the lowest-relevance evictable rows until the live set
// is within budget. If already within budget it archives nothing (the quiet-repo
// invariant): eviction is budget-triggered, never age-triggered. Returns the
// number archived and live tokens before/after.
func (e *Engine) evictToBudget(ctx context.Context, epoch int64, budget int, dryRun bool) (archived, before, after int, err error) {
	cands, live, err := e.loadActive(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	before, after = live, live
	if live <= budget {
		return 0, before, after, nil // under budget: never evict
	}

	// Eviction-eligible candidates, ranked by relevance ascending (worst first).
	var elig []candidate
	for _, c := range cands {
		if evictable(c.kind) {
			elig = append(elig, c)
		}
	}
	sort.Slice(elig, func(i, j int) bool {
		ri := relevance(elig[i].kind, float64(epoch-elig[i].lastUsedEpoch), elig[i].reinforcement, elig[i].confidence)
		rj := relevance(elig[j].kind, float64(epoch-elig[j].lastUsedEpoch), elig[j].reinforcement, elig[j].confidence)
		return ri < rj
	})

	var toArchive []string
	for _, c := range elig {
		if after <= budget {
			break
		}
		toArchive = append(toArchive, c.id)
		after -= c.tokens
	}
	if dryRun || len(toArchive) == 0 {
		return len(toArchive), before, after, nil
	}
	now := e.clk.Now().UnixMilli()
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.archiveRows(ctx, c, toArchive, epoch, now)
	}); err != nil {
		return 0, before, before, err
	}
	return len(toArchive), before, after, nil
}

// bucket identifies a MinHash collision bucket.
type bucket struct {
	band int
	hash int64
}

// parseCursor decodes the "band,hash" dedup cursor; the zero value (-1, min)
// selects every bucket as "after the cursor".
func parseCursor(s string) bucket {
	def := bucket{band: -1, hash: -1 << 62}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return def
	}
	bd, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return def // corrupt cursor: restart from the beginning rather than mid-band
	}
	return bucket{band: bd, hash: h}
}

func formatCursor(b bucket) string {
	return strconv.Itoa(b.band) + "," + strconv.FormatInt(b.hash, 10)
}

// dedupMember is an active row sharing a collision bucket.
type dedupMember struct {
	id            string
	body          string
	reinforcement int
	confidence    float64
	createdAt     int64
}

// dedupSweep walks up to maxBuckets MinHash collision buckets (resuming from the
// stored cursor), and within each merges same-kind active near-duplicates
// (Jaccard >= dedupThreshold) into the highest-reinforced canonical, marking the
// rest superseded. Returns the number of rows superseded by merging.
func (e *Engine) dedupSweep(ctx context.Context, dryRun bool, maxBuckets int) (merged int, err error) {
	cursorStr, err := getMetaStr(ctx, e.st.DB, metaDedupCursor, "")
	if err != nil {
		return 0, err
	}
	cur := parseCursor(cursorStr)

	// Gather candidate buckets via the pooled DB (read-only) so the write mutex
	// is not held across the GROUP BY scan.
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT mh.band, mh.hash FROM memory_minhash mh
		   JOIN memories m ON m.id = mh.memory_id
		  WHERE m.status = 'active' AND (mh.band, mh.hash) > (?, ?)
		  GROUP BY mh.band, mh.hash HAVING COUNT(*) > 1
		  ORDER BY mh.band, mh.hash LIMIT ?`, cur.band, cur.hash, maxBuckets)
	if err != nil {
		return 0, err
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.band, &b.hash); err != nil {
			rows.Close()
			return 0, err
		}
		buckets = append(buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := e.clk.Now().UnixMilli()
	last := cur
	wrap := len(buckets) < maxBuckets // fewer than the budget => reached the end

	for _, bk := range buckets {
		n, err := e.mergeBucket(ctx, bk, now, dryRun)
		if err != nil {
			return merged, err
		}
		merged += n
		last = bk
	}

	if dryRun {
		return merged, nil
	}
	next := formatCursor(last)
	if wrap {
		next = "" // start over next pass
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaStr(ctx, c, metaDedupCursor, next)
	}); err != nil {
		return merged, err
	}
	return merged, nil
}

// mergeBucket merges same-kind active near-dups within one collision bucket.
func (e *Engine) mergeBucket(ctx context.Context, bk bucket, now int64, dryRun bool) (int, error) {
	// Members are grouped by kind: a bucket can hold rows of several kinds.
	byKind := map[Kind][]dedupMember{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT m.id, m.kind, m.body, m.reinforcement_count, m.confidence, m.created_at
		   FROM memory_minhash mh JOIN memories m ON m.id = mh.memory_id
		  WHERE mh.band=? AND mh.hash=? AND m.status='active'`, bk.band, bk.hash)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var mem dedupMember
		var kind string
		if err := rows.Scan(&mem.id, &kind, &mem.body, &mem.reinforcement, &mem.confidence, &mem.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		byKind[Kind(kind)] = append(byKind[Kind(kind)], mem)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	merged := 0
	for _, members := range byKind {
		if len(members) < 2 {
			continue
		}
		// Greedy clustering: each member joins the first canonical it matches.
		type cluster struct {
			canon dedupMember
			set   map[string]struct{}
			dups  []dedupMember
		}
		var clusters []*cluster
		for _, mem := range members {
			set := shingleSet(mem.body)
			joined := false
			for _, cl := range clusters {
				if jaccard(set, cl.set) >= dedupThreshold {
					// Keep the better-reinforced row as canonical.
					if betterCanonical(mem, cl.canon) {
						cl.dups = append(cl.dups, cl.canon)
						cl.canon = mem
						cl.set = set
					} else {
						cl.dups = append(cl.dups, mem)
					}
					joined = true
					break
				}
			}
			if !joined {
				clusters = append(clusters, &cluster{canon: mem, set: set})
			}
		}
		for _, cl := range clusters {
			if len(cl.dups) == 0 {
				continue
			}
			merged += len(cl.dups)
			if dryRun {
				continue
			}
			if err := e.applyMerge(ctx, cl.canon.id, cl.dups, now); err != nil {
				return merged, err
			}
		}
	}
	return merged, nil
}

// betterCanonical reports whether a should be canonical over b: more
// reinforcement, then higher confidence, then newer.
func betterCanonical(a, b dedupMember) bool {
	if a.reinforcement != b.reinforcement {
		return a.reinforcement > b.reinforcement
	}
	if a.confidence != b.confidence {
		return a.confidence > b.confidence
	}
	return a.createdAt > b.createdAt
}

// applyMerge supersedes dups into canon and bumps canon's reinforcement_count.
func (e *Engine) applyMerge(ctx context.Context, canonID string, dups []dedupMember, now int64) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, d := range dups {
			if _, err := c.ExecContext(ctx,
				`UPDATE memories SET status='superseded', superseded_by=?, updated_at=?
				  WHERE id=? AND status='active'`, canonID, now, d.id); err != nil {
				return err
			}
			if err := e.lex.Delete(ctx, c, d.id); err != nil {
				return err
			}
		}
		_, err := c.ExecContext(ctx,
			`UPDATE memories SET reinforcement_count = reinforcement_count + ?, updated_at=?
			  WHERE id=? AND status='active'`, len(dups), now, canonID)
		return err
	})
}

// gcArchived hard-deletes archived rows older than archiveGraceEpochs (measured
// in epochs against the supplied current epoch). Returns the count removed.
func (e *Engine) gcArchived(ctx context.Context, epoch int64, dryRun bool) (int, error) {
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT id FROM memories
		  WHERE status='archived' AND (? - archived_epoch) > ?`, epoch, archiveGraceEpochs)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	if err := e.st.WriteTx(ctx, func(c store.Conn) error {
		return e.deleteMemoryRows(ctx, c, ids)
	}); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// errCompactSkip is the sentinel used to roll back the gate transaction when a
// pass must not run (interval gate not elapsed, or another pass holds the lease).
var errCompactSkip = errors.New("memory: compaction skipped")

// CompactOptions configures a compaction pass.
type CompactOptions struct {
	Force        bool // skip the min-interval gate (still respects the lease)
	DryRun       bool // compute the report without writing; bypasses gate + lease (read-only)
	BudgetTokens int  // 0 -> DefaultBudgetTokens
}

// CompactReport summarizes a pass.
type CompactReport struct {
	Ran              bool
	Epoch            int64
	SupersededGCd    int
	Archived         int
	Deduped          int
	ArchiveGCd       int
	LiveTokensBefore int
	LiveTokensAfter  int
	Budget           int
}

// Compact runs one bounded compaction pass: superseded-GC, budget-triggered
// decay eviction, a bounded dedup sweep, and archive-GC. It is gated to once per
// minInterval (unless Force) and serialized across worktrees by a single-flight
// lease in meta. A skipped pass returns CompactReport{Ran:false}, nil.
func (e *Engine) Compact(ctx context.Context, opts CompactOptions) (CompactReport, error) {
	now := e.clk.Now().UnixMilli()
	budget := opts.BudgetTokens
	if budget <= 0 {
		budget = DefaultBudgetTokens
	}
	rep := CompactReport{Budget: budget}

	if opts.DryRun {
		epoch, err := e.Epoch(ctx)
		if err != nil {
			return rep, err
		}
		return e.runSteps(ctx, epoch, budget, true)
	}

	// Gate + lease + epoch advance, atomically. Rolling back on errCompactSkip
	// leaves the epoch and lease untouched.
	var epoch int64
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		last, err := getMetaInt(ctx, c, metaLastRunAt, 0)
		if err != nil {
			return err
		}
		if !opts.Force && now-last < minIntervalMs {
			return errCompactSkip
		}
		leaseUntil, err := getMetaInt(ctx, c, metaLease, 0)
		if err != nil {
			return err
		}
		if now < leaseUntil {
			return errCompactSkip // another pass is in flight
		}
		cur, err := getMetaInt(ctx, c, metaActivityEpoch, 1)
		if err != nil {
			return err
		}
		epoch = cur + 1
		if err := setMetaInt(ctx, c, metaActivityEpoch, epoch); err != nil {
			return err
		}
		if err := setMetaInt(ctx, c, metaLastRunAt, now); err != nil {
			return err
		}
		return setMetaInt(ctx, c, metaLease, now+leaseTTLMs)
	})
	if errors.Is(err, errCompactSkip) {
		return CompactReport{Ran: false}, nil
	}
	if err != nil {
		return rep, err
	}
	defer func() { _ = e.clearLease(ctx) }()

	rep, err = e.runSteps(ctx, epoch, budget, false)
	if err != nil {
		return rep, err
	}

	// Best-effort lifetime counters (OUTSIDE the step transactions).
	_ = e.led.Incr(ctx, "lifetime", "compactions", 1)
	_ = e.led.Incr(ctx, "lifetime", "memories_archived", int64(rep.Archived))
	_ = e.led.Incr(ctx, "lifetime", "memories_deduped", int64(rep.Deduped))
	_ = e.led.Incr(ctx, "lifetime", "memories_gc", int64(rep.SupersededGCd+rep.ArchiveGCd))
	return rep, nil
}

// runSteps executes the four pass steps in order and assembles the report.
func (e *Engine) runSteps(ctx context.Context, epoch int64, budget int, dryRun bool) (CompactReport, error) {
	rep := CompactReport{Ran: true, Epoch: epoch, Budget: budget}
	var err error
	if rep.SupersededGCd, err = e.gcSuperseded(ctx, dryRun); err != nil {
		return rep, err
	}
	if rep.Archived, rep.LiveTokensBefore, rep.LiveTokensAfter, err = e.evictToBudget(ctx, epoch, budget, dryRun); err != nil {
		return rep, err
	}
	if rep.Deduped, err = e.dedupSweep(ctx, dryRun, defaultDedupBuckets); err != nil {
		return rep, err
	}
	if rep.ArchiveGCd, err = e.gcArchived(ctx, epoch, dryRun); err != nil {
		return rep, err
	}
	return rep, nil
}

// clearLease releases the single-flight lease so the next session is not blocked
// for the full TTL after a fast pass. Best-effort.
func (e *Engine) clearLease(ctx context.Context) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		return setMetaInt(ctx, c, metaLease, 0)
	})
}

// stampRecalled bumps last_used_epoch to the current activity epoch and
// increments recall_count for the surfaced rows, in ONE batched UPDATE. It is
// the decay subsystem's "time since last used" signal. Best-effort: callers
// ignore the error so a stamping failure never fails an otherwise-good recall.
// It runs in its own WriteTx, OUTSIDE any enclosing transaction (the store's
// write mutex is non-reentrant), alongside Recall's existing ledger writes.
func (e *Engine) stampRecalled(ctx context.Context, results []RecallResult) error {
	if len(results) == 0 {
		return nil
	}
	epoch, err := e.Epoch(ctx)
	if err != nil {
		return err
	}
	ph := make([]string, 0, len(results))
	args := make([]any, 0, len(results)+1)
	args = append(args, epoch)
	for _, r := range results {
		ph = append(ph, "?")
		args = append(args, r.Record.ID)
	}
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`UPDATE memories SET last_used_epoch = ?, recall_count = recall_count + 1
			  WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		return err
	})
}
