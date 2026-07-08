package remotesync

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// metaCursor is the meta key holding the last-applied server seq (the pull
// cursor). It is the ONLY sync-cursor mechanism — never a timestamp — because
// updated_at is set by whichever machine authored a row and is therefore
// clock-skew-sensitive; seq is assigned solely by the server and is
// monotonic.
const metaCursor = "sync_cursor"

// pullPageLimit bounds each pull page so a huge bootstrap (a fresh clone, or a
// fresh ForgeBoard microVM with cursor 0) makes bounded incremental progress
// instead of one unbounded response.
const pullPageLimit = 500

// pushBatchSize bounds each push request body.
const pushBatchSize = 200

// Syncer orchestrates pull-then-push for one repo against one server.
type Syncer struct {
	repoKey  string
	client   *Client
	eng      *memory.Engine
	st       *store.Store
	interval time.Duration
}

// New returns a Syncer for gc, or ok=false when sync is not configured (no
// URL/key) or the repo has no resolvable cross-machine identity (RepoKey).
// Either case means "stay local-only" — never an error.
func New(cfg Config, gc gitctx.Context, eng *memory.Engine, st *store.Store) (*Syncer, bool) {
	if !cfg.Enabled() {
		return nil, false
	}
	key, ok := RepoKey(gc)
	if !ok {
		return nil, false
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Syncer{
		repoKey:  key,
		eng:      eng,
		st:       st,
		client:   &Client{BaseURL: cfg.URL, Key: cfg.Key, HTTP: &http.Client{Timeout: timeout}},
		interval: interval,
	}, true
}

// Interval returns the configured poll period (RECARTO_SYNC_INTERVAL;
// default 60s) for a long-lived process's periodic sync loop (`recarto
// serve`'s background ticker).
func (s *Syncer) Interval() time.Duration { return s.interval }

// Pull applies remote changes since the local cursor into the local store,
// paging until the server reports no more, advancing the cursor after each
// applied page so an interrupted pull resumes rather than restarting.
func (s *Syncer) Pull(ctx context.Context) error {
	cursor, err := s.getCursor(ctx)
	if err != nil {
		return err
	}
	for {
		resp, err := s.client.Pull(ctx, s.repoKey, cursor, pullPageLimit)
		if err != nil {
			return err
		}
		for _, rec := range resp.Records {
			if _, _, err := s.eng.SyncApply(ctx, rec); err != nil {
				return err
			}
		}
		cursor = resp.Cursor
		if err := s.setCursor(ctx, cursor); err != nil {
			return err
		}
		if !resp.More {
			return nil
		}
	}
}

// pushItem is one queued outgoing change, carrying both its wire record and
// the outbox/tombstone version it was read at — see memory.IDVersion for why
// the version must travel with it (a race guard against a write landing on
// the same id while this exact item is still in flight over HTTP).
type pushItem struct {
	rec     syncwire.Record
	version int64
	tomb    bool
}

// Push sends every locally-authored change (the outbox) and pending tombstone
// to the server, in bounded batches, clearing each batch's local queue entries
// only after the server confirms it — and only up to the version actually
// sent, so a write that races an in-flight push is never silently dropped
// (see memory.IDVersion).
func (s *Syncer) Push(ctx context.Context) error {
	recs, err := s.eng.PendingPush(ctx)
	if err != nil {
		return err
	}
	tombs, err := s.eng.PendingTombstones(ctx)
	if err != nil {
		return err
	}
	all := make([]pushItem, 0, len(recs)+len(tombs))
	for _, r := range recs {
		all = append(all, pushItem{rec: r.Record, version: r.Version})
	}
	for _, t := range tombs {
		// PendingTombstones' UpdatedAt IS the tombstone's version (deleted_at).
		all = append(all, pushItem{rec: t, version: t.UpdatedAt, tomb: true})
	}
	for start := 0; start < len(all); start += pushBatchSize {
		end := min(start+pushBatchSize, len(all))
		batch := all[start:end]
		wire := make([]syncwire.Record, len(batch))
		for i, it := range batch {
			wire[i] = it.rec
		}
		if _, err := s.client.Push(ctx, s.repoKey, wire); err != nil {
			return err
		}
		var pushed, tombPushed []memory.IDVersion
		for _, it := range batch {
			if it.tomb {
				tombPushed = append(tombPushed, memory.IDVersion{ID: it.rec.ID, Version: it.version})
			} else {
				pushed = append(pushed, memory.IDVersion{ID: it.rec.ID, Version: it.version})
			}
		}
		if err := s.eng.ClearPushed(ctx, pushed); err != nil {
			return err
		}
		if err := s.eng.MarkTombstonesPushed(ctx, tombPushed); err != nil {
			return err
		}
	}
	return nil
}

// Once runs a bounded, best-effort pull then push. Callers (session-start /
// Stop hooks, the `serve` background ticker) treat any returned error as
// non-blocking: sync is additive, never load-bearing for local operation.
func (s *Syncer) Once(ctx context.Context) error {
	if err := s.Pull(ctx); err != nil {
		return err
	}
	return s.Push(ctx)
}

func (s *Syncer) getCursor(ctx context.Context) (int64, error) {
	var v string
	err := s.st.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaCursor).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n, nil
}

func (s *Syncer) setCursor(ctx context.Context, cursor int64) error {
	return s.st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			metaCursor, strconv.FormatInt(cursor, 10))
		return err
	})
}

// ResetCursor rewinds the local pull cursor and outbox-independent watermark to
// zero, forcing a full re-pull on the next Pull (used by `recarto sync --full`
// to reconcile from scratch). It does not touch the outbox or tombstones — a
// local push queue always reflects real pending local authorship regardless.
func (s *Syncer) ResetCursor(ctx context.Context) error {
	return s.setCursor(ctx, 0)
}

// RepoKeyValue returns the resolved repo key this Syncer talks to the server
// with (for diagnostics: `recarto doctor`/`status`).
func (s *Syncer) RepoKeyValue() string { return s.repoKey }

// Ping checks server reachability (for `recarto doctor`).
func (s *Syncer) Ping(ctx context.Context) error { return s.client.Healthz(ctx) }
