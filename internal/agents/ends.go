package agents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/store"
)

// endRecord stores F (the conversation end), L (the one warm launch claim), R
// (a lock-free request with no launch right), D (repo-end discovery), or M (a
// repo's chosen end time). D carries its time so interrupted publication is
// still recoverable.
type endRecord struct {
	Session  string   `json:"session"`
	Folder   string   `json:"folder,omitempty"`
	EndedAt  int64    `json:"ended_at,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	Worktree string   `json:"worktree,omitempty"`
}

func folderEndsDir(st *store.Store) string {
	return filepath.Join(filepath.Dir(filepath.Dir(st.Path())), "pending-ends")
}
func repoEndsDir(st *store.Store) string {
	return filepath.Join(filepath.Dir(st.Path()), "pending-repo-ends")
}
func folderID(st *store.Store) string { return filepath.Base(filepath.Dir(st.Path())) }

func endName(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(h[:])
}

func folderEndName(folder, sessionID string) string {
	return endName(folder + "\x00" + sessionID)
}

func requestEndName(folder, sessionID string) string {
	return folderEndName(folder, sessionID) + "-request"
}

func launchEndName(folder, sessionID string) string {
	return folderEndName(folder, sessionID) + "-launch"
}

// endClaimsLockPath serializes publication and removal of F and L in a folder,
// so each claim decision reads the current folder row and records.
func endClaimsLockPath(st *store.Store) string {
	return filepath.Join(filepath.Dir(st.Path()), "end-claims.lock")
}

func entryName(folder, sessionID, worktree string) string {
	h := sha256.Sum256([]byte(worktree))
	return folderEndName(folder, sessionID) + "-" + hex.EncodeToString(h[:])
}

func coveredWorktrees(covered []coveredRepo) []string {
	worktrees := make([]string, 0, len(covered))
	for _, c := range covered {
		worktrees = append(worktrees, c.Git.Worktree)
	}
	return worktrees
}

// lockFiles takes locks in the given order, all or none, waiting until ctx ends
// or not at all when try is true.
func lockFiles(ctx context.Context, paths []string, try bool) (release func(), ok bool, err error) {
	var releases []func()
	unlock := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, path := range paths {
		var release func()
		var got bool
		if try || ctx.Err() != nil {
			// A deadline that has passed gets one try: LockFile would leave a
			// waiter that can briefly hold the lock after its holder releases it.
			release, got, err = store.TryLockFile(path)
			if !got && err == nil && !try {
				err = ctx.Err()
			}
		} else {
			release, err = store.LockFile(ctx, path, nil)
			got = err == nil
		}
		if err != nil || !got {
			unlock()
			return nil, false, err
		}
		releases = append(releases, release)
	}
	return unlock, true, nil
}

// acquireEndLock takes all given repos' coordination locks in lock-path order,
// all or none, waiting until ctx ends or not at all when try is true. It is a
// var so tests can step lock acquisition.
var acquireEndLock = func(ctx context.Context, covered []coveredRepo, try bool) (release func(), ok bool, err error) {
	paths := make([]string, 0, len(covered))
	for _, c := range covered {
		paths = append(paths, graph.PendingEndsLockPath(c.Store))
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	return lockFiles(ctx, paths, try)
}

// claimEnd publishes rec as dir/name unless it exists, and returns the record
// that holds and whether this call published it: of concurrent callers,
// exactly one does.
func claimEnd(dir, name string, rec endRecord) (endRecord, bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return endRecord{}, false, err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*")
	if err != nil {
		return endRecord{}, false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	data, err := json.Marshal(rec)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return endRecord{}, false, err
	}
	if err := os.Link(tmp.Name(), filepath.Join(dir, name)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			old, _, readErr := readEnd(dir, name)
			return old, false, readErr
		}
		return endRecord{}, false, err
	}
	return rec, true, nil
}

func readEnd(dir, name string) (endRecord, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return endRecord{}, false, nil
	}
	if err != nil {
		return endRecord{}, false, err
	}
	var rec endRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return endRecord{}, false, err
	}
	return rec, true, nil
}

func listEnds(dir string) ([]endRecord, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []endRecord
	var errs []error
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		rec, ok, err := readEnd(dir, entry.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("read pending end %s: %w", entry.Name(), err))
			continue
		}
		if ok {
			records = append(records, rec)
		}
	}
	return records, errors.Join(errs...)
}

func removeEnd(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// conversationEnd is what endConversation did.
type conversationEnd struct {
	at      int64
	first   bool // this call published the conversation's launch claim L
	pending bool
}

type repoEnd struct {
	coveredRepo
	at int64
}

type endChoice struct {
	f       endRecord
	fok     bool
	ended   sql.NullInt64
	at      int64
	first   bool
	pending bool
	written []repoEnd
	errs    []error
}

type endClaim struct {
	locked bool
	ended  sql.NullInt64
	f      endRecord
	fok    bool
	first  bool
	now    int64
}

func removeMissingFolderEnds(dir, fid, sessionID, fname, lname, rname string) []error {
	errs := []error{removeEnd(dir, fname), removeEnd(dir, lname), removeEnd(dir, rname)}
	entries, err := listEnds(dir)
	if err != nil {
		errs = append(errs, err)
	}
	for _, rec := range entries {
		if rec.Folder == fid && rec.Session == sessionID && rec.Worktree != "" {
			errs = append(errs, removeEnd(dir, entryName(fid, sessionID, rec.Worktree)))
		}
	}
	return errs
}

func writeFolderEnd(ctx context.Context, a *app.App, sessionID string, at int64) (int64, bool, error) {
	endedAt := at
	err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		if _, err := c.ExecContext(ctx,
			`UPDATE sessions SET ended_at=? WHERE id=? AND ended_at IS NULL`, at, sessionID); err != nil {
			return err
		}
		return c.QueryRowContext(ctx, `SELECT ended_at FROM sessions WHERE id=?`, sessionID).Scan(&endedAt)
	})
	return endedAt, store.IsBusy(err), err
}

// classifyRepoEnds sorts covered by the time recorded for sessionID in each
// repo: known has M, then D, then F times; unset has no time. Ended rows have
// stale M and D records removed and appear in neither list.
func classifyRepoEnds(ctx context.Context, sessionID string, covered []coveredRepo, fdir, fid string, f endRecord, fok bool) (known []repoEnd, unset []coveredRepo, errs []error) {
	for _, c := range covered {
		wt := c.Git.Worktree
		rdir := repoEndsDir(c.Store)
		var ended bool
		if err := c.Store.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, sessionID).Scan(&ended); err != nil {
			errs = append(errs, err)
			continue
		}
		if ended {
			errs = append(errs, removeEnd(rdir, endName(sessionID)), removeEnd(fdir, entryName(fid, sessionID, wt)))
			continue
		}
		marker, mok, err := readEnd(rdir, endName(sessionID))
		if err != nil {
			errs = append(errs, err)
			mok = false
		}
		if mok && marker.EndedAt != 0 {
			known = append(known, repoEnd{coveredRepo: c, at: marker.EndedAt})
			continue
		}
		discovery, dok, err := readEnd(fdir, entryName(fid, sessionID, wt))
		if err != nil {
			errs = append(errs, err)
			dok = false
		}
		if dok && discovery.EndedAt != 0 {
			known = append(known, repoEnd{coveredRepo: c, at: discovery.EndedAt})
			continue
		}
		if fok && slices.Contains(f.Repos, wt) {
			known = append(known, repoEnd{coveredRepo: c, at: f.EndedAt})
			continue
		}
		unset = append(unset, c)
	}
	return known, unset, errs
}

// publishRepoEnds publishes each repo's timestamped discovery entry and marker.
// An already-published timestamp remains authoritative.
func publishRepoEnds(sessionID, fdir, fid string, ends []repoEnd) ([]repoEnd, []error) {
	var published []repoEnd
	var errs []error
	for _, r := range ends {
		wt := r.Git.Worktree
		discovery, _, err := claimEnd(fdir, entryName(fid, sessionID, wt), endRecord{
			Session: sessionID, Folder: fid, Worktree: wt, EndedAt: r.at,
		})
		if err != nil {
			errs = append(errs, err)
		}
		if discovery.EndedAt != 0 {
			r.at = discovery.EndedAt
		}
		marker, _, err := claimEnd(repoEndsDir(r.Store), endName(sessionID), endRecord{Session: sessionID, Folder: fid, EndedAt: r.at})
		if err != nil {
			errs = append(errs, err)
		}
		if marker.EndedAt != 0 {
			r.at = marker.EndedAt
		}
		published = append(published, r)
	}
	return published, errs
}

// claimConversationEnd publishes L for the first Stop and F once a writer with
// the required repo locks has a time to record. The claims lock makes each
// decision use current row/F/L/R state and prevents cleanup from reopening a
// launch claim after the row has ended.
func claimConversationEnd(ctx, lockCtx context.Context, a *app.App, sessionID, fdir, fid string, covered []coveredRepo, stop, choose, tryLock bool) (endClaim, []error) {
	release, ok, err := lockFiles(lockCtx, []string{endClaimsLockPath(a.Store)}, tryLock)
	if err != nil || !ok {
		if err != nil {
			return endClaim{}, []error{err}
		}
		return endClaim{}, nil
	}
	defer release()

	f, fok, err := readEnd(fdir, folderEndName(fid, sessionID))
	if err != nil {
		return endClaim{}, []error{err}
	}
	row, found, err := folderSession(ctx, a.Store, sessionID)
	if err != nil {
		return endClaim{}, []error{err}
	}
	c := endClaim{locked: true, ended: row.ended, f: f, fok: fok}
	if !found || row.ended.Valid || fok {
		return c, nil
	}
	_, lok, err := readEnd(fdir, launchEndName(fid, sessionID))
	if err != nil {
		return endClaim{}, []error{err}
	}
	_, rok, err := readEnd(fdir, requestEndName(fid, sessionID))
	if err != nil {
		return endClaim{}, []error{err}
	}
	if stop && !lok {
		_, c.first, err = claimEnd(fdir, launchEndName(fid, sessionID), endRecord{
			Session: sessionID, Folder: fid, Repos: coveredWorktrees(covered),
		})
		if err != nil {
			return endClaim{}, []error{err}
		}
		lok = c.first
	}
	if !choose || (!lok && !rok) {
		return c, nil
	}
	c.now = a.Clock.Now().UnixMilli()
	c.f, _, err = claimEnd(fdir, folderEndName(fid, sessionID), endRecord{
		Session: sessionID, Folder: fid, EndedAt: c.now, Repos: coveredWorktrees(covered),
	})
	if err != nil {
		return c, []error{err}
	}
	c.fok = true
	return c, nil
}

func chooseEndTimes(ctx, lockCtx context.Context, a *app.App, sessionID, fdir, fid string, covered, unset []coveredRepo, f endRecord, fok, claimF, stop, tryLock bool) endChoice {
	choice := endChoice{f: f, fok: fok}
	lockSet := []coveredRepo(nil)
	if claimF || stop {
		lockSet = unset
	}
	if !claimF && len(lockSet) == 0 {
		return choice
	}

	var release func()
	locked := false
	defer func() {
		if locked {
			release()
		}
	}()
	ok := true
	var err error
	if len(lockSet) > 0 {
		release, ok, err = acquireEndLock(lockCtx, lockSet, tryLock)
		if err != nil {
			choice.errs = append(choice.errs, err)
		}
		locked = ok && err == nil
	}
	held := ok && err == nil
	var own int64
	if claimF && (held || stop) {
		c, errs := claimConversationEnd(ctx, lockCtx, a, sessionID, fdir, fid, covered, stop, held, tryLock)
		choice.errs = append(choice.errs, errs...)
		if !c.locked {
			choice.pending = true
			if stop {
				_, _, err := claimEnd(fdir, requestEndName(fid, sessionID), endRecord{Session: sessionID, Folder: fid})
				if err != nil {
					choice.errs = append(choice.errs, err)
				}
			}
			return choice
		}
		choice.first, choice.ended = c.first, c.ended
		if c.fok {
			choice.f, choice.fok = c.f, true
		} else if c.now != 0 {
			choice.at = c.now
		}
		own = c.now
	}
	if !held {
		choice.pending = true
		return choice
	}
	if own == 0 && stop {
		own = a.Clock.Now().UnixMilli()
	}
	known, fresh, errs := classifyRepoEnds(ctx, sessionID, lockSet, fdir, fid, choice.f, choice.fok)
	choice.errs = append(choice.errs, errs...)
	ends := known
	if own != 0 {
		for _, c := range fresh {
			ends = append(ends, repoEnd{coveredRepo: c, at: own})
		}
	}
	choice.written, errs = publishRepoEnds(sessionID, fdir, fid, ends)
	choice.errs = append(choice.errs, errs...)
	if locked {
		release()
		locked = false
	}
	return choice
}

func writeRepoEnds(ctx context.Context, sessionID string, row folderRow, pending []repoEnd, dir, fid string) (bool, []error) {
	var busy bool
	var errs []error
	for _, r := range pending {
		err := r.Store.WriteTx(ctx, func(c store.Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET ended_at = excluded.ended_at WHERE sessions.ended_at IS NULL`,
				sessionID, row.agent, nullable(r.Git.Branch), r.Git.Worktree, row.startedAt, r.at)
			return err
		})
		switch {
		case err == nil:
			errs = append(errs, removeEnd(repoEndsDir(r.Store), endName(sessionID)), removeEnd(dir, entryName(fid, sessionID, r.Git.Worktree)))
		case store.IsBusy(err):
			busy = true
		default:
			errs = append(errs, fmt.Errorf("end the session in %s: %w", r.Git.Worktree, err))
		}
	}
	return busy, errs
}

func removeSettledClaims(ctx, lockCtx context.Context, a *app.App, sessionID string, covered []coveredRepo, fdir, fname, lname, rname string, tryLock bool) []error {
	_, fok, ferr := readEnd(fdir, fname)
	_, lok, lerr := readEnd(fdir, lname)
	_, rok, rerr := readEnd(fdir, rname)
	if !fok && !lok && !rok && ferr == nil && lerr == nil && rerr == nil {
		return nil
	}
	release, ok, err := lockFiles(lockCtx, []string{endClaimsLockPath(a.Store)}, tryLock)
	if err != nil || !ok {
		if (!ok && err == nil) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The end writes already determined pending status; cleanup can wait for the next hook.
			return nil
		}
		return []error{err}
	}
	defer release()

	folder, found, err := folderSession(ctx, a.Store, sessionID)
	if err != nil {
		return []error{err}
	}
	if !found || !folder.ended.Valid {
		return nil
	}
	f, fok, err := readEnd(fdir, fname)
	if err != nil {
		return []error{err}
	}
	errs := []error{removeEnd(fdir, lname), removeEnd(fdir, rname)}
	if !fok {
		return errs
	}
	for _, wt := range f.Repos {
		i := slices.IndexFunc(covered, func(c coveredRepo) bool { return c.Git.Worktree == wt })
		if i < 0 {
			return errs
		}
		var ended bool
		if err := covered[i].Store.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, sessionID).Scan(&ended); err != nil {
			return append(errs, err)
		}
		if !ended {
			return errs
		}
	}
	errs = append(errs, removeEnd(fdir, fname))
	return errs
}

// endConversation writes sessionID's folder and repo session ends, inserting
// missing repo rows. Recorded M, D and F times replay without coordination
// locks. New repo times are chosen after the affected repo locks are held and
// published before release. F and L decisions use the claims lock; store writes
// use ctx and lock waits use lockCtx, or do not wait when tryLock is true.
func endConversation(ctx, lockCtx context.Context, a *app.App, sessionID string, covered []coveredRepo, stop, tryLock bool) (conversationEnd, error) {
	var e conversationEnd
	var errs []error
	fdir := folderEndsDir(a.Store)
	fid := folderID(a.Store)
	fname := folderEndName(fid, sessionID)
	lname := launchEndName(fid, sessionID)
	rname := requestEndName(fid, sessionID)
	// Records are removed only after the row is ended, so an open row read after
	// them means no end had been settled yet.
	f, fok, ferr := readEnd(fdir, fname)
	if ferr != nil {
		errs = append(errs, ferr)
		fok = false
	}
	_, lok, lerr := readEnd(fdir, lname)
	if lerr != nil {
		errs = append(errs, lerr)
		lok = false
	}
	_, rok, rerr := readEnd(fdir, rname)
	if rerr != nil {
		errs = append(errs, rerr)
		rok = false
	}
	row, found, err := folderSession(ctx, a.Store, sessionID)
	if err != nil {
		return e, err
	}
	if !found {
		release, ok, lockErr := lockFiles(lockCtx, []string{endClaimsLockPath(a.Store)}, tryLock)
		if lockErr != nil {
			errs = append(errs, lockErr)
		}
		if ok && lockErr == nil {
			errs = append(errs, removeMissingFolderEnds(fdir, fid, sessionID, fname, lname, rname)...)
			release()
		}
		return e, errors.Join(errs...)
	}
	claimF := !row.ended.Valid && !fok && (stop || lok || rok)
	if !row.ended.Valid && !fok && !claimF {
		return e, errors.Join(errs...)
	}

	known, unset, classifyErrs := classifyRepoEnds(ctx, sessionID, covered, fdir, fid, f, fok)
	errs = append(errs, classifyErrs...)
	written, publishErrs := publishRepoEnds(sessionID, fdir, fid, known)
	errs = append(errs, publishErrs...)
	choice := chooseEndTimes(ctx, lockCtx, a, sessionID, fdir, fid, covered, unset, f, fok, claimF, stop, tryLock)
	f, fok = choice.f, choice.fok
	e.at, e.first, e.pending = choice.at, choice.first, choice.pending
	written = append(written, choice.written...)
	errs = append(errs, choice.errs...)
	if choice.ended.Valid {
		row.ended = choice.ended
	}

	if row.ended.Valid {
		e.at = row.ended.Int64
	} else if fok {
		e.at = f.EndedAt
	}
	if !row.ended.Valid {
		if e.at != 0 {
			var busy bool
			e.at, busy, err = writeFolderEnd(ctx, a, sessionID, e.at)
			if busy {
				e.pending = true
			} else if err != nil {
				errs = append(errs, err)
			}
		}
	}

	repoBusy, repoErrs := writeRepoEnds(ctx, sessionID, row, written, fdir, fid)
	e.pending = e.pending || repoBusy
	errs = append(errs, repoErrs...)

	cleanupErrs := removeSettledClaims(ctx, lockCtx, a, sessionID, covered, fdir, fname, lname, rname, tryLock)
	errs = append(errs, cleanupErrs...)
	return e, errors.Join(errs...)
}

// endPendingConversations tries once, without waiting for another process's
// writer, the session ends that Stops in a's folder left pending.
func endPendingConversations(ctx context.Context, a *app.App) error {
	entries, err := listEnds(folderEndsDir(a.Store))
	if err != nil {
		return err
	}
	fid := folderID(a.Store)
	var sessions []string
	for _, rec := range entries {
		if rec.Folder == fid && rec.Session != "" && !slices.Contains(sessions, rec.Session) {
			sessions = append(sessions, rec.Session)
		}
	}
	w := store.WithWriteDeadline(ctx, a.Clock.Now())
	var errs []error
	for _, session := range sessions {
		roster, err := readRoster(ctx, a.Ledger, session)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		covered, _, done := cover(a, roster)
		_, err = endConversation(w, ctx, a, session, covered, false, true)
		done()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EndRosterSessions writes the session ends a Stop of conversation sessionID
// left pending, trying a busy store again until none is left or ctx ends.
func EndRosterSessions(ctx context.Context, a *app.App, sessionID string) error {
	a, closeConv, err := conversationApp(ctx, a, sessionID)
	if err != nil {
		return err
	}
	defer closeConv()
	roster, err := readRoster(ctx, a.Ledger, sessionID)
	if err != nil || len(roster) == 0 {
		return err
	}
	covered, _, done := cover(a, roster)
	defer done()
	var last error
	for {
		deadline := a.Clock.Now().Add(writeGrace)
		lockCtx, cancel := context.WithDeadline(ctx, deadline)
		e, err := endConversation(store.WithWriteDeadline(ctx, deadline), lockCtx, a, sessionID, covered, false, false)
		cancel()
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err, last)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			last = err
			if waitErr := rosterRetryWait(ctx); waitErr != nil {
				return errors.Join(waitErr, last)
			}
			continue
		}
		last = err
		if e.at == 0 {
			return errors.Join(fmt.Errorf("conversation %s has no recorded end", sessionID), err)
		}
		if !e.pending {
			// A failure cleared by a later complete retry did not leave an end unwritten.
			return err
		}
		if waitErr := rosterRetryWait(ctx); waitErr != nil {
			return errors.Join(waitErr, last)
		}
	}
}

// endsHold reports whether a session end recorded beside this repo or in the
// shared cache is still to be written here: its span or row is not final, so
// pruning waits. A marker whose session row is ended is stale.
func endsHold(ctx context.Context, c store.Conn, repoDir, folderDir, worktree string) (bool, error) {
	repoEntries, err := listEnds(repoDir)
	if err != nil {
		return false, err
	}
	folderEntries, err := listEnds(folderDir)
	if err != nil {
		return false, err
	}
	for _, rec := range repoEntries {
		var ended bool
		if err := c.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, rec.Session).Scan(&ended); err != nil {
			return false, err
		}
		if !ended {
			return true, nil
		}
	}
	for _, rec := range folderEntries {
		if rec.Worktree != worktree && !slices.Contains(rec.Repos, worktree) {
			continue
		}
		var ended bool
		if err := c.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, rec.Session).Scan(&ended); err != nil {
			return false, err
		}
		if !ended {
			return true, nil
		}
	}
	return false, nil
}
