package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/cache"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/store"
)

// A Stop of a conversation started in a folder without code leaves each session
// end that does not fit its wait pending, in records later writers finish:
//
//   - F°, the claim of a running Stop on the conversation's end time, in the
//     folder store's own stop-claims folder: the one claim point of running Stops;
//   - F, the folder's pending end, in the cache's shared pending-ends folder,
//     published only with a running F°'s record;
//   - G, a repo's guard, beside the repository's store: the session row the Stop
//     would have written, linked before the Stop takes its time, so pruning waits
//     for it; a stamped G also carries its time;
//   - M, a repo's final pending end, beside the repository's store, with its time;
//   - K, the liveness file of F's or F°'s creator beside it, locked while the
//     creator runs;
//   - W, one liveness file per store F°'s creator may write, beside F°, locked
//     until the creator's last write attempt on that store.
//
// Every record is a fully written file linked or renamed into its name, never
// changed in place, and every removal of one goes through store.RemoveIf.

const (
	// WarmLimit bounds one recarto warm run.
	WarmLimit = 30 * time.Minute
	// EndRunGrace lets an end run retry a writer released just before WarmLimit.
	EndRunGrace = 5 * time.Second
	// EndRunLimit is the longest a Stop's background end run lives. A pending-end
	// entry that cannot be read, a leftover temp or liveness file, or a guard with
	// no time that no pending folder end covers, is removed once it is older.
	EndRunLimit = WarmLimit + 2*EndRunGrace
)

// folderEndRecord is a conversation's claim on its end time. In the folder
// store's stop-claims folder it is the claim of a running Stop (F°); in the
// cache's shared pending-ends folder it is the folder's pending session end (F).
type folderEndRecord struct {
	Session string   `json:"session"`
	Dir     string   `json:"dir"`               // the folder the conversation started in (App.Dir)
	EndedAt int64    `json:"ended_at"`          // the Stop's time
	Covered []string `json:"covered,omitempty"` // ids of the repositories whose guard was seen before EndedAt was taken
	Live    string   `json:"live"`              // the creator's liveness file in the same folder, locked while it runs
	// Writes is F°'s only: for each store its creator may still write (the
	// folder by its folder id, a repo by its repository id), a liveness file in
	// the same folder that the creator holds until its last write attempt there.
	// A Stop that borrows this claim leaves a store to its creator only while the
	// creator may still write it; once the creator has made its last attempt
	// there, the store is the borrower's to write.
	Writes map[string]string `json:"writes,omitempty"`
}

// valid checks f at name. Every liveness name must be bare, so a record can
// never point a probe or a removal at another file.
func (f folderEndRecord) valid(name string) bool {
	for k, w := range f.Writes {
		if k == "" || !liveName(w) {
			return false
		}
	}
	return f.Session != "" && f.Dir != "" && f.EndedAt > 0 && liveName(f.Live) &&
		name == folderEndName(gitctx.NonGitID(f.Dir), f.Session)
}

// liveName reports whether s is the bare name of a liveness file.
func liveName(s string) bool { return filepath.Base(s) == s && strings.HasPrefix(s, liveFilePrefix) }

// covers reports whether f's time is the time of the guard of repository id.
func (f folderEndRecord) covers(id string) bool { return slices.Contains(f.Covered, id) }

// repoEndRecord is a repo's pending session end, beside the repository's store:
// the session row the Stop would have written. A guard without EndedAt waits for
// its time from Dir's pending end or the next Stop; pruning already waits for it.
type repoEndRecord struct {
	Session   string `json:"session"`
	Agent     string `json:"agent"`
	Branch    string `json:"branch,omitempty"`
	Worktree  string `json:"worktree"`
	StartedAt int64  `json:"started_at"`
	Root      string `json:"root,omitempty"` // a root commit of the repository; "" when unknown
	Dir       string `json:"dir"`            // the conversation's folder
	EndedAt   int64  `json:"ended_at,omitempty"`
	Nonce     string `json:"nonce"` // unique to this file, so a removal can tell it from a later file at the same name
	// BlockMark is the mark the row gets if this record writes it.
	BlockMark sql.NullInt64 `json:"block_mark,omitzero"`
}

// valid checks r at name: a guard may wait for its time, a final end carries
// it. A record counts only at the name its readers open.
func (r repoEndRecord) valid(name string) bool {
	return r.Session != "" && r.Agent != "" && r.Worktree != "" && r.Dir != "" && r.Nonce != "" &&
		r.StartedAt > 0 && (name == guardName(r.Session) || (name == endName(r.Session) && r.EndedAt > 0))
}

// endRecordT is a pending-end record kind.
type endRecordT interface {
	folderEndRecord | repoEndRecord
	valid(name string) bool
}

const (
	guardSuffix     = "-guard"
	claimFilePrefix = ".claim-"
	liveFilePrefix  = ".live-"
	// staleLockName is a pending-ends folder's lock for removing files their
	// owner may not read; it is never removed, as no lock file is.
	staleLockName = ".stale.lock"
)

func folderEndsDir(st *store.Store) string {
	return filepath.Join(filepath.Dir(filepath.Dir(st.Path())), "pending-ends")
}

// storeDir is st's directory in the cache, named by its repository's cache id.
func storeDir(st *store.Store) string { return filepath.Dir(st.Path()) }

// stopClaimsDir holds the claims of the running Stops of the folder whose store is st.
func stopClaimsDir(st *store.Store) string { return filepath.Join(storeDir(st), "stop-claims") }

// repoEndsDir holds the pending ends of the repository whose store is in storeDir.
func repoEndsDir(storeDir string) string { return filepath.Join(storeDir, "pending-repo-ends") }

// repoID is the cache id of the repository whose store is in dir.
func repoID(dir string) string { return filepath.Base(dir) }

func folderID(st *store.Store) string { return filepath.Base(filepath.Dir(st.Path())) }

func endName(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(h[:])
}

func folderEndName(folder, sessionID string) string {
	return endName(folder + "\x00" + sessionID)
}

func guardName(sessionID string) string { return endName(sessionID) + guardSuffix }

// newNonce returns 16 random hex digits.
func newNonce() string {
	var b [8]byte
	// crypto/rand.Read never fails on supported platforms; it crashes the program instead.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// createUnique creates prefix+nonce in dir, and its directory when needed. A
// 64-bit random name never comes up twice in practice, so removing such a file
// by name removes only the file its remover decided on, never a later one.
func createUnique(dir, prefix string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for {
		f, err := os.OpenFile(filepath.Join(dir, prefix+newNonce()), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
}

// writeUnique writes data to a new unique file in dir and returns its path.
func writeUnique(dir, prefix string, data []byte) (string, error) {
	f, err := createUnique(dir, prefix)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// A temp left behind is removed by a session start once it is older than EndRunLimit.
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// linkEnd links data at dir/name unless a file is there, and returns the
// existing file's content when one is: of concurrent callers, exactly one
// creates it.
func linkEnd(dir, name string, data []byte) (existing []byte, created bool, err error) {
	tmp, err := writeUnique(dir, claimFilePrefix, data)
	if err != nil {
		return nil, false, err
	}
	// A temp left behind is removed by a session start once it is older than EndRunLimit.
	defer func() { _ = os.Remove(tmp) }()
	path := filepath.Join(dir, name)
	for try := 0; ; try++ {
		err := os.Link(tmp, path)
		if err == nil {
			return nil, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, err
		}
		existing, err := os.ReadFile(path)
		if err == nil {
			return existing, false, nil
		}
		// The existing file was removed between the link and the read: link again, once.
		if !errors.Is(err, fs.ErrNotExist) || try > 0 {
			return nil, false, err
		}
	}
}

// publishEnd creates a record file first-wins. Every F, G and M claim goes
// through it, so a test can step or fail a claim.
var publishEnd = linkEnd

// claimEnd publishes rec as dir/name unless a record is there, and returns the
// record that holds and whether this call published it. An existing file that
// cannot be read as a record is an error.
func claimEnd[T endRecordT](dir, name string, rec T) (got T, created bool, err error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return got, false, err
	}
	existing, created, err := publishEnd(dir, name, data)
	if err != nil || created {
		if created {
			got = rec
		}
		return got, created, err
	}
	got, err = decodeEnd[T](name, existing)
	if err != nil {
		return got, false, fmt.Errorf("pending end %s: %w", filepath.Join(dir, name), err)
	}
	return got, false, nil
}

// decodeEnd parses and validates one record at name.
func decodeEnd[T endRecordT](name string, data []byte) (T, error) {
	var rec T
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if !rec.valid(name) {
		return rec, errors.New("not a pending session end")
	}
	return rec, nil
}

// readEnd reads dir/name: ok is false, with no error, when it is missing.
func readEnd[T endRecordT](dir, name string) (rec T, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	rec, err = decodeEnd[T](name, data)
	if err != nil {
		return rec, false, fmt.Errorf("pending end %s: %w", filepath.Join(dir, name), err)
	}
	return rec, true, nil
}

// endEntry is a pending-ends folder entry with its Lstat info.
type endEntry struct {
	name string
	info fs.FileInfo
}

type namedEnd[T endRecordT] struct {
	endEntry
	rec T
}

// listEnds reads dir's records. other holds every entry that is not a usable
// record: temps, liveness files and records that cannot be read. A young
// unreadable entry may be mid-publication or from a newer Recarto, so it is
// left alone, and removeStaleEnds removes it only once it is old.
func listEnds[T endRecordT](dir string) (recs []namedEnd[T], other []endEntry, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if entry.Name() == staleLockName {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// The entry went since the listing: nothing is left to read or remove.
			continue
		}
		e := endEntry{entry.Name(), info}
		if strings.HasPrefix(e.name, claimFilePrefix) || strings.HasPrefix(e.name, liveFilePrefix) {
			other = append(other, e)
			continue
		}
		// Recarto creates every name in lower case, and its readers and writers
		// open those names. On a case-insensitive volume another spelling opens
		// the same file, so it is that record. On a case-sensitive volume it is a
		// distinct file no reader opens, so it is no record; a failed Lstat leaves
		// it under its own name, where the exact name check decides the same.
		if lower := strings.ToLower(e.name); lower != e.name {
			if li, err := os.Lstat(filepath.Join(dir, lower)); err == nil && os.SameFile(li, info) {
				e.name = lower
			}
		}
		rec, ok, err := readEnd[T](dir, e.name)
		switch {
		case err != nil:
			// Unreadable: it affects only itself.
			other = append(other, e)
		case ok:
			recs = append(recs, namedEnd[T]{e, rec})
		}
	}
	return recs, other, nil
}

// older reports whether info's file is older than EndRunLimit at now.
func older(info fs.FileInfo, now time.Time) bool {
	return now.Sub(info.ModTime()) > EndRunLimit
}

// removeFile removes a record file the caller decided to remove. Every record
// removal goes through it, so a test can run another writer between a removal's
// decision and its open.
var removeFile = store.RemoveIf

// recordMatch accepts an opened file holding exactly want at name.
func recordMatch[T endRecordT](name string, same func(T) bool) func(f *os.File, fi fs.FileInfo) bool {
	return func(f *os.File, _ fs.FileInfo) bool {
		data, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		got, err := decodeEnd[T](name, data)
		return err == nil && same(got)
	}
}

// removeStaleEnds removes the entries older than EndRunLimit: unreadable
// records, temps and liveness files no creator holds.
func removeStaleEnds[T endRecordT](dir string, entries []endEntry, now time.Time) error {
	var errs []error
	for _, e := range entries {
		if !older(e.info, now) {
			continue
		}
		switch {
		case strings.HasPrefix(e.name, claimFilePrefix):
			if err := os.Remove(filepath.Join(dir, e.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		case strings.HasPrefix(e.name, liveFilePrefix):
			// A process holds a liveness file's lock only through an open of it,
			// which none can make once its owner bits deny the user reading it.
			err := removeLiveFile(dir, e.name)
			if errors.Is(err, fs.ErrPermission) {
				err = removeDenied(dir, e, err)
			}
			errs = append(errs, err)
		default:
			errs = append(errs, removeStaleRecord[T](dir, e, now))
		}
	}
	return errors.Join(errs...)
}

// removeStaleRecord removes the old entry e that cannot be read as a record.
func removeStaleRecord[T endRecordT](dir string, e endEntry, now time.Time) error {
	path := filepath.Join(dir, e.name)
	// Only a file that is still unreadable and old goes: one that took the name
	// since may be a valid or young record.
	_, err := removeFile(path, func(f *os.File, fi fs.FileInfo) bool {
		if !older(fi, now) {
			return false
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return true
		}
		_, err = decodeEnd[T](e.name, data)
		return err != nil
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrPermission):
		return removeDenied(dir, e, err)
	default:
		return fmt.Errorf("remove stale pending end %s: %w", path, err)
	}
}

// removeDenied decides on the old entry e whose removal failed with the
// permission error removeErr.
//
// A permission error from the removal does not say what was refused: Windows
// asks for reading and deleting in one open, and a folder can refuse the
// unlink. Only a file this process cannot read goes to removeUnreadable; a
// readable file whose removal was refused is reported.
func removeDenied(dir string, e endEntry, removeErr error) error {
	path := filepath.Join(dir, e.name)
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return removeUnreadable(dir, e)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("remove stale pending end %s: %w", path, err)
	}
	fi, err := f.Stat()
	// Nothing was written through f, so a close error loses nothing.
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("remove stale pending end %s: %w", path, err)
	}
	if os.SameFile(fi, e.info) {
		return fmt.Errorf("remove stale pending end %s: %w", path, removeErr)
	}
	// The listed entry has left the path, and the file now there is not the one
	// this sweep decided on; the next session start looks at it.
	return nil
}

// removeUnreadable removes the old entry e, which this process cannot read,
// when no process of the user can.
func removeUnreadable(dir string, e endEntry) error {
	path := filepath.Join(dir, e.name)
	// A file it leaves without an error is one store.RemoveUnreadable must not
	// remove, or one another sweep is removing; that is final for this sweep.
	if _, err := store.RemoveUnreadable(path, e.info, filepath.Join(dir, staleLockName)); err != nil {
		return fmt.Errorf("remove stale pending end %s: %w", path, err)
	}
	return nil
}

// removeLiveFile removes a liveness file unless its creator still holds its
// lock. Its name is unique to one creator, which never locks it again once it
// has released it: a free lock means no one will lock it again and no other
// file will take its name, so a plain removal is safe.
func removeLiveFile(dir, name string) error {
	path := filepath.Join(dir, name)
	held, err := store.LockHeld(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || held {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// newLiveFile creates a liveness file in dir with its creator's lock already
// held, and returns its name and the function that drops the lock and removes it.
//
// A liveness file is a lock file that is removed, which LockFile forbids for its
// own files: here only its creator ever takes its exclusive lock, once, on a name
// nobody else creates, so removing it never lets two processes hold two files of
// one name.
func newLiveFile(dir string) (name string, release func(), err error) {
	k, err := createUnique(dir, liveFilePrefix)
	if err != nil {
		return "", nil, err
	}
	kpath := k.Name()
	closeErr := k.Close()
	unlock, ok, err := store.TryLockFile(kpath)
	if err == nil && !ok {
		err = fmt.Errorf("lock %s: held by another process", kpath)
	}
	if err = errors.Join(closeErr, err); err != nil {
		if unlock != nil {
			unlock()
		}
		// An unlocked liveness file left behind reads as a finished creator, and a
		// session start removes it once it is older than EndRunLimit.
		_ = os.Remove(kpath)
		return "", nil, err
	}
	return filepath.Base(kpath), sync.OnceFunc(func() {
		unlock()
		// As above: an unlocked liveness file left behind is harmless and swept later.
		_ = os.Remove(kpath)
	}), nil
}

// claimFolderEnd publishes F with its creator's lock already held on a new
// liveness file K. release drops that lock and removes K.
func claimFolderEnd(fdir string, rec folderEndRecord) (got folderEndRecord, created bool, release func(), err error) {
	live, drop, err := newLiveFile(fdir)
	if err != nil {
		return got, false, nil, err
	}
	rec.Live = live
	got, created, err = claimEnd(fdir, folderEndName(gitctx.NonGitID(rec.Dir), rec.Session), rec)
	if err != nil || !created {
		drop()
		return got, false, nil, err
	}
	return got, true, drop, nil
}

// fCreatorRunning reports whether the Stop that created f still holds the lock
// on its liveness file. A missing liveness file means it finished or died.
func fCreatorRunning(fdir string, f folderEndRecord) (bool, error) {
	held, err := store.LockHeld(filepath.Join(fdir, f.Live))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return held, err
}

// creatorTries reports whether the creator of the Stop claim f in fdir still
// tries the store key: it holds that store's liveness file. A var so a test can
// run the creator's steps between a borrower's reads and this probe.
var creatorTries = func(fdir string, f folderEndRecord, key string) (bool, error) {
	w, ok := f.Writes[key]
	if !ok {
		return false, nil
	}
	held, err := store.LockHeld(filepath.Join(fdir, w))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return held, err
}

// removeFolderEnd removes F at fdir/fname, the record f the caller read, unless
// its creator still runs, and then its liveness files.
func removeFolderEnd(fdir, fname string, f folderEndRecord) error {
	running, err := fCreatorRunning(fdir, f)
	if err != nil || running {
		// The creator removes F itself; F stays while it cannot be told finished.
		return err
	}
	_, err = removeFile(filepath.Join(fdir, fname), recordMatch(fname, func(got folderEndRecord) bool {
		return got.Session == f.Session && got.EndedAt == f.EndedAt && got.Live == f.Live
	}))
	errs := []error{err, removeLiveFile(fdir, f.Live)}
	for _, w := range f.Writes {
		errs = append(errs, removeLiveFile(fdir, w))
	}
	return errors.Join(errs...)
}

// removeEnd removes dir/name if it still holds want, the G or M the caller read.
func removeEnd(dir, name string, want repoEndRecord) error {
	_, err := removeFile(filepath.Join(dir, name), recordMatch(name, func(got repoEndRecord) bool { return got == want }))
	return err
}

// stampGuard replaces the guard g in dir with one that carries its time at.
func stampGuard(dir string, g repoEndRecord, at int64) error {
	g.EndedAt, g.Nonce = at, newNonce()
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	tmp, err := writeUnique(dir, claimFilePrefix, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, guardName(g.Session))); err != nil {
		// A temp left behind is removed by a session start once it is older than EndRunLimit.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// recoverInto turns a panic into an error joined to *errp.
func recoverInto(errp *error, what string) {
	if r := recover(); r != nil {
		*errp = errors.Join(*errp, fmt.Errorf("%s: panic: %v", what, r))
	}
}

// endTarget is one repository a conversation's session end goes to.
type endTarget struct {
	dir       string           // the repository's store directory in the cache
	wt        string           // the conversation's checkout path of it (the roster entry)
	root      string           // the roster's root commit for it
	rc        *app.RepoContext // the open checkout at wt, verified to be the roster's repository; nil otherwise
	blockMark sql.NullInt64    // the mark the row gets if this end writes it
}

// endTargets returns where the session end of each roster repo goes: its open
// checkout when it is still the roster's repository, else the repository's
// store directory alone, found by cache id.
func endTargets(roster []rosterRepo, covered []coveredRepo) ([]endTarget, error) {
	var wg sync.WaitGroup
	for _, c := range covered {
		wg.Go(func() { c.isRoster() })
	}
	wg.Wait()
	var targets []endTarget
	var errs []error
	add := func(t endTarget) {
		// Worktrees of one repository share its store and its one session row.
		if !slices.ContainsFunc(targets, func(o endTarget) bool { return o.dir == t.dir }) {
			targets = append(targets, t)
		}
	}
	for _, e := range roster {
		i := slices.IndexFunc(covered, func(c coveredRepo) bool { return c.Git.Worktree == e.wt })
		if i >= 0 && covered[i].isRoster() {
			add(endTarget{dir: storeDir(covered[i].Store), wt: e.wt, root: e.root, rc: covered[i].RepoContext, blockMark: e.blockMark})
			continue
		}
		if e.id == "" {
			continue
		}
		p, err := cache.StorePath(e.id)
		if err == nil {
			_, err = os.Stat(p)
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		add(endTarget{dir: filepath.Dir(p), wt: e.wt, root: e.root, blockMark: e.blockMark})
	}
	return targets, errors.Join(errs...)
}

// writeRepoEnd writes rec's session end into st: the row the Stop would have
// written, or only its end when the row exists and is open. The record's mark
// is the conversation's latest block mark, and a row written before that block
// read the journal holds a lower one, so an open row also takes the higher
// mark. sessionsOf reads an open row a pending entry covers the same two ways,
// so a range is the same before and after this write.
func writeRepoEnd(ctx context.Context, st *store.Store, rec repoEndRecord) error {
	return st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at, delta_mark)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET ended_at = excluded.ended_at,
			   delta_mark = CASE WHEN excluded.delta_mark > COALESCE(sessions.delta_mark, -1)
			                     THEN excluded.delta_mark ELSE sessions.delta_mark END
			 WHERE sessions.ended_at IS NULL`,
			rec.Session, rec.Agent, nullable(rec.Branch), rec.Worktree, rec.StartedAt, rec.EndedAt, rec.BlockMark)
		return err
	})
}

// rowEnded reports whether c holds sessionID's row with its end written.
func rowEnded(ctx context.Context, c store.Conn, sessionID string) (bool, error) {
	var ended bool
	err := c.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND ended_at IS NOT NULL)`, sessionID).Scan(&ended)
	return ended, err
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

// finalizeGuard gives target t's guard g its time at: it links M first-wins and
// returns the time that holds there. When M cannot be created, it stamps the
// guard. created reports a record this call wrote.
func finalizeGuard(t endTarget, g repoEndRecord, at int64) (int64, bool, error) {
	dir := repoEndsDir(t.dir)
	// A writer whose M could not be created stamped the guard: its time holds.
	if r := readRepoRecords(dir, g.Session); r.gok && r.g.EndedAt > 0 {
		at = r.g.EndedAt
	}
	m := g
	m.EndedAt, m.Nonce = at, newNonce()
	got, created, err := claimEnd(dir, endName(g.Session), m)
	if err == nil {
		return got.EndedAt, created, nil
	}
	// The stamp records the same time, so M's failure loses nothing once it succeeds.
	if serr := stampGuard(dir, g, at); serr != nil {
		return 0, false, errors.Join(err, serr)
	}
	return at, true, nil
}

// repoRecords is what a repository's pending-ends folder holds for a session.
type repoRecords struct {
	m, g     repoEndRecord
	mok, gok bool
}

func readRepoRecords(dir, sessionID string) repoRecords {
	var r repoRecords
	// An unreadable record is not a usable one: publishing over it fails and is
	// reported then, and a session start removes it once it is old.
	r.m, r.mok, _ = readEnd[repoEndRecord](dir, endName(sessionID))
	r.g, r.gok, _ = readEnd[repoEndRecord](dir, guardName(sessionID))
	r.mok = r.mok && r.m.Session == sessionID
	r.gok = r.gok && r.g.Session == sessionID
	return r
}

// recorded returns the time recorded beside the repository: M's, else a stamped G's.
func (r repoRecords) recorded() (repoEndRecord, bool) {
	if r.mok {
		return r.m, true
	}
	if r.gok && r.g.EndedAt > 0 {
		return r.g, true
	}
	return repoEndRecord{}, false
}

// plain reports a guard still waiting for its time.
func (r repoRecords) plain() bool {
	_, ok := r.recorded()
	return r.gok && !ok
}

// remove removes the records r read from dir.
func (r repoRecords) remove(dir string) error {
	var errs []error
	if r.mok {
		errs = append(errs, removeEnd(dir, endName(r.m.Session), r.m))
	}
	if r.gok {
		errs = append(errs, removeEnd(dir, guardName(r.g.Session), r.g))
	}
	return errors.Join(errs...)
}

// settleFolderEnd gives every guard f covers that has no recorded time f's
// time, in the covered repositories' own directories.
func settleFolderEnd(f folderEndRecord) error {
	root, err := cache.Dir()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range f.Covered {
		t := endTarget{dir: filepath.Join(root, id)}
		r := readRepoRecords(repoEndsDir(t.dir), f.Session)
		if !r.plain() || r.g.Dir != f.Dir {
			continue
		}
		if _, _, err := finalizeGuard(t, r.g, f.EndedAt); err != nil {
			errs = append(errs, fmt.Errorf("record the pending session end for %s: %w", r.g.Worktree, err))
		}
	}
	return errors.Join(errs...)
}

// conversationEnd is what endConversation did.
type conversationEnd struct {
	pending bool // an end a later writer can finish is still unwritten in a store this process opened
	launch  bool // pending, and this call created a record: a Stop then starts the background run
}

// targetEnd is endConversation's state of one target.
type targetEnd struct {
	r        repoRecords
	ended    bool
	recErr   error         // the guard could not be created: the target is unguarded
	finErr   error         // the guard's time could be recorded neither as M nor as a stamp
	finG     repoEndRecord // that guard, with the time it was to get
	direct   bool          // a guard the live claim does not cover: written only directly, at tc
	decided  bool          // a slot of this call decided to write this store
	left     bool          // left to the borrowed claim's creator
	leftLock string        // the creator's liveness file of this store, found held when left
	busy     bool
	err      error
}

// errLeftToCreator is a borrowing slot's outcome for a store it leaves to the
// Stop that claimed the time it borrowed, while that Stop still tries it.
var errLeftToCreator = errors.New("left to the Stop that claimed its time")

// endWrite runs one store write of endConversation. A var so a test can pause
// or order one store's worker.
var endWrite = func(slot string, write func() error) error { return write() }

// conversationEnding is the state of one endConversation call.
//
// A Stop claims its time first as F°, in the folder store's own stop-claims
// folder: it gives overlapping Stops of the conversation one first-wins claim
// in a folder the conversation's own store needs anyway, also when the shared
// folder takes no F. F is published only with a running F°'s record, so two
// running Stops never hold claims with different times.
type conversationEnding struct {
	ctx            context.Context
	a              *app.App
	sessionID      string
	targets        []endTarget
	fdir, fname    string
	cdir           string // the folder store's Stop claims
	row            folderRow
	ts             []targetEnd
	fin            *folderEndRecord  // the live claim, if any: F, or a running Stop's F°
	finDir         string            // the folder holding fin: fdir for F, cdir for F°
	ownClaim       *folderEndRecord  // the F° this call created and still holds
	ownF           *folderEndRecord  // the F this call published and still holds
	releases       []func()          // the creator locks this call holds
	writes         map[string]func() // the release of each store's liveness file of the F° this call created, by store key
	createdAny     bool              // this call created a G, an F or an M
	folderRecErr   error             // the folder's end could not be recorded
	tc             int64             // the common time of a direct write
	now            int64             // this Stop's own time
	seen           []int             // the targets whose plain guard this Stop saw before taking now
	folderBusy     bool
	folderLeft     bool   // the folder was left to the borrowed claim's creator
	folderLeftLock string // the creator's liveness file of the folder, found held when left
	folderDecided  bool   // a slot of this call decided to write the folder
	folderErr      error
	errs           []error

	mu sync.Mutex // serializes the slots' write-time decisions and the takeover
}

// endConversation writes sessionID's folder and repo session ends. With stop it
// also records the ends not yet recorded: guards first, then the time, then the
// claim and F, then each guard's time. Store writes use ctx's write deadline.
// Without stop it gives covered guards F's time and writes what is recorded.
func endConversation(ctx context.Context, a *app.App, sessionID string, targets []endTarget, stop bool) (conversationEnd, error) {
	c := &conversationEnding{ctx: ctx, a: a, sessionID: sessionID, targets: targets,
		fdir: folderEndsDir(a.Store), cdir: stopClaimsDir(a.Store), fname: folderEndName(folderID(a.Store), sessionID)}
	if found, err := c.read(stop); err != nil || !found {
		return conversationEnd{}, errors.Join(append(c.errs, err)...)
	}
	if stop {
		c.publish()
	} else if c.fin != nil {
		c.errs = append(c.errs, settleFolderEnd(*c.fin))
	}
	for i, t := range targets {
		c.ts[i].r = readRepoRecords(repoEndsDir(t.dir), sessionID)
	}
	c.writeStores(stop)
	// Every write attempt of this call is over, so another Stop that finds a
	// lock free, or its file gone, can treat this one as finished.
	c.release()
	e := conversationEnd{pending: c.finishFolder()}
	e.pending = c.finishRepos() || e.pending
	e.launch = stop && e.pending && c.createdAny
	return e, errors.Join(c.errs...)
}

// release drops the creator locks this call holds.
func (c *conversationEnding) release() {
	for _, r := range c.releases {
		r()
	}
	c.releases = nil
}

// ownsFin reports whether fin is a file this call created: its F°, or its F.
func (c *conversationEnding) ownsFin() bool {
	return c.fin != nil && (c.fin == c.ownClaim || c.fin == c.ownF)
}

// transient reports whether fin is a Stop claim F°, not the durable F.
func (c *conversationEnding) transient() bool { return c.fin != nil && c.finDir == c.cdir }

// borrowed reports whether fin is another running Stop's F° that no F carries yet.
func (c *conversationEnding) borrowed() bool { return c.fin != nil && c.transient() && !c.ownsFin() }

// live reports whether f's time can still be the folder's end.
func (c *conversationEnding) live(f folderEndRecord) bool {
	return !c.row.ended.Valid || c.row.ended.Int64 == f.EndedAt
}

// read reads the records, then the rows: a row read open after its record was
// read means no writer had settled that end yet. found is false when the
// folder holds no row of the conversation. A Stop also reads the claim of a
// running Stop.
func (c *conversationEnding) read(stop bool) (found bool, err error) {
	var claim *folderEndRecord
	if stop {
		// F° is read before F: a Stop links F° before F and removes F° only after
		// its last write, so a reader that finds neither has missed no running claim.
		claim = c.readClaim()
	}
	f, fok, err := readEnd[folderEndRecord](c.fdir, c.fname)
	c.errs = append(c.errs, err)
	c.row, found, err = folderSession(c.ctx, c.a.Store, c.sessionID)
	if err != nil {
		return false, err
	}
	if !found {
		if fok {
			c.errs = append(c.errs, settleFolderEnd(f), removeFolderEnd(c.fdir, c.fname, f))
		}
		return false, nil
	}
	if fok && !c.live(f) {
		// The stale claim of a Stop that resumed after another settled the folder.
		c.errs = append(c.errs, removeFolderEnd(c.fdir, c.fname, f))
		fok = false
	}
	if claim != nil && !c.live(*claim) {
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, *claim))
		claim = nil
	}
	switch {
	case fok:
		// A live F wins: it differs from a running F° only when its Stop finished
		// between the two reads, and its time is in that Stop's records.
		c.fin, c.finDir = &f, c.fdir
	case claim != nil:
		c.fin, c.finDir = claim, c.cdir
	}
	c.ts = make([]targetEnd, len(c.targets))
	for i, t := range c.targets {
		c.ts[i].r = readRepoRecords(repoEndsDir(t.dir), c.sessionID)
		if t.rc != nil {
			c.ts[i].ended, err = rowEnded(c.ctx, t.rc.Store.DB, c.sessionID)
			c.errs = append(c.errs, err)
		}
	}
	return true, nil
}

// readClaim returns the claim F° of a Stop of this conversation that still
// runs. One whose creator no longer runs is a dead Stop's leftover, removed here.
func (c *conversationEnding) readClaim() *folderEndRecord {
	f, ok, err := readEnd[folderEndRecord](c.cdir, c.fname)
	if err != nil || !ok {
		c.errs = append(c.errs, err)
		return nil
	}
	running, err := fCreatorRunning(c.cdir, f)
	if err != nil {
		// Taken as finished: this Stop's own time is the safe side for the journal.
		c.errs = append(c.errs, err)
		return nil
	}
	if !running {
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, f))
		return nil
	}
	return &f
}

// publish records the ends not yet recorded: the guards, then the time, then
// the claim and F, then each guard's time.
func (c *conversationEnding) publish() {
	needFolder := !c.row.ended.Valid && c.fin == nil
	var seen []int
	for i := range c.ts {
		if c.ts[i].r.plain() {
			seen = append(seen, i)
		}
	}
	guarded, guarding := c.guard(needFolder)
	seen = append(seen, guarded...)
	// The time is taken only after every guard it may be given was seen, so a
	// prune that saw no guard ran before the time. A Stop that borrows another
	// Stop's claim may take over once that Stop has returned, and then claims or
	// writes its own time.
	if guarding || needFolder || len(seen) > 0 || c.transient() {
		c.now = c.a.Clock.Now().UnixMilli()
	}
	c.seen = seen
	c.settleClaim(c.now, seen)
}

// settleClaim claims now as the conversation's time when the folder needs a
// claim, publishes F with the claim's record, and gives each seen guard its time.
func (c *conversationEnding) settleClaim(now int64, seen []int) {
	if !c.row.ended.Valid && c.fin == nil {
		c.claimStop(now, seen)
	}
	// Only the claim's creator publishes it: a Stop that borrows the claim cannot
	// tell, when its link lands, that the creator still runs.
	if c.fin != nil && c.fin == c.ownClaim && !c.row.ended.Valid {
		c.publishFolderEnd()
	}
	c.dropStaleClaim()
	joined := c.joined(seen)
	c.tc = now
	if joined {
		c.tc = c.fin.EndedAt
	}
	for _, i := range seen {
		c.guardTime(i, now, joined)
	}
}

// guard links a guard for each target that has no record and needs one, and
// returns the targets whose guard waits for its time and whether it tried any.
func (c *conversationEnding) guard(needFolder bool) (seen []int, guarding bool) {
	for i, t := range c.targets {
		s := &c.ts[i]
		needed := (t.rc != nil && !s.ended) || (t.rc == nil && needFolder)
		if s.r.mok || s.r.gok || !needed {
			continue
		}
		guarding = true
		g := repoEndRecord{Session: c.sessionID, Agent: c.row.agent, Worktree: t.wt, StartedAt: c.row.startedAt,
			Root: t.root, Dir: c.a.Dir, Nonce: newNonce(), BlockMark: t.blockMark}
		if t.rc != nil {
			g.Branch = t.rc.Git.Branch
		}
		got, created, err := claimEnd(repoEndsDir(t.dir), guardName(c.sessionID), g)
		if err != nil {
			s.recErr = fmt.Errorf("record the pending session end for %s: %w", t.wt, err)
			continue
		}
		c.createdAny = c.createdAny || created
		s.r.g, s.r.gok = got, true
		if got.EndedAt == 0 {
			seen = append(seen, i)
		}
	}
	return seen, guarding
}

// claimStop claims now, covering the seen guards, as the conversation's Stop
// claim F°, or adopts the claim of a Stop that still runs.
//
// When F° cannot be created, as in a cache on a filesystem without hard links,
// this Stop has no claim and writes its own time; a Stop of the conversation
// that overlaps it may then write another. A conversation's Stops rarely
// overlap: a host runs them at the end of turns.
func (c *conversationEnding) claimStop(now int64, seen []int) {
	rec := folderEndRecord{Session: c.sessionID, Dir: c.a.Dir, EndedAt: now}
	for _, i := range seen {
		rec.Covered = append(rec.Covered, repoID(c.targets[i].dir))
	}
	var writes map[string]func()
	var err error
	rec.Writes, writes, err = c.claimWrites()
	if err != nil {
		c.folderRecErr = fmt.Errorf("record the pending session end for %s: %w", c.a.Dir, err)
		return
	}
	dropWrites := func() {
		for _, release := range writes {
			release()
		}
	}
	for try := 0; try < 2; try++ {
		got, created, release, err := claimFolderEnd(c.cdir, rec)
		if err != nil {
			dropWrites()
			c.folderRecErr = fmt.Errorf("record the pending session end for %s: %w", c.a.Dir, err)
			return
		}
		if created {
			c.ownClaim, c.fin, c.finDir = &got, &got, c.cdir
			c.releases = append(c.releases, release)
			if c.writes == nil {
				c.writes = map[string]func(){}
			}
			for key, release := range writes {
				c.writes[key] = release
				c.releases = append(c.releases, release)
			}
			return
		}
		running, err := fCreatorRunning(c.cdir, got)
		c.errs = append(c.errs, err)
		if running {
			dropWrites()
			c.fin, c.finDir = &got, c.cdir
			return
		}
		if try == 0 {
			// A dead Stop's leftover, which never affects a time once its creator stopped.
			c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, got))
		}
	}
	dropWrites()
	c.folderRecErr = fmt.Errorf("record the pending session end for %s: the claim of a Stop that no longer runs is still in %s", c.a.Dir, c.cdir)
}

// claimWrites makes a held liveness file for each store this call may still
// write: the folder, and each reachable repo whose row is open, unless a slot of
// this call has already decided to write it. It returns their names and
// releases by store key.
func (c *conversationEnding) claimWrites() (names map[string]string, releases map[string]func(), err error) {
	var keys []string
	if !c.folderDecided {
		keys = append(keys, c.key(folderSlot))
	}
	for i, t := range c.targets {
		if t.rc != nil && !c.ts[i].ended && !c.ts[i].decided {
			keys = append(keys, c.key(i))
		}
	}
	names, releases = map[string]string{}, map[string]func(){}
	for _, key := range keys {
		name, release, err := newLiveFile(c.cdir)
		if err != nil {
			for _, r := range releases {
				r()
			}
			return nil, nil, err
		}
		names[key], releases[key] = name, release
	}
	return names, releases, nil
}

// key is the store key of slot i in a claim's Writes.
func (c *conversationEnding) key(i int) string {
	if i == folderSlot {
		return folderID(c.a.Store)
	}
	return repoID(c.targets[i].dir)
}

// publishFolderEnd publishes F, the folder's pending end, with the record of
// the claim this call created, so that end survives this call.
func (c *conversationEnding) publishFolderEnd() {
	f := *c.fin
	f.Writes = nil
	got, created, release, err := claimFolderEnd(c.fdir, f)
	if err != nil {
		// The claim lives only as F°; finishFolder decides from F as it is on disk then.
		c.folderRecErr = fmt.Errorf("record the pending session end for %s: %w", c.a.Dir, err)
		return
	}
	c.fin, c.finDir = &got, c.fdir
	if created {
		c.ownF = c.fin
		c.createdAny = true
		c.releases = append(c.releases, release)
		return
	}
	// The existing F carries the same claim, or the time of a Stop that has
	// finished, which is in that Stop's records.
	if c.ownClaim != nil {
		c.release()
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, *c.ownClaim))
		c.ownClaim = nil
	}
}

// dropStaleClaim drops the claim and F this call made once the folder's row was
// ended at another time meanwhile, by a Stop that resumed after it.
func (c *conversationEnding) dropStaleClaim() {
	if !c.ownsFin() {
		return
	}
	again, _, err := folderSession(c.ctx, c.a.Store, c.sessionID)
	if err != nil {
		c.errs = append(c.errs, err)
		return
	}
	if !again.ended.Valid || again.ended.Int64 == c.fin.EndedAt {
		return
	}
	c.release()
	if c.ownF != nil {
		c.errs = append(c.errs, removeFolderEnd(c.fdir, c.fname, *c.ownF))
	}
	if c.ownClaim != nil {
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, *c.ownClaim))
	}
	c.fin, c.ownF, c.ownClaim = nil, nil, nil
}

// joined reports whether this call writes directly with the claim's time: it
// holds the claim, or the claim's creator still runs and writes that time
// wherever it can. The creator is asked only when a direct write needs it.
func (c *conversationEnding) joined(seen []int) bool {
	if c.fin == nil || c.borrowed() {
		// A borrowing Stop writes no store directly, so it needs no common time.
		return false
	}
	if c.ownsFin() {
		return true
	}
	needed := slices.ContainsFunc(c.ts, func(s targetEnd) bool { return s.recErr != nil }) ||
		slices.ContainsFunc(seen, func(i int) bool { return !c.fin.covers(repoID(c.targets[i].dir)) })
	if !needed {
		return false
	}
	running, err := fCreatorRunning(c.finDir, *c.fin)
	c.errs = append(c.errs, err)
	return running
}

// guardTime gives the seen guard of target i its time: the claim's when the
// claim covers it, else this Stop's own, taken after it saw the guard. A guard
// the running claim does not cover gets no recorded time: the claim's time
// predates the guard, so it is only written directly, while that claim's
// creator still runs.
func (c *conversationEnding) guardTime(i int, now int64, joined bool) {
	at := now
	switch {
	case c.borrowed():
		// A Stop that borrows a claim records nothing with its time: the creator
		// records it while it runs, and a record linked here could land after the
		// creator returned.
		return
	case c.fin != nil && c.fin.covers(repoID(c.targets[i].dir)):
		at = c.fin.EndedAt
	case joined:
		c.ts[i].direct = true
		return
	}
	_, created, err := finalizeGuard(c.targets[i], c.ts[i].r.g, at)
	c.createdAny = c.createdAny || created
	if err != nil {
		c.ts[i].finErr = fmt.Errorf("record the pending session end for %s: %w", c.targets[i].wt, err)
		c.ts[i].finG = c.ts[i].r.g
		c.ts[i].finG.EndedAt = at
	}
}

// repoRecord returns what target i's store gets written, if anything.
func (c *conversationEnding) repoRecord(i int, stop bool) (repoEndRecord, bool) {
	s, t := &c.ts[i], c.targets[i]
	if rec, ok := s.r.recorded(); ok {
		return rec, true
	}
	if s.r.plain() {
		g := s.r.g
		switch {
		case c.fin != nil && g.Dir == c.fin.Dir && c.fin.covers(repoID(t.dir)):
			// The claim holds its time, whether or not its copy beside the repository succeeded.
			g.EndedAt = c.fin.EndedAt
			return g, true
		case s.direct:
			g.EndedAt = c.tc
			return g, true
		}
	}
	if s.finErr != nil {
		return s.finG, true
	}
	if stop && s.recErr != nil {
		return c.unguardedRecord(i, c.tc), true
	}
	return repoEndRecord{}, false
}

// unguardedRecord is the session row of target i, whose guard could not be
// created, with its end at at.
func (c *conversationEnding) unguardedRecord(i int, at int64) repoEndRecord {
	t := c.targets[i]
	return repoEndRecord{Session: c.sessionID, Agent: c.row.agent, Branch: t.rc.Git.Branch, Worktree: t.wt,
		StartedAt: c.row.startedAt, Dir: c.a.Dir, EndedAt: at, BlockMark: t.blockMark}
}

// writeStores writes every store side by side under the one deadline, so a
// busy store never holds up a free one.
func (c *conversationEnding) writeStores(stop bool) {
	if c.borrowed() {
		c.writeBorrowed()
		return
	}
	var folderAt int64
	if !c.row.ended.Valid {
		if c.fin != nil {
			folderAt = c.fin.EndedAt
		} else if c.folderRecErr != nil {
			folderAt = c.tc
		}
	}
	var jobs []func()
	launched := map[string]bool{}
	if folderAt != 0 {
		launched[c.key(folderSlot)] = true
		write := c.attempt(c.key(folderSlot), c.folderWrite(folderAt))
		jobs = append(jobs, func() {
			c.folderBusy, _, c.folderErr = c.writeSlot(c.a.Dir, write, func() error { return nil })
		})
	}
	for i, t := range c.targets {
		s := &c.ts[i]
		if t.rc == nil {
			// Its records wait for a session start that serves the repository.
			continue
		}
		pdir := repoEndsDir(t.dir)
		if s.ended {
			c.errs = append(c.errs, s.r.remove(pdir))
			continue
		}
		rec, ok := c.repoRecord(i, stop)
		if !ok {
			continue
		}
		launched[c.key(i)] = true
		write := c.attempt(c.key(i), c.repoWrite(i, rec))
		jobs = append(jobs, func() {
			s.busy, _, s.err = c.writeSlot(t.wt, write, func() error { return s.r.remove(pdir) })
		})
	}
	for key, release := range c.writes {
		if !launched[key] {
			release()
		}
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Go(job)
	}
	wg.Wait()
	c.errs = append(c.errs, c.folderErr)
}

// attempt returns write, followed by the release of the liveness file of store
// key that this call holds as its claim's creator, if any. The release runs
// within the write endWrite runs, right after the store's attempt.
//
// A Stop that borrows this claim waits on this lock to know whether this call
// will still write the store.
func (c *conversationEnding) attempt(key string, write func() error) func() error {
	release := c.writes[key]
	if release == nil {
		return write
	}
	return func() error {
		defer release()
		return write()
	}
}

// releaseWrite releases the liveness file of store key this call holds, if any.
func (c *conversationEnding) releaseWrite(key string) {
	if release := c.writes[key]; release != nil {
		release()
	}
}

// folderSlot is the folder's slot among a borrowing Stop's slots, which are
// otherwise target indexes.
const folderSlot = -1

func (c *conversationEnding) folderWrite(at int64) func() error {
	return func() error {
		_, _, err := writeFolderEnd(c.ctx, c.a, c.sessionID, at)
		return err
	}
}

func (c *conversationEnding) repoWrite(i int, rec repoEndRecord) func() error {
	return func() error { return writeRepoEnd(c.ctx, c.targets[i].rc.Store, rec) }
}

// writeBorrowed writes the stores of a Stop that borrows another running
// Stop's claim. Each slot decides what it writes only when its write runs.
func (c *conversationEnding) writeBorrowed() {
	var slots []int
	if !c.row.ended.Valid {
		slots = append(slots, folderSlot)
	}
	for i, t := range c.targets {
		if t.rc == nil {
			// Its records wait for a session start that serves the repository.
			continue
		}
		if c.ts[i].ended {
			c.errs = append(c.errs, c.ts[i].r.remove(repoEndsDir(t.dir)))
			continue
		}
		slots = append(slots, i)
	}
	var wg sync.WaitGroup
	for _, i := range slots {
		wg.Go(func() { c.runSlot(i) })
	}
	wg.Wait()
	c.errs = append(c.errs, c.folderErr)
}

// runSlot runs one borrowing slot's write, decides again each time the store it
// left is no longer tried by the claim's creator, and once more when its wait
// for that ends without the lock; the last outcome holds.
//
// A slot can leave its store while the claim's creator still tries it, and the
// creator can then stop trying it, or return, without writing it. The slot
// waits for that, within this Stop's own deadline and apart from the other
// stores, and then writes the store unless its end is recorded or the creator
// still tries it.
func (c *conversationEnding) runSlot(i int) {
	write := func() error {
		w, err := c.slotWrite(i)
		if err != nil || w == nil {
			return err
		}
		return w()
	}
	for final := false; ; {
		var left bool
		if i == folderSlot {
			c.folderBusy, c.folderLeft, c.folderErr = c.writeSlot(c.a.Dir, write, func() error { return nil })
			left = c.folderLeft
		} else {
			s, pdir := &c.ts[i], repoEndsDir(c.targets[i].dir)
			s.busy, s.left, s.err = c.writeSlot(c.targets[i].wt, write, func() error { return s.r.remove(pdir) })
			left = s.left
		}
		if !left || final {
			return
		}
		// A wait that ends without the lock ended at this Stop's write deadline,
		// or failed. The creator may have recorded this store's end meanwhile, so
		// the slot decides once more. A passed deadline gives the store write one
		// try, so this adds no wait.
		final = !c.awaitCreator(i)
	}
}

// awaitCreator waits, until this Stop's write deadline, for the claim's creator
// to release the liveness file of the store slot i left to it. It reports
// whether the creator no longer holds it.
func (c *conversationEnding) awaitCreator(i int) bool {
	var path string
	if i == folderSlot {
		path = c.folderLeftLock
	} else {
		path = c.ts[i].leftLock
	}
	release, ok, err := store.WaitShareLockFile(c.ctx, path, nil)
	if errors.Is(err, fs.ErrNotExist) {
		// The creator removes the file only after releasing it.
		return true
	}
	if err != nil {
		c.mu.Lock()
		c.errs = append(c.errs, err)
		c.mu.Unlock()
		return false
	}
	if ok {
		release()
	}
	return ok
}

// slotWrite decides, when slot i's write runs, what it writes: nil for nothing,
// or errLeftToCreator for a store left to the borrowed claim's creator.
//
// A borrowed time lives only while its creator runs, and the creator writes and
// records it itself. So a Stop that borrows it writes only a time recorded
// durably, which every later writer writes too, or its own time for a store the
// creator has stopped trying. Once the creator has returned, it claims anew
// through the same first-wins link as any Stop, and writes its own time or
// borrows the newer claim.
func (c *conversationEnding) slotWrite(i int) (func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.borrowed() {
		if w := c.durableWrite(i); w != nil {
			return w, nil
		}
		running, err := fCreatorRunning(c.cdir, *c.fin)
		// A failed probe is read as returned: this Stop then writes its own time.
		c.errs = append(c.errs, err)
		if !running {
			c.takeOver()
			continue
		}
		key := c.key(i)
		tries, err := creatorTries(c.cdir, *c.fin, key)
		// A failed probe is read as done: this Stop then writes its own time.
		c.errs = append(c.errs, err)
		if tries {
			lock := filepath.Join(c.cdir, c.fin.Writes[key])
			if i == folderSlot {
				c.folderLeftLock = lock
			} else {
				c.ts[i].leftLock = lock
			}
			return nil, errLeftToCreator
		}
		// The creator publishes F and links M before its store writes, and
		// releases this store's lock only after its last write here. Only a read
		// taken after that release shows every time it recorded for this store.
		if w := c.durableWrite(i); w != nil {
			return w, nil
		}
		return c.ownTimeWrite(i), nil
	}
	return c.ownedWrite(i), nil
}

// durableWrite returns slot i's write of a time recorded durably: F's, or one
// recorded beside the repository; nil when there is none. It keeps the repo
// records it read.
func (c *conversationEnding) durableWrite(i int) func() error {
	// An unreadable F records nothing; finishFolder reads it again.
	f, fok, _ := readEnd[folderEndRecord](c.fdir, c.fname)
	if i == folderSlot {
		if !fok {
			return nil
		}
		c.folderDecided = true
		return c.folderWrite(f.EndedAt)
	}
	t, s := c.targets[i], &c.ts[i]
	s.r = readRepoRecords(repoEndsDir(t.dir), c.sessionID)
	rec, ok := s.r.recorded()
	if !ok && s.r.plain() && fok && s.r.g.Dir == f.Dir && f.covers(repoID(t.dir)) {
		rec, ok = s.r.g, true
		rec.EndedAt = f.EndedAt
	}
	if !ok {
		return nil
	}
	s.decided = true
	return c.repoWrite(i, rec)
}

// ownTimeWrite returns slot i's write at this Stop's own time, deciding from the
// records durableWrite last read; nil when there is nothing to write.
//
// The claim's creator has made its last attempt on this store and recorded no
// end there: its records could not be created and the store stayed busy for
// it. That end is the conversation's next Stop's to write, with its own time.
func (c *conversationEnding) ownTimeWrite(i int) func() error {
	if i == folderSlot {
		c.folderDecided = true
		return c.folderWrite(c.now)
	}
	t, s := c.targets[i], &c.ts[i]
	var rec repoEndRecord
	if got, ok := s.r.recorded(); ok {
		rec = got
	} else if s.r.plain() && slices.Contains(c.seen, i) {
		// This Stop saw the guard before taking its time, so the time may be
		// recorded for it, and a busy store then stays pending with it.
		rec = s.r.g
		at, created, err := finalizeGuard(t, s.r.g, c.now)
		c.createdAny = c.createdAny || created
		if err != nil {
			s.finErr = fmt.Errorf("record the pending session end for %s: %w", t.wt, err)
			rec.EndedAt = c.now
			s.finG = rec
		} else {
			rec.EndedAt = at
			s.r = readRepoRecords(repoEndsDir(t.dir), c.sessionID)
		}
	} else if s.r.plain() {
		rec = s.r.g
		rec.EndedAt = c.now
	} else if s.recErr != nil {
		rec = c.unguardedRecord(i, c.now)
	} else {
		return nil
	}
	s.decided = true
	return c.repoWrite(i, rec)
}

// ownedWrite returns slot i's write once this Stop no longer borrows: it owns
// its claim, adopted a durable F, or has no claim.
func (c *conversationEnding) ownedWrite(i int) func() error {
	key := c.key(i)
	if i == folderSlot {
		row, found, err := folderSession(c.ctx, c.a.Store, c.sessionID)
		c.errs = append(c.errs, err)
		if err == nil && (!found || row.ended.Valid) {
			c.releaseWrite(key)
			return nil
		}
		at := c.tc
		if c.fin != nil {
			at = c.fin.EndedAt
		}
		c.folderDecided = true
		return c.attempt(key, c.folderWrite(at))
	}
	s := &c.ts[i]
	s.r = readRepoRecords(repoEndsDir(c.targets[i].dir), c.sessionID)
	rec, ok := c.repoRecord(i, true)
	if !ok {
		c.releaseWrite(key)
		return nil
	}
	s.decided = true
	return c.attempt(key, c.repoWrite(i, rec))
}

// takeOver settles this Stop's own claim once the claim it borrowed has lost
// its creator: it adopts a live F, or claims anew, and times the guards it saw
// that no slot has settled yet. It runs under c.mu.
func (c *conversationEnding) takeOver() {
	c.fin, c.finDir = nil, ""
	if row, found, err := folderSession(c.ctx, c.a.Store, c.sessionID); err != nil {
		c.errs = append(c.errs, err)
	} else if found {
		c.row = row
	}
	f, fok, err := readEnd[folderEndRecord](c.fdir, c.fname)
	c.errs = append(c.errs, err)
	var kept []int
	for _, i := range c.seen {
		// A store a slot of this call already decided to write, or whose guard is
		// gone, is settled; giving it a time now would leave a record nothing removes.
		if c.ts[i].decided {
			continue
		}
		if r := readRepoRecords(repoEndsDir(c.targets[i].dir), c.sessionID); r.plain() {
			c.ts[i].r = r
			kept = append(kept, i)
		}
	}
	if fok && c.live(f) {
		// The folder's end is recorded in F; a takeover never claims a time over it.
		c.fin, c.finDir = &f, c.fdir
	}
	// A guard this Stop saw that F does not cover is timed as by any Stop that
	// reads F; left plain, nothing but the next Stop would end its store.
	c.settleClaim(c.now, kept)
}

// writeSlot runs one store's write and, once it succeeded, done. A panicking
// write is reported and then decided like a busy one.
func (c *conversationEnding) writeSlot(slot string, write, done func() error) (busy, left bool, err error) {
	panicked := true
	defer func() { busy = busy || panicked }()
	defer recoverInto(&err, "end the session in "+slot)
	werr := endWrite(slot, write)
	panicked = false
	switch {
	case werr == nil:
		return false, false, done()
	case errors.Is(werr, errLeftToCreator):
		return false, true, nil
	case store.IsBusy(werr):
		return true, false, nil
	default:
		return false, false, fmt.Errorf("end the session in %s: %w", slot, werr)
	}
}

// finishFolder removes this call's claim, settles and removes F once the
// folder's row is ended, and reports whether the folder's end is still pending.
func (c *conversationEnding) finishFolder() bool {
	if c.ownClaim != nil {
		// Its lock is released, so it goes whatever happens to F.
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, *c.ownClaim))
	}
	if c.fin != nil && !c.transient() {
		return c.finishF()
	}
	// No F was known: the claim's holder may have linked one meanwhile, so the
	// folder's outcome is decided from F as it is on disk now. An unreadable F
	// was reported when read, and records nothing.
	if f, ok, _ := readEnd[folderEndRecord](c.fdir, c.fname); ok {
		row, found, err := folderSession(c.ctx, c.a.Store, c.sessionID)
		c.errs = append(c.errs, err)
		if found && (!row.ended.Valid || row.ended.Int64 == f.EndedAt) {
			// The folder's end is recorded in that F.
			c.folderRecErr = nil
			c.fin, c.finDir = &f, c.fdir
			return c.finishF()
		}
	}
	c.errs = append(c.errs, c.folderRecErr)
	if c.folderBusy && c.folderOpen() {
		c.errs = append(c.errs, fmt.Errorf("session end not recorded in %s: its store stayed busy; the conversation's next Stop writes it", c.a.Dir))
	}
	if c.fin != nil && c.fin != c.ownClaim {
		// Another Stop's claim; it stays while that Stop still holds it.
		c.errs = append(c.errs, removeFolderEnd(c.cdir, c.fname, *c.fin))
	}
	return false
}

// folderOpen reports whether the folder's row, read now, is still open: a Stop
// that borrowed this call's claim may have ended it after this call found its
// store busy. A failed read counts as open.
func (c *conversationEnding) folderOpen() bool {
	row, found, err := folderSession(c.ctx, c.a.Store, c.sessionID)
	c.errs = append(c.errs, err)
	return err != nil || (found && !row.ended.Valid)
}

// finishF settles and removes F, fin, once the folder's row is ended, and
// reports whether the folder's end is still pending.
func (c *conversationEnding) finishF() bool {
	again, found, err := folderSession(c.ctx, c.a.Store, c.sessionID)
	switch {
	case err != nil:
		c.errs = append(c.errs, err)
	case !found || again.ended.Valid:
		if found && again.ended.Int64 == c.fin.EndedAt {
			c.errs = append(c.errs, settleFolderEnd(*c.fin))
		}
		c.errs = append(c.errs, removeFolderEnd(c.fdir, c.fname, *c.fin))
	case c.folderBusy:
		return true
	}
	return false
}

// finishRepos decides each busy repo store: pending when a later writer can
// finish its end, reported when only the conversation's next Stop can.
func (c *conversationEnding) finishRepos() (pending bool) {
	var present *folderEndRecord
	if p, ok, _ := readEnd[folderEndRecord](c.fdir, c.fname); ok {
		// An unreadable F was reported when read; it covers nothing.
		present = &p
	}
	for i, t := range c.targets {
		s := &c.ts[i]
		c.errs = append(c.errs, s.recErr, s.finErr, s.err)
		if !s.busy && !s.left {
			continue
		}
		ended, err := rowEnded(c.ctx, t.rc.Store.DB, c.sessionID)
		if err != nil {
			c.errs = append(c.errs, err)
			continue
		}
		pdir := repoEndsDir(t.dir)
		r := readRepoRecords(pdir, c.sessionID)
		_, recorded := r.recorded()
		switch {
		case ended:
			c.errs = append(c.errs, r.remove(pdir))
		case s.left:
			// Its claim's creator writes it, or names it when its store stays busy.
		case recorded || (r.plain() && present != nil && present.covers(repoID(t.dir))):
			pending = true
		default:
			c.errs = append(c.errs, fmt.Errorf("session end not recorded in %s: its time could not be recorded; the conversation's next Stop writes it", t.wt))
		}
	}
	return pending
}

// endPendingRepoEnds writes, trying each store once, the pending session ends
// recorded beside every store a serves. A session start runs it before its
// blocks, so an end it writes no longer holds back this start's journal pruning.
func endPendingRepoEnds(ctx context.Context, a *app.App) error {
	var errs []error
	now := a.Clock.Now()
	var dirs []string
	for _, rc := range a.AllRepos() {
		dir := storeDir(rc.Store)
		if slices.Contains(dirs, dir) {
			continue
		}
		dirs = append(dirs, dir)
		pdir := repoEndsDir(dir)
		recs, other, err := listEnds[repoEndRecord](pdir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var sessions []string
		infos := map[string]fs.FileInfo{}
		for _, r := range recs {
			infos[r.name] = r.info
			if !slices.Contains(sessions, r.rec.Session) {
				sessions = append(sessions, r.rec.Session)
			}
		}
		same := map[string]bool{}
		isRepo := func(root string) bool {
			v, ok := same[root]
			if !ok {
				v = sameRepository(rc, root)
				same[root] = v
			}
			return v
		}
		for _, session := range sessions {
			r := readRepoRecords(pdir, session)
			if !r.mok && !r.gok {
				continue
			}
			errs = append(errs, endRepoSession(ctx, a, rc, dir, r, infos[guardName(session)], isRepo, now))
		}
		errs = append(errs, removeStaleEnds[repoEndRecord](pdir, other, now))
	}
	return errors.Join(errs...)
}

// coveringFolderEnd returns the folder end that holds the time of g, a plain
// guard beside the repository store in dir: the pending end of g's folder
// conversation in the cache's shared folder, when it covers that repository.
func coveringFolderEnd(fdir, dir string, g repoEndRecord) (folderEndRecord, bool, error) {
	f, ok, err := readEnd[folderEndRecord](fdir, folderEndName(gitctx.NonGitID(g.Dir), g.Session))
	if err != nil || !ok || !f.covers(repoID(dir)) {
		return folderEndRecord{}, false, err
	}
	return f, true, nil
}

// pendingSessions returns the sessions of rc's worktree whose rows wait in a
// pending end beside rc's store: each with its start, the end that holds for
// it (as recovery writes it) and its block's mark. A record of another
// repository at this store path is left out.
func pendingSessions(rc *app.RepoContext) ([]graph.Session, error) {
	pdir, recs, _, err := repoEndsOf(rc)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, rec := range recs {
		if !slices.Contains(ids, rec.Session) {
			ids = append(ids, rec.Session)
		}
	}
	var out []graph.Session
	for _, id := range ids {
		r := readRepoRecords(pdir, id)
		rec, recorded := r.recorded()
		if !recorded {
			rec = r.g
		}
		s := graph.Session{ID: id, StartedAt: rec.StartedAt, Mark: rec.BlockMark}
		if recorded {
			s.EndedAt = sql.NullInt64{Int64: rec.EndedAt, Valid: true}
		} else if f, ok, err := coveringFolderEnd(folderEndsDir(rc.Store), storeDir(rc.Store), r.g); err != nil {
			return nil, err
		} else if ok {
			s.EndedAt = sql.NullInt64{Int64: f.EndedAt, Valid: true}
		}
		out = append(out, s)
	}
	return out, nil
}

// endRepoSession writes one session's pending end r recorded beside the store
// in dir, which rc serves, and removes its records once the row is ended.
// guardInfo is the guard's listing, for its age.
func endRepoSession(ctx context.Context, a *app.App, rc *app.RepoContext, dir string, r repoRecords,
	guardInfo fs.FileInfo, isRepo func(root string) bool, now time.Time) error {
	pdir := repoEndsDir(dir)
	rec, recorded := r.recorded()
	if !recorded {
		rec = r.g
	}
	ended, err := rowEnded(ctx, rc.Store.DB, rec.Session)
	if err != nil {
		return err
	}
	if ended {
		return r.remove(pdir)
	}
	if !isRepo(rec.Root) {
		// The records belong to another repository at this store path.
		return nil
	}
	var errs []error
	if !recorded {
		f, ok, err := coveringFolderEnd(folderEndsDir(a.Store), dir, rec)
		errs = append(errs, err)
		if !ok {
			// A guard no folder end covers waits for the conversation's next
			// Stop until its Stop and background run are surely gone.
			if guardInfo != nil && older(guardInfo, now) {
				errs = append(errs, removeEnd(pdir, guardName(rec.Session), r.g))
			}
			return errors.Join(errs...)
		}
		rec.EndedAt = f.EndedAt
		if at, _, err := finalizeGuard(endTarget{dir: dir}, r.g, f.EndedAt); err != nil {
			// F still holds the time, so the end is written at it all the same.
			errs = append(errs, fmt.Errorf("record the pending session end for %s: %w", r.g.Worktree, err))
		} else {
			rec.EndedAt = at
		}
	}
	err = writeRepoEnd(store.WithWriteDeadline(ctx, a.Clock.Now()), rc.Store, rec)
	switch {
	case err == nil:
		errs = append(errs, readRepoRecords(pdir, rec.Session).remove(pdir))
	case !store.IsBusy(err):
		errs = append(errs, fmt.Errorf("end the session in %s: %w", rc.Git.Worktree, err))
	}
	return errors.Join(errs...)
}

// endPendingFolderEnds sweeps the cache's shared pending-ends folder at a session
// start. It gives the guards of a deleted folder's pending end their time and drops
// it. It clears a pending end whose folder row is already ended, and writes one
// whose Stop and background run are gone. It removes entries it cannot read once
// they are older than EndRunLimit. In a folder (inFolder) it also writes, trying
// each store once, the pending ends of that folder's conversations: those with an
// F there, and those with a repo record in any repository the folder's
// conversations had.
func endPendingFolderEnds(ctx context.Context, a *app.App, inFolder bool) error {
	var errs []error
	fdir := folderEndsDir(a.Store)
	now := a.Clock.Now()
	recs, other, err := listEnds[folderEndRecord](fdir)
	if err != nil {
		return err
	}
	myID := folderID(a.Store)
	var sessions []string
	for _, r := range recs {
		f := r.rec
		if _, err := os.Stat(f.Dir); errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, settleFolderEnd(f), removeFolderEnd(fdir, r.name, f))
			continue
		}
		if inFolder && gitctx.NonGitID(f.Dir) == myID {
			if !slices.Contains(sessions, f.Session) {
				sessions = append(sessions, f.Session)
			}
			continue
		}
		errs = append(errs, settleOtherFolderEnd(ctx, a, f, now.Sub(r.info.ModTime())))
	}
	errs = append(errs, removeStaleEnds[folderEndRecord](fdir, other, now))
	if inFolder {
		errs = append(errs, sweepStopClaims(stopClaimsDir(a.Store), now))
		ids, err := folderRepoIDs(ctx, a.Ledger)
		errs = append(errs, err)
		for _, id := range ids {
			p, err := cache.StorePath(id)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			repoRecs, _, err := listEnds[repoEndRecord](repoEndsDir(filepath.Dir(p)))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, r := range repoRecs {
				if slices.Contains(sessions, r.rec.Session) {
					continue
				}
				has, err := hasSession(ctx, a.Store, r.rec.Session)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if has {
					sessions = append(sessions, r.rec.Session)
				}
			}
		}
	}
	for _, session := range sessions {
		roster, err := readRoster(ctx, a.Ledger, session)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		covered, _, done := cover(a, roster)
		targets, err := endTargets(roster, covered)
		errs = append(errs, err)
		_, err = endConversation(store.WithWriteDeadline(ctx, a.Clock.Now()), a, session, targets, false)
		done()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// sweepStopClaims removes the Stop claims in dir whose creator no longer runs,
// and old entries there that cannot be read.
func sweepStopClaims(dir string, now time.Time) error {
	recs, other, err := listEnds[folderEndRecord](dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		errs = append(errs, removeFolderEnd(dir, r.name, r.rec))
	}
	return errors.Join(append(errs, removeStaleEnds[folderEndRecord](dir, other, now))...)
}

// settleOtherFolderEnd handles F of a folder this start is not in, aged age.
// It opens that folder's store, reads F's row and closes the store.
func settleOtherFolderEnd(ctx context.Context, a *app.App, f folderEndRecord, age time.Duration) (err error) {
	fdir := folderEndsDir(a.Store)
	fname := folderEndName(gitctx.NonGitID(f.Dir), f.Session)
	st, err := app.FolderStore(f.Dir)
	if err != nil {
		return err
	}
	if st == nil {
		return errors.Join(settleFolderEnd(f), removeFolderEnd(fdir, fname, f))
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	row, found, err := folderSession(ctx, st, f.Session)
	switch {
	case err != nil:
		return err
	case !found:
		return errors.Join(settleFolderEnd(f), removeFolderEnd(fdir, fname, f))
	case row.ended.Valid:
		var errs []error
		if row.ended.Int64 == f.EndedAt {
			errs = append(errs, settleFolderEnd(f))
		}
		return errors.Join(append(errs, removeFolderEnd(fdir, fname, f))...)
	case age <= EndRunLimit:
		// Its conversation's own writers still may finish it.
		return nil
	}
	// Its Stop and background run are gone. The folder's end does not depend on
	// the copy of its time to the covered guards.
	settleErr := settleFolderEnd(f)
	fa := *a
	fa.Store = st
	_, busy, err := writeFolderEnd(store.WithWriteDeadline(ctx, a.Clock.Now()), &fa, f.Session, f.EndedAt)
	if busy {
		return settleErr
	}
	if err != nil {
		return errors.Join(settleErr, fmt.Errorf("end the session in %s: %w", f.Dir, err))
	}
	return errors.Join(settleErr, removeFolderEnd(fdir, fname, f))
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
	targets, targetErr := endTargets(roster, covered)
	var last error
	for {
		e, err := endConversation(store.WithWriteDeadline(ctx, a.Clock.Now().Add(writeGrace)), a, sessionID, targets, false)
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), targetErr, err, last)
		}
		last = err
		if !e.pending {
			// A failure cleared by a later complete retry did not leave an end unwritten.
			return errors.Join(targetErr, err)
		}
		if waitErr := rosterRetryWait(ctx); waitErr != nil {
			return errors.Join(waitErr, targetErr, last)
		}
	}
}

// repoEndsOf lists rc's pending end records: the worktree's records whose root
// git says belongs to rc's repository, plus the folder they were read from and a
// filter that knows the roots of that listing. A root the filter was not asked
// about counts as unknown, so a record that appears later still holds a prune.
func repoEndsOf(rc *app.RepoContext) (dir string, recs []repoEndRecord, isRepo func(root string) (same, known bool), err error) {
	dir = repoEndsDir(storeDir(rc.Store))
	all, _, err := listEnds[repoEndRecord](dir)
	if err != nil {
		return dir, nil, nil, err
	}
	same := map[string]bool{}
	for _, r := range all {
		if r.rec.Worktree != rc.Git.Worktree {
			continue
		}
		v, ok := same[r.rec.Root]
		if !ok {
			v = sameRepository(rc, r.rec.Root)
			same[r.rec.Root] = v
		}
		if v {
			recs = append(recs, r.rec)
		}
	}
	isRepo = func(root string) (bool, bool) {
		v, ok := same[root]
		return v, ok
	}
	return dir, recs, isRepo, nil
}

// endsHoldFor returns the hold of a prune of rc's journal: whose repository a
// record's root is, git decides now, outside the prune's transaction.
func endsHoldFor(ctx context.Context, rc *app.RepoContext) func(c store.Conn) (bool, error) {
	dir, _, isRepo, listErr := repoEndsOf(rc)
	return func(c store.Conn) (bool, error) {
		if listErr != nil {
			return false, listErr
		}
		return endsHold(ctx, c, dir, rc.Git.Worktree, isRepo)
	}
}

// endsHold reports whether a pending end of this repository recorded for worktree
// is still unwritten, guards included: pruning then waits. isRepo tells whether a
// record's root belongs to this repository; a root it was not asked about counts
// as this repository's, so a record that appeared since holds.
func endsHold(ctx context.Context, c store.Conn, dir, worktree string, isRepo func(root string) (same, known bool)) (bool, error) {
	recs, _, err := listEnds[repoEndRecord](dir)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.rec.Worktree != worktree {
			continue
		}
		if same, known := isRepo(r.rec.Root); known && !same {
			continue
		}
		ended, err := rowEnded(ctx, c, r.rec.Session)
		if err != nil {
			return false, err
		}
		if !ended {
			return true, nil
		}
	}
	return false, nil
}
