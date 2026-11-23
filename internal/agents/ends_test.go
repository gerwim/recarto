package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/store"
)

// stopScratch runs a Stop of conversation id one second later and returns its
// result and time.
func stopScratch(t *testing.T, s *scratch, a *app.App, id string) (Result, int64) {
	t.Helper()
	s.clk.Advance(time.Second)
	at := s.clk.Now().UnixMilli()
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, SessionID: id, StopActive: true})
	if err != nil {
		t.Fatal(err)
	}
	return res, at
}

func requireSessionEnd(t *testing.T, repo *app.RepoContext, id string, ended bool, at int64) {
	t.Helper()
	rows := sessionRows(t, repo, id)
	if len(rows) != 1 || rows[0].end.Valid != ended || (ended && rows[0].end.Int64 != at) {
		t.Fatalf("session %s rows in %s = %+v; want ended=%v at=%d", id, repo.Git.Worktree, rows, ended, at)
	}
}

// folderRow returns a's own store as a repo context, for sessionRows.
func folderRC(a *app.App) *app.RepoContext { return a.AllRepos()[0] }

// pendingDir is the pending-ends folder beside rc's store.
func pendingDir(rc *app.RepoContext) string { return repoEndsDir(storeDir(rc.Store)) }

// requireNoPendingFiles fails when the cache's shared pending-ends folder, a's
// Stop claims folder, or the pending-ends folder beside any of repos' stores,
// holds any file but a stale-removal lock.
func requireNoPendingFiles(t *testing.T, a *app.App, repos ...*app.RepoContext) {
	t.Helper()
	dirs := []string{folderEndsDir(a.Store), stopClaimsDir(a.Store)}
	for _, rc := range repos {
		dirs = append(dirs, pendingDir(rc))
	}
	for _, dir := range dirs {
		requireEmpty(t, dir)
	}
}

// requireEmpty fails when dir holds any file but a stale-removal lock. A
// regular file at dir's path (unwritable) holds nothing.
func requireEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr != nil || fi.IsDir() {
			t.Fatal(err)
		}
	}
	var names []string
	for _, e := range entries {
		if e.Name() != staleLockName {
			names = append(names, e.Name())
		}
	}
	if len(names) != 0 {
		t.Fatalf("%s holds %v", dir, names)
	}
}

// failure returns res's failure as text, "" for none.
func failure(res Result) string {
	if res.Failure == nil {
		return ""
	}
	return res.Failure.Error()
}

// unwritable puts a regular file at dir's path, so nothing can be created in it,
// even by root.
func unwritable(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// onPublish routes every record claim through f for the rest of the test.
func onPublish(t *testing.T, f func(dir, name string, data []byte) ([]byte, bool, error)) {
	t.Helper()
	prev := publishEnd
	publishEnd = f
	t.Cleanup(func() { publishEnd = prev })
}

func isGuard(name string) bool { return strings.HasSuffix(name, guardSuffix) }
func isFolder(dir string) bool { return filepath.Base(dir) == "pending-ends" }
func isClaim(dir string) bool  { return filepath.Base(dir) == "stop-claims" }
func isFinal(dir, name string) bool {
	return filepath.Base(dir) == "pending-repo-ends" && !isGuard(name)
}

// age sets path's modification time d before the fake clock's now.
func age(t *testing.T, s *scratch, path string, d time.Duration) {
	t.Helper()
	at := s.clk.Now().Add(-d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// testGuard is a valid plain guard of session in worktree wt.
func testGuard(session, wt, dir string) repoEndRecord {
	return repoEndRecord{Session: session, Agent: "claude-code", Worktree: wt, StartedAt: 1, Dir: dir, Nonce: newNonce()}
}

func TestEndRecordFiles(t *testing.T) {
	t.Run("claim first wins", testClaimEndFirstWins)
	t.Run("stamp guard", testStampGuard)
	t.Run("read under a regular file", testReadEndUnderAFile)
	t.Run("list and sweep", testListAndRemoveStaleEnds)
	t.Run("folder end and its creator", testClaimFolderEnd)
	t.Run("spellings of a record's name", testListSpellings)
	t.Run("a claim's store liveness files", testClaimWrites)
}

func testClaimWrites(t *testing.T) {
	folder := t.TempDir()
	name := folderEndName(gitctx.NonGitID(folder), "s")
	decode := func(writes map[string]string) (folderEndRecord, error) {
		data, err := json.Marshal(folderEndRecord{Session: "s", Dir: folder, EndedAt: 9, Live: liveFilePrefix + "k", Writes: writes})
		if err != nil {
			t.Fatal(err)
		}
		return decodeEnd[folderEndRecord](name, data)
	}
	for _, bad := range []map[string]string{
		{"repo": "../" + liveFilePrefix + "w"},
		{"repo": "w"},
		{"": liveFilePrefix + "w"},
	} {
		if _, err := decode(bad); err == nil {
			t.Fatalf("a claim with writes %v was read", bad)
		}
	}
	want := map[string]string{"folder": liveFilePrefix + "a", "repo": liveFilePrefix + "b"}
	got, err := decode(want)
	if err != nil || !maps.Equal(got.Writes, want) {
		t.Fatalf("claim = %+v, %v; want writes %v", got, err, want)
	}
}

func testClaimEndFirstWins(t *testing.T) {
	dir := t.TempDir()
	g1 := testGuard("s", "/wt", "/folder")
	got, created, err := claimEnd(dir, guardName("s"), g1)
	if err != nil || !created || got != g1 {
		t.Fatalf("first claim = %+v %v %v", got, created, err)
	}
	g2 := testGuard("s", "/other", "/folder")
	got, created, err = claimEnd(dir, guardName("s"), g2)
	if err != nil || created || got != g1 {
		t.Fatalf("second claim = %+v %v %v; want the first", got, created, err)
	}
	if err := os.WriteFile(filepath.Join(dir, endName("u")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := testGuard("u", "/wt", "/folder")
	m.EndedAt = 5
	if _, _, err := claimEnd(dir, endName("u"), m); err == nil {
		t.Fatal("a claim over an unreadable file succeeded")
	}
}

func testStampGuard(t *testing.T) {
	dir := t.TempDir()
	g1 := testGuard("s", "/wt", "/folder")
	if _, _, err := claimEnd(dir, guardName("s"), g1); err != nil {
		t.Fatal(err)
	}
	if err := stampGuard(dir, g1, 7); err != nil {
		t.Fatal(err)
	}
	stamped, ok, err := readEnd[repoEndRecord](dir, guardName("s"))
	if err != nil || !ok || stamped.EndedAt != 7 || stamped.Nonce == g1.Nonce {
		t.Fatalf("stamped guard = %+v %v %v", stamped, ok, err)
	}
	if err := removeEnd(dir, guardName("s"), g1); err != nil {
		t.Fatal(err)
	}
	if again, ok, err := readEnd[repoEndRecord](dir, guardName("s")); err != nil || !ok || again != stamped {
		t.Fatalf("removing the plain guard took the stamped one: %+v %v %v", again, ok, err)
	}
}

func testReadEndUnderAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readEnd[repoEndRecord](filepath.Join(file, "child"), "x"); err == nil || ok {
		t.Fatalf("read under a regular file = %v, %v; want an error", ok, err)
	}
}

func testListAndRemoveStaleEnds(t *testing.T) {
	list := t.TempDir()
	if _, _, err := claimEnd(list, guardName("g"), testGuard("g", "/wt", "/folder")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".claim-x", ".live-y", "junk"} {
		if err := os.WriteFile(filepath.Join(list, name), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recs, other, err := listEnds[repoEndRecord](list)
	var names []string
	for _, e := range other {
		names = append(names, e.name)
	}
	slices.Sort(names)
	if err != nil || len(recs) != 1 || recs[0].rec.Session != "g" || !slices.Equal(names, []string{".claim-x", ".live-y", "junk"}) {
		t.Fatalf("list = %+v, %v, %v", recs, names, err)
	}

	now := time.Now()
	setAge := func(name string, d time.Duration) {
		at := now.Add(-d)
		if err := os.Chtimes(filepath.Join(list, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	young := EndRunLimit - time.Minute
	for _, name := range names {
		setAge(name, young)
	}
	_, other, _ = listEnds[repoEndRecord](list)
	if err := removeStaleEnds[repoEndRecord](list, other, now); err != nil {
		t.Fatal(err)
	}
	if _, other, _ = listEnds[repoEndRecord](list); len(other) != 3 {
		t.Fatalf("young entries removed: %+v", other)
	}
	held := ".live-held"
	release, ok, err := store.TryLockFile(filepath.Join(list, held))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer release()
	for _, name := range append(names, held) {
		setAge(name, EndRunLimit+time.Minute)
	}
	_, other, _ = listEnds[repoEndRecord](list)
	if err := removeStaleEnds[repoEndRecord](list, other, now); err != nil {
		t.Fatal(err)
	}
	recs, other, _ = listEnds[repoEndRecord](list)
	if len(recs) != 1 || len(other) != 1 || other[0].name != held {
		t.Fatalf("after the old entries went: %+v, %+v; want the record and the held liveness file", recs, other)
	}
}

func testClaimFolderEnd(t *testing.T) {
	fdir := t.TempDir()
	folder := t.TempDir()
	f, created, rel, err := claimFolderEnd(fdir, folderEndRecord{Session: "s", Dir: folder, EndedAt: 9})
	if err != nil || !created {
		t.Fatalf("claim F = %+v %v %v", f, created, err)
	}
	if running, err := fCreatorRunning(fdir, f); err != nil || !running {
		t.Fatalf("creator running = %v, %v; want true", running, err)
	}
	if _, lost, _, err := claimFolderEnd(fdir, folderEndRecord{Session: "s", Dir: folder, EndedAt: 10}); err != nil || lost {
		t.Fatalf("second claim created=%v err=%v", lost, err)
	}
	rel()
	if running, err := fCreatorRunning(fdir, f); err != nil || running {
		t.Fatalf("creator running after release = %v, %v", running, err)
	}
	entries, err := os.ReadDir(fdir)
	if err != nil || len(entries) != 1 || entries[0].Name() != folderEndName(gitctx.NonGitID(folder), "s") {
		t.Fatalf("after release the folder holds %v, %v; want F alone", entries, err)
	}

	bad := f
	bad.Live = "../" + f.Live
	if _, err := decodeEnd[folderEndRecord](folderEndName(gitctx.NonGitID(folder), "s"), []byte(fmt.Sprintf(
		`{"session":"s","dir":%q,"ended_at":9,"live":%q}`, folder, bad.Live))); err == nil {
		t.Fatal("an F whose liveness file is not a bare name was read")
	}
}

// putRecord writes rec at dir/name as is, whatever the name.
func putRecord(t *testing.T, dir, name string, rec repoEndRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testListSpellings(t *testing.T) {
	dir := t.TempDir()
	g := testGuard("g", "/wt", "/folder")
	if _, _, err := claimEnd(dir, guardName("g"), g); err != nil {
		t.Fatal(err)
	}
	upperG, upperH := strings.ToUpper(guardName("g")), strings.ToUpper(guardName("h"))
	_, err := os.Stat(filepath.Join(dir, upperG))
	insensitive := err == nil
	if !insensitive {
		// Two names of one file, as a case-insensitive volume shows every spelling.
		if err := os.Link(filepath.Join(dir, guardName("g")), filepath.Join(dir, upperG)); err != nil {
			t.Fatal(err)
		}
	}
	h := testGuard("h", "/wt", "/folder")
	putRecord(t, dir, upperH, h)
	recs, other, err := listEnds[repoEndRecord](dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range recs {
		names = append(names, r.name+" "+r.rec.Session)
	}
	slices.Sort(names)
	var otherNames []string
	for _, e := range other {
		otherNames = append(otherNames, e.name)
	}
	want, wantOther := []string{guardName("g") + " g", guardName("g") + " g"}, []string{upperH}
	if insensitive {
		want, wantOther = []string{guardName("g") + " g", guardName("h") + " h"}, nil
		slices.Sort(want)
		if got, ok, err := readEnd[repoEndRecord](dir, guardName("h")); err != nil || !ok || got != h {
			t.Fatalf("read under the lower-case name = %+v, %v, %v", got, ok, err)
		}
	}
	if !slices.Equal(names, want) || !slices.Equal(otherNames, wantOther) {
		t.Fatalf("list = %v, other %v; want %v, other %v", names, otherNames, want, wantOther)
	}
	now := time.Now()
	old := now.Add(-EndRunLimit - time.Minute)
	for _, name := range []string{guardName("g"), upperH} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	_, other, _ = listEnds[repoEndRecord](dir)
	if err := removeStaleEnds[repoEndRecord](dir, other, now); err != nil {
		t.Fatal(err)
	}
	wantLeft := []string{guardName("g"), upperG}
	if insensitive {
		wantLeft = []string{guardName("g"), upperH}
	}
	for _, name := range wantLeft {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s removed: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, upperH)); !insensitive && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the distinct file at another spelling stays: %v", err)
	}
}

func TestEndsHoldItsOwnWorktreeAndRepository(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	st := rc(t, s.open(r), r).Store
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "ends")
	isRepo := func(root string) (bool, bool) {
		switch root {
		case "mine":
			return true, true
		case "theirs":
			return false, true
		}
		return false, false
	}
	check := func(worktree string, want bool) {
		t.Helper()
		got, err := endsHold(ctx, st.DB, dir, worktree, isRepo)
		if err != nil || got != want {
			t.Fatalf("endsHold(%s) = %v, %v; want %v", worktree, got, err, want)
		}
	}
	put := func(name string, rec repoEndRecord) {
		t.Helper()
		if _, created, err := claimEnd(dir, name, rec); err != nil || !created {
			t.Fatalf("claim %s: %v %v", name, created, err)
		}
	}
	clear := func() {
		t.Helper()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	check(r, false)
	plain := testGuard("open", r, "/folder")
	plain.Root = "mine"
	stamped := plain
	stamped.EndedAt = 5
	for _, tc := range []struct {
		name string
		rec  repoEndRecord
	}{{guardName("open"), plain}, {guardName("open"), stamped}, {endName("open"), stamped}} {
		put(tc.name, tc.rec)
		check(r, true) // no row yet
		check("/elsewhere", false)
		clear()
	}
	if _, err := st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at) VALUES ('open', ?, 1)`, r); err != nil {
		t.Fatal(err)
	}
	put(guardName("open"), plain)
	check(r, true)
	clear()
	unasked := plain
	unasked.Root = "new"
	put(guardName("open"), unasked)
	check(r, true)
	clear()
	theirs := plain
	theirs.Root = "theirs"
	put(guardName("open"), theirs)
	check(r, false)
	clear()
	// Another spelling holds only where it is the very file readers open.
	putRecord(t, dir, strings.ToUpper(guardName("open")), plain)
	_, err := os.Stat(filepath.Join(dir, guardName("open")))
	check(r, err == nil)
	clear()
	put(guardName("open"), plain)
	if _, err := st.DB.Exec(`UPDATE sessions SET ended_at=2 WHERE id='open'`); err != nil {
		t.Fatal(err)
	}
	check(r, false)
	if err := os.WriteFile(filepath.Join(dir, endName("x")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	check(r, false)
}

func TestScratchStopLeavesBusyEndsPending(t *testing.T) {
	s := newScratch(t)
	r1, r2 := s.goRepo("one"), s.goRepo("two")
	a := s.open(s.folder, r1, r2)
	for _, wt := range []string{r1, r2} {
		index(t, rc(t, a, wt))
	}
	start(t, a, "ref", "startup")
	want, _ := stopScratch(t, s, a, "ref")
	start(t, a, "s", "startup")
	var later []string
	a.EndLater = func(id string) { later = append(later, id) }
	release1 := holdWriter(t, rc(t, a, r1).Store)
	release2 := holdWriter(t, rc(t, a, r2).Store)
	got, t1 := stopScratch(t, s, a, "s")
	if got.Failure != nil || got.Context != want.Context || got.Block != want.Block || got.SystemMessage != want.SystemMessage {
		t.Fatalf("busy Stop = %+v, reference %+v", got, want)
	}
	if !slices.Equal(later, []string{"s"}) {
		t.Fatalf("background requests = %v, want [s]", later)
	}
	requireSessionEnd(t, folderRC(a), "s", true, t1)
	for _, wt := range []string{r1, r2} {
		requireSessionEnd(t, rc(t, a, wt), "s", false, 0)
	}
	release1()
	got, _ = stopScratch(t, s, a, "s")
	if got.Failure != nil || len(later) != 1 {
		t.Fatalf("first retry = %+v, background requests %v", got, later)
	}
	requireSessionEnd(t, rc(t, a, r1), "s", true, t1)
	release2()
	got, _ = stopScratch(t, s, a, "s")
	if got.Failure != nil || len(later) != 1 {
		t.Fatalf("second retry = %+v, background requests %v", got, later)
	}
	for _, wt := range []string{r1, r2} {
		requireSessionEnd(t, rc(t, a, wt), "s", true, t1)
	}
	requireNoPendingFiles(t, a, rc(t, a, r1), rc(t, a, r2))
}

func TestScratchSessionStartWritesPendingEnds(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	release := holdWriter(t, rc(t, a, r).Store)
	_, t1 := stopScratch(t, s, a, "s")
	if err := endPendingRepoEnds(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, rc(t, a, r), "s", false, 0)
	start(t, a, "m", "startup")
	requireSessionEnd(t, rc(t, a, r), "s", false, 0)
	release()
	start(t, a, "n", "startup")
	requireSessionEnd(t, rc(t, a, r), "s", true, t1)
	requireNoPendingFiles(t, a, rc(t, a, r))
}

func TestScratchNextHookEndsAFreeRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{{name: "next Stop", stop: true}, {name: "session start"}} {
		t.Run(tc.name, func(t *testing.T) { testScratchNextHookEndsAFreeRepo(t, tc.stop) })
	}
}

func testScratchNextHookEndsAFreeRepo(t *testing.T, stop bool) {
	t.Helper()
	s := newScratch(t)
	aPath, bPath := s.goRepo("repo-a"), s.goRepo("repo-b")
	a := s.open(s.folder, aPath, bPath)
	start(t, a, "s", "startup")
	unstick := holdWriter(t, rc(t, a, aPath).Store)
	_, first := stopScratch(t, s, a, "s")
	requireSessionEnd(t, rc(t, a, bPath), "s", true, first)
	unstick()
	if stop {
		if res, _ := stopScratch(t, s, a, "s"); res.Failure != nil {
			t.Fatalf("next Stop failure = %v", res.Failure)
		}
	} else {
		start(t, a, "new", "startup")
	}
	requireSessionEnd(t, rc(t, a, aPath), "s", true, first)
	requireNoPendingFiles(t, a, rc(t, a, aPath), rc(t, a, bPath))
}

// releaseOnRetry makes the background run's first retry wait call release.
func releaseOnRetry(t *testing.T, release func()) *int {
	t.Helper()
	prev := rosterRetryWait
	t.Cleanup(func() { rosterRetryWait = prev })
	calls := 0
	rosterRetryWait = func(context.Context) error {
		calls++
		if calls == 1 {
			release()
		}
		return nil
	}
	return &calls
}

func TestScratchBackgroundRunWritesPendingEnds(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	release := holdWriter(t, rc(t, a, r).Store)
	_, t1 := stopScratch(t, s, a, "s")
	releaseOnRetry(t, release)
	if err := EndRosterSessions(context.Background(), a, "s"); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, rc(t, a, r), "s", true, t1)
	requireNoPendingFiles(t, a, rc(t, a, r))
}

func TestScratchBackgroundRetryReportsLastFullTry(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	if _, err := a.Store.DB.Exec(`CREATE TRIGGER refuse_end BEFORE UPDATE OF ended_at ON sessions
		WHEN OLD.id='s' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	release := holdWriter(t, rc(t, a, r).Store)
	stopScratch(t, s, a, "s")
	ctx, cancel := context.WithCancel(context.Background())
	prev := rosterRetryWait
	t.Cleanup(func() { rosterRetryWait = prev; cancel() })
	rosterRetryWait = func(context.Context) error {
		cancel()
		return context.Canceled
	}
	err := EndRosterSessions(ctx, a, "s")
	release()
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("retry error = %v, want last failure and cancellation", err)
	}
}

func TestScratchBackgroundRetryCancelledDuringTry(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	if _, err := a.Store.DB.Exec(`CREATE TRIGGER refuse_end BEFORE UPDATE OF ended_at ON sessions
		WHEN OLD.id='s' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	releaseWriter := holdWriter(t, rc(t, a, r).Store)
	stopScratch(t, s, a, "s")
	defer releaseWriter()

	ctx, cancel := context.WithCancel(context.Background())
	previousWait := rosterRetryWait
	t.Cleanup(func() { rosterRetryWait = previousWait; cancel() })
	called := false
	rosterRetryWait = func(context.Context) error {
		if !called {
			called = true
			cancel()
		}
		return nil
	}
	err := EndRosterSessions(ctx, a, "s")
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("retry error = %v, want refused and cancellation", err)
	}
}

func TestScratchClaimerStartsTheRun(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	b := s.open(s.folder, r)
	var aRuns, bRuns int
	a.EndLater = func(string) { aRuns++ }
	b.EndLater = func(string) { bRuns++ }
	releaseFolder := holdWriter(t, a.Store)
	releaseRepo := holdWriter(t, rc(t, a, r).Store)
	_, t1 := stopScratch(t, s, a, "s")
	if aRuns != 1 || bRuns != 0 {
		t.Fatalf("after claim: A runs %d, B runs %d", aRuns, bRuns)
	}
	releaseFolder()
	_, _ = stopScratch(t, s, b, "s")
	requireSessionEnd(t, folderRC(a), "s", true, t1)
	requireSessionEnd(t, rc(t, a, r), "s", false, 0)
	_, _ = stopScratch(t, s, b, "s")
	if aRuns != 1 || bRuns != 0 {
		t.Fatalf("later Stop started a run: A %d, B %d", aRuns, bRuns)
	}
	releaseRepo()
	if err := EndRosterSessions(context.Background(), a, "s"); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, rc(t, a, r), "s", true, t1)
	requireNoPendingFiles(t, a, rc(t, a, r))
}

func TestScratchRepoBackAfterTheFirstStop(t *testing.T) {
	s := newScratch(t)
	r1, r2 := s.goRepo("one"), s.goRepo("two")
	a := s.open(s.folder, r1, r2)
	start(t, a, "s", "startup")
	moved := r2 + "-moved"
	if err := os.Rename(r2, moved); err != nil {
		t.Fatal(err)
	}
	gone := s.open(s.folder, r1)
	release1 := holdWriter(t, rc(t, gone, r1).Store)
	res, t1 := stopScratch(t, s, gone, "s")
	if res.Failure != nil {
		t.Fatal(res.Failure)
	}
	r2Dir := storeDir(rc(t, a, r2).Store)
	m, ok, err := readEnd[repoEndRecord](repoEndsDir(r2Dir), endName("s"))
	if err != nil || !ok || m.EndedAt != t1 || m.Worktree != r2 || m.Root != rc(t, a, r2).Git.RootCommit {
		t.Fatalf("repo two's pending end = %+v, %v, %v; want %d", m, ok, err, t1)
	}
	requireSessionEnd(t, folderRC(a), "s", true, t1)
	release1()
	if err := os.Rename(moved, r2); err != nil {
		t.Fatal(err)
	}
	back := s.open(s.folder, r1, r2)
	start(t, back, "m", "startup")
	for _, wt := range []string{r1, r2} {
		requireSessionEnd(t, rc(t, back, wt), "s", true, t1)
	}
	requireNoPendingFiles(t, back, rc(t, back, r1), rc(t, back, r2))
}

func TestScratchDeferredEndReportsMatch(t *testing.T) {
	for _, writer := range []string{"stop", "start", "background"} {
		ref := runDeferredEndReport(t, false, writer, false)
		deferred := runDeferredEndReport(t, true, writer, false)
		assertDeferredReportsMatch(t, writer, ref, deferred)
	}
	ref := runDeferredEndReport(t, false, "stop", true)
	deferred := runDeferredEndReport(t, true, "stop", true)
	assertDeferredReportsMatch(t, "missing row", ref, deferred)
}

type deferredEndReport struct {
	tier, ended string
	baseline    int64
}

// editIn appends function name to file and reports the edit in session.
func editIn(t *testing.T, s *scratch, a *app.App, session, file, name string) {
	t.Helper()
	s.clk.Advance(time.Second)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, "\nfunc %s() {}\n", name); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res, err := Handle(context.Background(), a, Event{Kind: KindPostEdit, SessionID: session, FilePath: file})
	if err != nil || res.Failure != nil {
		t.Fatalf("edit %s: result %+v, err %v", name, res, err)
	}
}

// inRepoSessionWithEdit runs one session in in's own repo: a start, an edit of
// file and a Stop.
func inRepoSessionWithEdit(t *testing.T, s *scratch, in *app.App, n int, file string) {
	t.Helper()
	session := fmt.Sprintf("in-%d", n)
	start(t, in, session, "startup")
	editIn(t, s, in, session, file, fmt.Sprintf("Edit%d", n))
	s.stop(in, session)
}

func runDeferredEndReport(t *testing.T, deferred bool, writer string, missingRow bool) deferredEndReport {
	t.Helper()
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	repoCtx := rc(t, a, r)
	index(t, repoCtx)
	file := filepath.Join(r, "repo", "a.go")
	start(t, a, "A", "startup")
	editIn(t, s, a, "A", file, "First")
	stopScratch(t, s, a, "A")
	start(t, a, "B", "startup")
	if missingRow {
		if _, err := repoCtx.Store.DB.Exec(`DELETE FROM sessions WHERE id='B'`); err != nil {
			t.Fatal(err)
		}
		editIn(t, s, a, "B", file, "WithinB")
	}
	var release func()
	if deferred {
		release = holdWriter(t, repoCtx.Store)
	}
	_, tB := stopScratch(t, s, a, "B")
	if release != nil {
		release()
	}
	if !missingRow {
		editIn(t, s, a, "B", file, "AfterEnd")
	}
	for i := range 10 {
		started := s.clk.Now().Add(time.Second).UnixMilli()
		s.clk.Advance(time.Second)
		if _, err := repoCtx.Store.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at, ended_at) VALUES (?, 'test', ?, ?, ?)`, fmt.Sprintf("noop-%d", i), r, started, started); err != nil {
			t.Fatal(err)
		}
	}
	if deferred {
		switch writer {
		case "stop":
			stopScratch(t, s, a, "B")
		case "start":
			start(t, a, "writer", "startup")
		case "background":
			if err := EndRosterSessions(context.Background(), a, "B"); err != nil {
				t.Fatal(err)
			}
		}
	}
	requireSessionEnd(t, repoCtx, "B", true, tB)
	in := s.open(r)
	block := start(t, in, "in", "startup")
	baseline, ok, err := rc(t, in, r).Graph.DeltaBaseline(context.Background(), r, s.clk.Now().UnixMilli())
	if err != nil || !ok {
		t.Fatalf("baseline = %d %v %v", baseline, ok, err)
	}
	return deferredEndReport{tier: tierLines(block), ended: fmt.Sprint(tB), baseline: baseline}
}

func assertDeferredReportsMatch(t *testing.T, scenario string, ref, deferred deferredEndReport) {
	t.Helper()
	if ref.tier == "" || ref.tier != deferred.tier || ref.baseline != deferred.baseline || ref.ended != deferred.ended {
		t.Fatalf("%s reports differ:\nref: %+v\ndeferred: %+v", scenario, ref, deferred)
	}
}

func TestFolderSessionReadsEndedTime(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	_, t1 := stopScratch(t, s, a, "s")
	row, found, err := folderSession(context.Background(), a.Store, "s")
	if err != nil || !found || !row.ended.Valid || row.ended.Int64 != t1 || row.startedAt == 0 || row.agent == "" {
		t.Fatalf("folder session = %+v, %v, %v", row, found, err)
	}
}

func TestRosterKeepsRepoIdentity(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	ctx := context.Background()
	want := []rosterRepo{{"/a:b/c", "id1", ""}, {"/plain", "id2", "abc123"}}
	if _, err := a.Store.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at) VALUES ('k', 'test', ?, 1)`, s.folder); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error { return writeRoster(ctx, c, "k", want) }); err != nil {
		t.Fatal(err)
	}
	got, err := readRoster(ctx, a.Ledger, "k")
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("roster = %+v, %v; want %+v", got, err, want)
	}
	ids, err := folderRepoIDs(ctx, a.Ledger)
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []string{"id1", "id2"}) {
		t.Fatalf("folder repo ids = %v, %v", ids, err)
	}

	unborn := filepath.Join(s.base, "unborn")
	if err := os.MkdirAll(unborn, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, unborn, "init", "-q")
	roster := rosterOf([]*app.RepoContext{rc(t, a, r), {Git: gitctx.Context{Worktree: unborn, RepoID: "u"}}})
	if roster[0] != (rosterRepo{r, rc(t, a, r).Git.RepoID, rc(t, a, r).Git.RootCommit}) || roster[0].root == "" ||
		roster[1] != (rosterRepo{unborn, "u", ""}) {
		t.Fatalf("roster of a repo and an unborn one = %+v", roster)
	}
}

func TestRecoverIntoReportsAPanic(t *testing.T) {
	var err error
	func() {
		defer recoverInto(&err, "end the session in /x")
		panic("boom")
	}()
	if err == nil || !strings.Contains(err.Error(), "end the session in /x") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("recovered error = %v", err)
	}
}
