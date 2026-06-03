package memory

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Compaction tuning. These are policy defaults (not empirically optimized) and
// are overridable per call (BudgetTokens) or, for the rest, by editing here.
const (
	// DefaultBudgetTokens is the live-set token budget. Deliberately generous:
	// normal/quiet repos stay well under it and never evict — eviction is a
	// safety valve against runaway growth, not routine behavior.
	DefaultBudgetTokens = 250_000

	halfLifeActionEpochs = 5.0  // disposable activity log: fastest decay
	halfLifeStdEpochs    = 30.0 // preference / learning
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
// (they can still be deduped). Only action/preference/learning decay.
func evictable(k Kind) bool {
	switch k {
	case KindAction, KindPreference, KindLearning:
		return true
	}
	return false
}

// halfLifeEpochs is the decay half-life (in activity epochs) for a kind.
func halfLifeEpochs(k Kind) float64 {
	if k == KindAction {
		return halfLifeActionEpochs
	}
	return halfLifeStdEpochs // preference / learning
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
