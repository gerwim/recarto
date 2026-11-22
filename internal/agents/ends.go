package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/gerwim/recarto/internal/store"
)

// A Stop of a conversation started in a folder without code leaves each session
// end that does not fit its wait pending, in records later writers finish:
//
//   - F, the folder's pending end and the conversation's one claim on its end
//     time, in the cache's shared pending-ends folder;
//   - G, a repo's guard, beside the repository's store: the session row the Stop
//     would have written, linked before the Stop takes its time, so pruning waits
//     for it; a stamped G also carries its time;
//   - M, a repo's final pending end, beside the repository's store, with its time;
//   - K, F's creator's liveness file beside F, locked while the creator runs.
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

// folderEndRecord is a folder's pending session end, in the cache's shared
// pending-ends folder, and the conversation's one claim on its end time.
type folderEndRecord struct {
	Session string   `json:"session"`
	Dir     string   `json:"dir"`               // the folder the conversation started in (App.Dir)
	EndedAt int64    `json:"ended_at"`          // the Stop's time
	Covered []string `json:"covered,omitempty"` // ids of the repositories whose guard was seen before EndedAt was taken
	Live    string   `json:"live"`              // the creator's liveness file in the same folder, locked while it runs
}

// valid checks f at name. Live must be a bare liveness name, so a record can
// never point a probe or a removal at another file.
func (f folderEndRecord) valid(name string) bool {
	return f.Session != "" && f.Dir != "" && f.EndedAt > 0 &&
		filepath.Base(f.Live) == f.Live && strings.HasPrefix(f.Live, liveFilePrefix) &&
		name == folderEndName(gitctx.NonGitID(f.Dir), f.Session)
}

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
}

// valid checks r at name: a guard may wait for its time, a final end carries it.
func (r repoEndRecord) valid(name string) bool {
	return r.Session != "" && r.Agent != "" && r.Worktree != "" && r.Dir != "" && r.Nonce != "" &&
		r.StartedAt > 0 && (strings.HasSuffix(name, guardSuffix) || r.EndedAt > 0)
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
)

func folderEndsDir(st *store.Store) string {
	return filepath.Join(filepath.Dir(filepath.Dir(st.Path())), "pending-ends")
}

// storeDir is st's directory in the cache, named by its repository's cache id.
func storeDir(st *store.Store) string { return filepath.Dir(st.Path()) }

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
		path := filepath.Join(dir, e.name)
		switch {
		case strings.HasPrefix(e.name, claimFilePrefix):
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		case strings.HasPrefix(e.name, liveFilePrefix):
			errs = append(errs, removeLiveFile(dir, e.name))
		default:
			// Only a file that is still unreadable and old goes: one that took the
			// name since may be a valid or young record.
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
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

// claimFolderEnd publishes F with its creator's lock already held on a new
// liveness file K. release drops that lock and removes K.
//
// K is a lock file that is removed, which LockFile forbids for its own files:
// here only its creator ever takes its exclusive lock, once, on a name nobody
// else creates, so removing it never lets two processes hold two files of one name.
func claimFolderEnd(fdir string, rec folderEndRecord) (got folderEndRecord, created bool, release func(), err error) {
	k, err := createUnique(fdir, liveFilePrefix)
	if err != nil {
		return got, false, nil, err
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
		// An unlocked K left behind reads as a finished creator, and a session
		// start removes it once it is older than EndRunLimit.
		_ = os.Remove(kpath)
		return got, false, nil, err
	}
	drop := func() {
		unlock()
		// As above: an unlocked K left behind is harmless and swept later.
		_ = os.Remove(kpath)
	}
	rec.Live = filepath.Base(kpath)
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

// removeFolderEnd removes F at fdir/fname, the record f the caller read, unless
// its creator still runs, and then its liveness file.
func removeFolderEnd(fdir, fname string, f folderEndRecord) error {
	running, err := fCreatorRunning(fdir, f)
	if err != nil || running {
		// The creator removes F itself; F stays while it cannot be told finished.
		return err
	}
	_, err = removeFile(filepath.Join(fdir, fname), recordMatch(fname, func(got folderEndRecord) bool {
		return got.Session == f.Session && got.EndedAt == f.EndedAt && got.Live == f.Live
	}))
	return errors.Join(err, removeLiveFile(fdir, f.Live))
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
	dir  string           // the repository's store directory in the cache
	wt   string           // the conversation's checkout path of it (the roster entry)
	root string           // the roster's root commit for it
	rc   *app.RepoContext // the open checkout at wt, verified to be the roster's repository; nil otherwise
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
			add(endTarget{storeDir(covered[i].Store), e.wt, e.root, covered[i].RepoContext})
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
		add(endTarget{filepath.Dir(p), e.wt, e.root, nil})
	}
	return targets, errors.Join(errs...)
}

// writeRepoEnd writes rec's session end into st: the row the Stop would have
// written, or only its end when the row exists and is open.
func writeRepoEnd(ctx context.Context, st *store.Store, rec repoEndRecord) error {
	return st.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO sessions(id, agent, branch, worktree, started_at, ended_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET ended_at = excluded.ended_at WHERE sessions.ended_at IS NULL`,
			rec.Session, rec.Agent, nullable(rec.Branch), rec.Worktree, rec.StartedAt, rec.EndedAt)
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
	r      repoRecords
	ended  bool
	recErr error         // the guard could not be created: the target is unguarded
	finErr error         // the guard's time could be recorded neither as M nor as a stamp
	finG   repoEndRecord // that guard, with the time it was to get
	busy   bool
	err    error
}

// endWrite runs one store write of endConversation. A var so a test can pause
// or order one store's worker.
var endWrite = func(slot string, write func() error) error { return write() }

// conversationEnding is the state of one endConversation call.
type conversationEnding struct {
	ctx          context.Context
	a            *app.App
	sessionID    string
	targets      []endTarget
	fdir, fname  string
	row          folderRow
	ts           []targetEnd
	fin          *folderEndRecord // the live F, if any
	release      func()           // drops this call's creator lock on F; nil when it holds none
	createdAny   bool             // this call created a record
	folderRecErr error            // F could not be created
	tc           int64            // the common time of a direct write without a record
	folderBusy   bool
	errs         []error
}

// endConversation writes sessionID's folder and repo session ends. With stop it
// also records the ends not yet recorded: guards first, then the time, then F,
// then each guard's time. Store writes use ctx's write deadline. Without stop it
// gives covered guards F's time and writes what is recorded.
func endConversation(ctx context.Context, a *app.App, sessionID string, targets []endTarget, stop bool) (conversationEnd, error) {
	c := &conversationEnding{ctx: ctx, a: a, sessionID: sessionID, targets: targets,
		fdir: folderEndsDir(a.Store), fname: folderEndName(folderID(a.Store), sessionID)}
	if found, err := c.read(); err != nil || !found {
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
	// Every write attempt of this call is over, so another Stop that finds the
	// lock free can treat this one as finished.
	if c.release != nil {
		c.release()
	}
	e := conversationEnd{pending: c.finishFolder()}
	e.pending = c.finishRepos() || e.pending
	e.launch = stop && e.pending && c.createdAny
	return e, errors.Join(c.errs...)
}

// read reads the records, then the rows: a row read open after its record was
// read means no writer had settled that end yet. found is false when the
// folder holds no row of the conversation.
func (c *conversationEnding) read() (found bool, err error) {
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
	if fok && c.row.ended.Valid && c.row.ended.Int64 != f.EndedAt {
		// The stale claim of a Stop that resumed after another settled the folder.
		c.errs = append(c.errs, removeFolderEnd(c.fdir, c.fname, f))
		fok = false
	}
	if fok {
		c.fin = &f
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

// publish records the ends not yet recorded: the guards, then the time, then F,
// then each guard's time.
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
	// prune that saw no guard ran before the time.
	var now int64
	if guarding || needFolder || len(seen) > 0 {
		now = c.a.Clock.Now().UnixMilli()
	}
	createdF := needFolder && c.claimFolder(now, seen)
	c.tc = c.commonTime(now, createdF)
	for _, i := range seen {
		at := now
		if c.fin != nil && c.fin.covers(repoID(c.targets[i].dir)) {
			at = c.fin.EndedAt
		}
		_, created, err := finalizeGuard(c.targets[i], c.ts[i].r.g, at)
		c.createdAny = c.createdAny || created
		if err != nil {
			c.ts[i].finErr = fmt.Errorf("record the pending session end for %s: %w", c.targets[i].wt, err)
			c.ts[i].finG = c.ts[i].r.g
			c.ts[i].finG.EndedAt = at
		}
	}
}

// guard links a guard for each target that has no record and needs one, and
// returns the targets whose guard waits for its time and whether it tried any.
func (c *conversationEnding) guard(needFolder bool) (seen []int, guarding bool) {
	for i, t := range c.targets {
		s := &c.ts[i]
		if s.r.mok || s.r.gok || !((t.rc != nil && !s.ended) || (t.rc == nil && needFolder)) {
			continue
		}
		guarding = true
		g := repoEndRecord{Session: c.sessionID, Agent: c.row.agent, Worktree: t.wt, StartedAt: c.row.startedAt,
			Root: t.root, Dir: c.a.Dir, Nonce: newNonce()}
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

// claimFolder claims F at now, covering the seen guards, and reports whether
// this call created it and it holds.
func (c *conversationEnding) claimFolder(now int64, seen []int) bool {
	rec := folderEndRecord{Session: c.sessionID, Dir: c.a.Dir, EndedAt: now}
	for _, i := range seen {
		rec.Covered = append(rec.Covered, repoID(c.targets[i].dir))
	}
	got, created, release, err := claimFolderEnd(c.fdir, rec)
	if err != nil {
		c.folderRecErr = fmt.Errorf("record the pending session end for %s: %w", c.a.Dir, err)
		return false
	}
	c.fin = &got
	if !created {
		return false
	}
	c.createdAny = true
	again, _, err := folderSession(c.ctx, c.a.Store, c.sessionID)
	c.errs = append(c.errs, err)
	if err == nil && again.ended.Valid && again.ended.Int64 != got.EndedAt {
		release()
		c.errs = append(c.errs, removeFolderEnd(c.fdir, c.fname, got))
		c.fin = nil
		return false
	}
	c.release = release
	return true
}

// commonTime is the time of a direct write without a record: F's while its
// creator still runs, since that creator writes F's time everywhere it can;
// otherwise this Stop's own, now.
func (c *conversationEnding) commonTime(now int64, createdF bool) int64 {
	if c.fin == nil {
		return now
	}
	if createdF {
		return c.fin.EndedAt
	}
	if !slices.ContainsFunc(c.ts, func(s targetEnd) bool { return s.recErr != nil }) {
		return now
	}
	running, err := fCreatorRunning(c.fdir, *c.fin)
	c.errs = append(c.errs, err)
	if running {
		return c.fin.EndedAt
	}
	return now
}

// repoRecord returns what target i's store gets written, if anything.
func (c *conversationEnding) repoRecord(i int, stop bool) (repoEndRecord, bool) {
	s, t := &c.ts[i], c.targets[i]
	if rec, ok := s.r.recorded(); ok {
		return rec, true
	}
	if s.finErr != nil {
		return s.finG, true
	}
	if stop && s.recErr != nil {
		return repoEndRecord{Session: c.sessionID, Agent: c.row.agent, Branch: t.rc.Git.Branch, Worktree: t.wt,
			StartedAt: c.row.startedAt, Dir: c.a.Dir, EndedAt: c.tc}, true
	}
	return repoEndRecord{}, false
}

// writeStores writes every store side by side under the one deadline, so a
// busy store never holds up a free one.
func (c *conversationEnding) writeStores(stop bool) {
	var folderAt int64
	if !c.row.ended.Valid {
		if c.fin != nil {
			folderAt = c.fin.EndedAt
		} else if c.folderRecErr != nil {
			folderAt = c.tc
		}
	}
	var folderErr error
	var wg sync.WaitGroup
	if folderAt != 0 {
		wg.Go(func() {
			c.folderBusy, folderErr = c.writeSlot(c.a.Dir, func() error {
				_, _, err := writeFolderEnd(c.ctx, c.a, c.sessionID, folderAt)
				return err
			}, func() error { return nil })
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
		wg.Go(func() {
			s.busy, s.err = c.writeSlot(t.wt, func() error { return writeRepoEnd(c.ctx, t.rc.Store, rec) },
				func() error { return s.r.remove(pdir) })
		})
	}
	wg.Wait()
	c.errs = append(c.errs, c.folderRecErr, folderErr)
}

// writeSlot runs one store's write and, once it succeeded, done. A panicking
// write is reported and then decided like a busy one.
func (c *conversationEnding) writeSlot(slot string, write, done func() error) (busy bool, err error) {
	panicked := true
	defer func() { busy = busy || panicked }()
	defer recoverInto(&err, "end the session in "+slot)
	werr := endWrite(slot, write)
	panicked = false
	switch {
	case werr == nil:
		return false, done()
	case store.IsBusy(werr):
		return true, nil
	default:
		return false, fmt.Errorf("end the session in %s: %w", slot, werr)
	}
}

// finishFolder settles and removes F once the folder's row is ended, and
// reports whether the folder's end is still pending.
func (c *conversationEnding) finishFolder() bool {
	if c.fin == nil {
		if c.folderBusy {
			c.errs = append(c.errs, fmt.Errorf("session end not recorded in %s: its store stayed busy; the conversation's next Stop writes it", c.a.Dir))
		}
		return false
	}
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
		if !s.busy {
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
	fdir := folderEndsDir(a.Store)
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
			ended, err := rowEnded(ctx, rc.Store.DB, session)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ended {
				errs = append(errs, r.remove(pdir))
				continue
			}
			root := r.g.Root
			if r.mok {
				root = r.m.Root
			}
			if !isRepo(root) {
				// The records belong to another repository at this store path.
				continue
			}
			if r.plain() {
				g := r.g
				f, ok, err := readEnd[folderEndRecord](fdir, folderEndName(gitctx.NonGitID(g.Dir), session))
				if err != nil {
					errs = append(errs, err)
				}
				if !ok || !f.covers(repoID(dir)) {
					// A guard no folder end covers waits for the conversation's next
					// Stop until its Stop and background run are surely gone.
					if info := infos[guardName(session)]; info != nil && older(info, now) {
						errs = append(errs, removeEnd(pdir, guardName(session), g))
					}
					continue
				}
				if _, _, err := finalizeGuard(endTarget{dir: dir}, g, f.EndedAt); err != nil {
					errs = append(errs, fmt.Errorf("record the pending session end for %s: %w", g.Worktree, err))
					continue
				}
				r = readRepoRecords(pdir, session)
			}
			rec, ok := r.recorded()
			if !ok {
				continue
			}
			err = writeRepoEnd(store.WithWriteDeadline(ctx, a.Clock.Now()), rc.Store, rec)
			switch {
			case err == nil:
				errs = append(errs, r.remove(pdir))
			case !store.IsBusy(err):
				errs = append(errs, fmt.Errorf("end the session in %s: %w", rc.Git.Worktree, err))
			}
		}
		errs = append(errs, removeStaleEnds[repoEndRecord](pdir, other, now))
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
	// Its Stop and background run are gone.
	if err := settleFolderEnd(f); err != nil {
		return err
	}
	fa := *a
	fa.Store = st
	_, busy, err := writeFolderEnd(store.WithWriteDeadline(ctx, a.Clock.Now()), &fa, f.Session, f.EndedAt)
	if busy {
		return nil
	}
	if err != nil {
		return fmt.Errorf("end the session in %s: %w", f.Dir, err)
	}
	return removeFolderEnd(fdir, fname, f)
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

// endsHoldFor returns the hold of a prune of rc's journal: whose repository a
// record's root is, git decides now, outside the prune's transaction.
func endsHoldFor(ctx context.Context, rc *app.RepoContext) func(c store.Conn) (bool, error) {
	dir := repoEndsDir(storeDir(rc.Store))
	recs, _, listErr := listEnds[repoEndRecord](dir)
	same := map[string]bool{}
	for _, r := range recs {
		if _, ok := same[r.rec.Root]; !ok && r.rec.Worktree == rc.Git.Worktree {
			same[r.rec.Root] = sameRepository(rc, r.rec.Root)
		}
	}
	isRepo := func(root string) (bool, bool) {
		v, ok := same[root]
		return v, ok
	}
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
