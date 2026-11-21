package graph

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gerwim/recarto/internal/store"
)

// pruneBatchFiles is how many files each prune transaction tears down. Batched
// commits keep the write lock short and make the prune resumable: a worktree
// holds a full graph copy (easily 100k+ rows), and a single transaction would
// both stall readers for its whole duration and lose everything to one
// cancellation — the same trap the per-index rebuild fix closed.
const pruneBatchFiles = 256

// usedKeyPrefix prefixes the meta key holding when Recarto last used a worktree (unix ms).
const usedKeyPrefix = "worktree_used:"

// errWorktreeUsed stops a prune whose worktree was used again since the sweep chose it.
var errWorktreeUsed = errors.New("worktree used since the prune began")

// WorktreeUse is a worktree with indexed files and when Recarto last used it.
type WorktreeUse struct {
	Worktree string
	LastUsed time.Time
}

// Worktrees returns the distinct worktrees with indexed files in this store.
func (e *Engine) Worktrees(ctx context.Context) ([]string, error) {
	rows, err := e.st.DB.QueryContext(ctx, `SELECT DISTINCT worktree FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var wt string
		if err := rows.Scan(&wt); err != nil {
			return nil, err
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

// MarkUsed records that Recarto is working in worktree now (e.clk.Now()).
// The mark never moves backwards: a write landing late from a process whose
// clock read was earlier must not hide a newer use from the prune guard.
func (e *Engine) MarkUsed(ctx context.Context, worktree string) error {
	now := e.clk.Now()
	return e.markUsedBefore(ctx, worktree, now, now)
}

// useMarkInterval is how old a worktree's use mark gets before a hook or graph
// query writes a new one. An unused worktree's cutoff is at least a day, so a
// worktree in use never looks unused, and most hooks only read.
const useMarkInterval = time.Hour

// NoteUsed is MarkUsed for hooks and graph queries. It writes only when the
// worktree has no mark or its mark is more than useMarkInterval old, so most
// calls take no write lock. A sweep picks an unused worktree only when its mark
// is at least a day old, so a session that comes back during that prune writes
// the mark that stops it. A deleted worktree's prune stops when its directory
// comes back (PruneDeadWorktree).
func (e *Engine) NoteUsed(ctx context.Context, worktree string) error {
	now := e.clk.Now()
	cutoff := now.Add(-useMarkInterval)
	recent, err := usedSince(ctx, e.st.DB, worktree, cutoff)
	if err != nil || recent {
		return err
	}
	return e.markUsedBefore(ctx, worktree, now, cutoff)
}

// markUsedBefore writes now as worktree's use mark unless the stored mark is
// at or after cutoff. A write landing late from a process whose clock read was
// earlier then never hides a newer use, and a mark another process refreshed
// after the caller read it stays.
func (e *Engine) markUsedBefore(ctx context.Context, worktree string, now, cutoff time.Time) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value
			 WHERE CAST(meta.value AS INTEGER) < ?`,
			usedKeyPrefix+worktree, strconv.FormatInt(now.UnixMilli(), 10), cutoff.UnixMilli())
		return err
	})
}

// usedSince reports whether worktree's use mark is at or after t. It is the
// one rule both the prune guard and WorktreesUsedBefore (its negation) apply;
// NoteUsed applies it too.
func usedSince(ctx context.Context, c store.Conn, worktree string, t time.Time) (bool, error) {
	var v int64
	err := c.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM meta WHERE key=?`, usedKeyPrefix+worktree).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v >= t.UnixMilli(), nil
}

// WorktreesUsedBefore returns the worktrees with indexed files last used before
// cutoff, oldest first. A worktree never marked counts from its newest file index time.
func (e *Engine) WorktreesUsedBefore(ctx context.Context, cutoff time.Time) ([]WorktreeUse, error) {
	// last_used < cutoff is the negation of usedSince: a worktree used exactly
	// at the cutoff is kept, one used a millisecond earlier is selected.
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT worktree, last_used FROM (
		   SELECT w.worktree,
		          COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?1 || w.worktree),
		                   (SELECT MAX(last_indexed_at) FROM files WHERE worktree = w.worktree)) AS last_used
		   FROM (SELECT DISTINCT worktree FROM files) w)
		 WHERE last_used < ?2 ORDER BY last_used`,
		usedKeyPrefix, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	// rows.Err reports any read failure; closing a read-only cursor has nothing to lose.
	defer func() { _ = rows.Close() }()
	var out []WorktreeUse
	for rows.Next() {
		var u WorktreeUse
		var ms int64
		if err := rows.Scan(&u.Worktree, &ms); err != nil {
			return nil, err
		}
		u.LastUsed = time.UnixMilli(ms)
		out = append(out, u)
	}
	return out, rows.Err()
}

// PruneWorktree removes worktree's graph from the store. It stops, reporting
// false, once Recarto marks the worktree used at or after usedBefore.
//
// The prune is a series of self-contained transactions (pruneStep), so it stays
// batched and safe to interrupt: every committed step leaves a worktree that
// re-indexes cleanly, and one with files left is still listed, so the next
// sweep resumes it. Memories, sessions and every non-graph table stay.
func (e *Engine) PruneWorktree(ctx context.Context, worktree string, usedBefore time.Time) (bool, error) {
	return e.prune(ctx, worktree, usedBefore, false)
}

// PruneDeadWorktree is PruneWorktree for a worktree whose directory was gone
// when the sweep chose it. It also stops, reporting false, once the directory
// exists again: a session there may hold a mark too recent for it to rewrite.
func (e *Engine) PruneDeadWorktree(ctx context.Context, worktree string, usedBefore time.Time) (bool, error) {
	return e.prune(ctx, worktree, usedBefore, true)
}

func (e *Engine) prune(ctx context.Context, worktree string, usedBefore time.Time, dead bool) (bool, error) {
	for {
		done, err := e.pruneStep(ctx, worktree, usedBefore, pruneBatchFiles, dead)
		if errors.Is(err, errWorktreeUsed) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
}

// pruneStep runs one prune transaction and reports done once worktree has no files left.
//
// It first checks nobody came back to the worktree, so a returning session's
// mark stops the prune before it writes anything. It then drops the freshness
// token, cached code map and change journal before any file: a prune cut short
// leaves a worktree that re-indexes instead of one that looks fresh, and whose
// re-created files journal nothing. The last batch drops the remaining meta in
// the same transaction, so no committed state has the files gone and the meta
// pending.
func (e *Engine) pruneStep(ctx context.Context, worktree string, usedBefore time.Time, batch int, dead bool) (bool, error) {
	done := false
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		used, err := usedSince(ctx, c, worktree, usedBefore)
		if err != nil {
			return err
		}
		if used {
			return errWorktreeUsed
		}
		if dead {
			// Checked under the batch's write lock: a directory that comes back while
			// the batch waits for the lock stops the prune before it deletes anything.
			// Anything but a missing directory keeps the graph, the same test
			// PrunableWorktrees applies when it chooses dead worktrees.
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				return errWorktreeUsed
			}
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?, ?)`,
			graphTokenKey+worktree, archCacheKey+worktree, builtKey(worktree)); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM graph_deltas WHERE worktree=?`, worktree); err != nil {
			return err
		}
		ids, err := fileIDBatch(ctx, c, worktree, batch)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			args := make([]any, len(ids))
			ph := make([]string, len(ids))
			for i, id := range ids {
				args[i], ph[i] = id, "?"
			}
			in := strings.Join(ph, ",")
			for _, fts := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
				if _, err := c.ExecContext(ctx, `DELETE FROM `+fts+` WHERE rowid IN (
					SELECT fts_rowid FROM node_fts_rows
					WHERE node_id IN (SELECT id FROM nodes WHERE file_id IN (`+in+`)))`, args...); err != nil {
					return err
				}
			}
			if _, err := c.ExecContext(ctx, `DELETE FROM files WHERE id IN (`+in+`)`, args...); err != nil {
				return err
			}
		}
		var left bool
		if err := c.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM files WHERE worktree=?)`, worktree).Scan(&left); err != nil {
			return err
		}
		if left {
			return nil
		}
		if _, err := c.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?)`,
			resolvePendingKey(worktree), usedKeyPrefix+worktree); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// fileIDBatch returns up to batch file ids of worktree.
func fileIDBatch(ctx context.Context, c store.Conn, worktree string, batch int) ([]string, error) {
	rows, err := c.QueryContext(ctx,
		`SELECT id FROM files WHERE worktree=? LIMIT ?`, worktree, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
