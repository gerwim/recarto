package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/store"
)

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

func requirePendingRecord(t *testing.T, dir, name string, want bool) endRecord {
	t.Helper()
	rec, ok, err := readEnd(dir, name)
	if err != nil || ok != want {
		t.Fatalf("record %s/%s = %+v, %v, %v; want present=%v", dir, name, rec, ok, err, want)
	}
	return rec
}

func requireSessionEnd(t *testing.T, repo *app.RepoContext, id string, ended bool, at int64) {
	t.Helper()
	rows := sessionRows(t, repo, id)
	if len(rows) != 1 || rows[0].end.Valid != ended || (ended && rows[0].end.Int64 != at) {
		t.Fatalf("session %s rows = %+v; want ended=%v at=%d", id, rows, ended, at)
	}
}

func requireNoPendingEnds(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := listEnds(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("records in %s = %+v, %v", dir, entries, err)
		}
	}
}

func TestEndRecords(t *testing.T) {
	dir := t.TempDir()
	want := endRecord{Session: "s", EndedAt: 17, Repos: []string{"/a", "/b"}}
	got, first, err := claimEnd(dir, "f", want)
	if err != nil || !first || got.Session != want.Session || got.EndedAt != want.EndedAt || !slices.Equal(got.Repos, want.Repos) {
		t.Fatalf("first claim = %+v %v %v", got, first, err)
	}
	got, first, err = claimEnd(dir, "f", endRecord{Session: "other", EndedAt: 99})
	if err != nil || first || got.Session != "s" || got.EndedAt != 17 || !slices.Equal(got.Repos, want.Repos) {
		t.Fatalf("second claim = %+v %v %v", got, first, err)
	}
	if got, ok, err := readEnd(dir, "f"); err != nil || !ok || got.EndedAt != 17 {
		t.Fatalf("read = %+v %v %v", got, ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claim-x"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := listEnds(dir)
	if err != nil || len(entries) != 1 || entries[0].Session != "s" {
		t.Fatalf("list = %+v %v", entries, err)
	}
	if err := removeEnd(dir, "f"); err != nil {
		t.Fatal(err)
	}
	if err := removeEnd(dir, "f"); err != nil {
		t.Fatal(err)
	}
}

func TestEndsHold(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	st := rc(t, a, r).Store
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "markers")
	folderDir := filepath.Join(t.TempDir(), "folder-markers")
	check := func(dir string, want bool) {
		t.Helper()
		var got bool
		if err := st.WriteTx(ctx, func(c store.Conn) error {
			var err error
			got, err = endsHold(ctx, c, dir, folderDir, r)
			return err
		}); err != nil || got != want {
			t.Fatalf("endsHold(%s) = %v, %v; want %v", dir, got, err, want)
		}
	}
	check(dir, false)
	if _, err := st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at) VALUES ('folder-open', ?, 1)`, r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimEnd(folderDir, "folder-open", endRecord{Session: "folder-open", Folder: "folder", Repos: []string{r}}); err != nil {
		t.Fatal(err)
	}
	check(dir, true)
	if err := removeEnd(folderDir, "folder-open"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at) VALUES ('open', ?, 1)`, r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimEnd(dir, "open", endRecord{Session: "open", EndedAt: 2}); err != nil {
		t.Fatal(err)
	}
	check(dir, true)
	if _, _, err := claimEnd(dir, "missing", endRecord{Session: "missing", EndedAt: 2}); err != nil {
		t.Fatal(err)
	}
	check(dir, true)
	if _, err := st.DB.Exec(`UPDATE sessions SET ended_at=2 WHERE id='open'`); err != nil {
		t.Fatal(err)
	}
	check(dir, true) // the missing row still holds pruning
	if err := removeEnd(dir, "missing"); err != nil {
		t.Fatal(err)
	}
	check(dir, false)
	if err := os.WriteFile(filepath.Join(dir, ".claim-x"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(dir, false)
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
	if len(later) != 1 || later[0] != "s" {
		t.Fatalf("background requests = %v, want [s]", later)
	}
	if rows := sessionRows(t, a.AllRepos()[0], "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("folder row = %+v, want end %d", rows, t1)
	}
	for _, wt := range []string{r1, r2} {
		if rows := sessionRows(t, rc(t, a, wt), "s"); len(rows) != 1 || rows[0].end.Valid {
			t.Fatalf("%s row = %+v, want open", wt, rows)
		}
	}
	release1()
	got, _ = stopScratch(t, s, a, "s")
	if got.Failure != nil || len(later) != 1 || sessionRows(t, rc(t, a, r1), "s")[0].end.Int64 != t1 {
		t.Fatalf("first retry = %+v, background requests %v", got, later)
	}
	release2()
	got, _ = stopScratch(t, s, a, "s")
	if got.Failure != nil || len(later) != 1 {
		t.Fatalf("second retry = %+v, background requests %v", got, later)
	}
	for _, wt := range []string{r1, r2} {
		if rows := sessionRows(t, rc(t, a, wt), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
			t.Fatalf("%s row = %+v, want end %d", wt, rows, t1)
		}
	}
}

func TestScratchSessionStartWritesPendingEnds(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	release := holdWriter(t, rc(t, a, r).Store)
	_, t1 := stopScratch(t, s, a, "s")
	if err := endPendingConversations(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("held row = %+v, want open", rows)
	}
	start(t, a, "m", "startup")
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("locked session start closed the pending row: %+v", rows)
	}
	release()
	start(t, a, "n", "startup")
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("session-start close = %+v, want %d", rows, t1)
	}
}

func TestScratchStopCoordinationTimeoutDefersUntilSessionStart(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	var later []string
	a.EndLater = func(id string) { later = append(later, id) }
	previousAcquire := acquireEndLock
	acquireEndLock = func(ctx context.Context, covered []coveredRepo, try bool) (func(), bool, error) {
		if !try {
			return nil, false, context.DeadlineExceeded
		}
		return previousAcquire(ctx, covered, true)
	}
	t.Cleanup(func() { acquireEndLock = previousAcquire })
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, SessionID: "s", StopActive: true})
	acquireEndLock = previousAcquire
	if err != nil || res.Failure == nil || !strings.Contains(res.Failure.Error(), context.DeadlineExceeded.Error()) || !slices.Equal(later, []string{"s"}) {
		t.Fatalf("timed-out Stop = %+v, %v; background requests %v", res, err, later)
	}
	launch, ok, err := readEnd(folderEndsDir(a.Store), launchEndName(folderID(a.Store), "s"))
	if err != nil || !ok || launch.EndedAt != 0 || !slices.Equal(launch.Repos, []string{r}) {
		t.Fatalf("launch claim = %+v, %v, %v", launch, ok, err)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("timed-out repo row = %+v, want open", rows)
	}
	start(t, a, "next", "startup")
	if _, ok, err := readEnd(folderEndsDir(a.Store), launchEndName(folderID(a.Store), "s")); err != nil || ok {
		t.Fatalf("launch claim remains after recovery: %v %v", ok, err)
	}
	folder := sessionRows(t, a.AllRepos()[0], "s")
	repo := sessionRows(t, rc(t, a, r), "s")
	if len(folder) != 1 || !folder[0].end.Valid || len(repo) != 1 || !repo[0].end.Valid || folder[0].end.Int64 != repo[0].end.Int64 {
		t.Fatalf("recovered rows differ: folder=%+v repo=%+v", folder, repo)
	}
}

func TestScratchLaterStopStartsNoSecondRun(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	previousAcquire := acquireEndLock
	t.Cleanup(func() { acquireEndLock = previousAcquire })
	acquireEndLock = func(context.Context, []coveredRepo, bool) (func(), bool, error) {
		return nil, false, context.DeadlineExceeded
	}
	var launches int
	a.EndLater = func(string) { launches++ }
	first, _ := stopScratch(t, s, a, "s")
	if !errors.Is(first.Failure, context.DeadlineExceeded) || launches != 1 {
		t.Fatalf("first Stop = %+v; warm launches = %d, want timeout and one launch", first, launches)
	}
	acquireEndLock = previousAcquire
	release := holdWriter(t, rc(t, a, r).Store)
	defer release()
	second, t2 := stopScratch(t, s, a, "s")
	if second.Failure != nil || launches != 1 {
		t.Fatalf("second Stop = %+v; warm launches = %d, want no failure and one launch", second, launches)
	}
	if rows := sessionRows(t, a.AllRepos()[0], "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t2 {
		t.Fatalf("folder row = %+v, want end %d", rows, t2)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("held repo row = %+v, want open", rows)
	}
	release()
	third, _ := stopScratch(t, s, a, "s")
	if third.Failure != nil || launches != 1 {
		t.Fatalf("third Stop = %+v; warm launches = %d, want no failure and one launch", third, launches)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t2 {
		t.Fatalf("repo row = %+v, want end %d", rows, t2)
	}
}

func TestScratchNextHookEndsAFreeRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{{name: "next Stop", stop: true}, {name: "session start"}} {
		t.Run(tc.name, func(t *testing.T) { testScratchNextHookEndsAFreeRepo(t, tc.stop) })
	}
}

func TestScratchOverlappingStopsStartOneRun(t *testing.T) {
	for _, tc := range []struct {
		name           string
		firstToPublish bool
	}{
		{"the inner Stop times out", true},
		{"the outer Stop times out", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			r := s.goRepo("repo")
			a := s.open(s.folder, r)
			start(t, a, "s", "startup")
			release := holdWriter(t, rc(t, a, r).Store)
			defer release()
			var launches int
			a.EndLater = func(string) { launches++ }
			previousAcquire := acquireEndLock
			var nested Result
			inNested := false
			acquireEndLock = func(ctx context.Context, repos []coveredRepo, try bool) (func(), bool, error) {
				inside := inNested
				if !inside {
					inNested = true
					nested, _ = stopScratch(t, s, a, "s")
					inNested = false
				}
				if inside == tc.firstToPublish {
					return nil, false, context.DeadlineExceeded
				}
				return previousAcquire(ctx, repos, try)
			}
			t.Cleanup(func() { acquireEndLock = previousAcquire })
			outer, _ := stopScratch(t, s, a, "s")
			timedOut, gotEnd := outer, nested
			if tc.firstToPublish {
				timedOut, gotEnd = nested, outer
			}
			if gotEnd.Failure != nil || !errors.Is(timedOut.Failure, context.DeadlineExceeded) {
				t.Fatalf("fixture: timed-out failure=%v, timestamping Stop failure=%v", timedOut.Failure, gotEnd.Failure)
			}
			if row := sessionRows(t, rc(t, a, r), "s")[0]; row.end.Valid {
				t.Fatalf("held repo row ended: %+v", row)
			}
			if launches != 1 {
				t.Fatalf("overlapping Stops requested %d end runs; want one", launches)
			}
		})
	}
}

func TestScratchStopAfterASettledEndStartsNoRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout bool
	}{{"times out", true}, {"takes the lock", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			r := s.goRepo("repo")
			a := s.open(s.folder, r)
			start(t, a, "s", "startup")
			var launches int
			a.EndLater = func(string) { launches++ }
			previousAcquire := acquireEndLock
			t.Cleanup(func() { acquireEndLock = previousAcquire })
			firstAcquire := true
			var nested Result
			var nestedAt int64
			acquireEndLock = func(ctx context.Context, repos []coveredRepo, try bool) (func(), bool, error) {
				if firstAcquire {
					firstAcquire = false
					nested, nestedAt = stopScratch(t, s, a, "s")
					t.Cleanup(holdWriter(t, rc(t, a, r).Store))
					if tc.timeout {
						return nil, false, context.DeadlineExceeded
					}
				}
				return previousAcquire(ctx, repos, try)
			}
			outer, _ := stopScratch(t, s, a, "s")
			if nested.Failure != nil {
				t.Fatalf("nested Stop failure = %v", nested.Failure)
			}
			if tc.timeout && !errors.Is(outer.Failure, context.DeadlineExceeded) || !tc.timeout && outer.Failure != nil {
				t.Fatalf("outer Stop failure = %v", outer.Failure)
			}
			if launches != 0 {
				t.Fatalf("settled conversation requested %d end runs; want none", launches)
			}
			if rows := sessionRows(t, a.AllRepos()[0], "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != nestedAt {
				t.Fatalf("folder row = %+v, want end %d", rows, nestedAt)
			}
			if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != nestedAt {
				t.Fatalf("repo row = %+v, want end %d", rows, nestedAt)
			}
			for _, dir := range []string{folderEndsDir(a.Store), repoEndsDir(rc(t, a, r).Store)} {
				if entries, err := listEnds(dir); err != nil || len(entries) != 0 {
					t.Fatalf("records in %s = %+v, %v", dir, entries, err)
				}
			}
		})
	}
}

func TestScratchClaimsLockMissLeavesARequest(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	var launches int
	a.EndLater = func(string) { launches++ }
	release, ok, err := store.TryLockFile(endClaimsLockPath(a.Store))
	if err != nil || !ok {
		t.Fatalf("hold claims lock: %v, %v", ok, err)
	}
	defer release()
	res, _ := stopScratch(t, s, a, "s")
	if !errors.Is(res.Failure, context.DeadlineExceeded) || launches != 0 {
		t.Fatalf("Stop = %+v; launches = %d, want timeout and no launch", res, launches)
	}
	if rows := sessionRows(t, a.AllRepos()[0], "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("folder row = %+v, want open", rows)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("repo row = %+v, want open", rows)
	}
	if _, ok, err := readEnd(folderEndsDir(a.Store), folderEndName(folderID(a.Store), "s")); err != nil || ok {
		t.Fatalf("F = %v, %v; want absent", ok, err)
	}
	if _, ok, err := readEnd(folderEndsDir(a.Store), launchEndName(folderID(a.Store), "s")); err != nil || ok {
		t.Fatalf("L = %v, %v; want absent", ok, err)
	}
	request, ok, err := readEnd(folderEndsDir(a.Store), requestEndName(folderID(a.Store), "s"))
	if err != nil || !ok || request.EndedAt != 0 || request.Repos != nil {
		t.Fatalf("R = %+v, %v, %v; want a time-less request", request, ok, err)
	}
	release()
	start(t, a, "next", "startup")
	folder := sessionRows(t, a.AllRepos()[0], "s")
	repo := sessionRows(t, rc(t, a, r), "s")
	if len(folder) != 1 || !folder[0].end.Valid || len(repo) != 1 || !repo[0].end.Valid || folder[0].end.Int64 != repo[0].end.Int64 {
		t.Fatalf("recovered rows differ: folder=%+v repo=%+v", folder, repo)
	}
	if launches != 0 {
		t.Fatalf("recovery requested %d end runs; want none", launches)
	}
	for _, dir := range []string{folderEndsDir(a.Store), repoEndsDir(rc(t, a, r).Store)} {
		if entries, err := listEnds(dir); err != nil || len(entries) != 0 {
			t.Fatalf("records in %s = %+v, %v", dir, entries, err)
		}
	}
}

func TestScratchCleanupWaitsForTheClaimsLock(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	var launches int
	a.EndLater = func(string) { launches++ }
	releaseWriter := holdWriter(t, rc(t, a, r).Store)
	t.Cleanup(releaseWriter)
	first, t1 := stopScratch(t, s, a, "s")
	if first.Failure != nil || launches != 1 {
		t.Fatalf("first Stop = %+v; launches = %d, want no failure and one launch", first, launches)
	}
	requireSessionEnd(t, a.AllRepos()[0], "s", true, t1)
	requireSessionEnd(t, rc(t, a, r), "s", false, 0)
	fdir, fid := folderEndsDir(a.Store), folderID(a.Store)
	if f := requirePendingRecord(t, fdir, folderEndName(fid, "s"), true); !slices.Equal(f.Repos, []string{r}) {
		t.Fatalf("F = %+v; want repo list [%s]", f, r)
	}
	requirePendingRecord(t, fdir, launchEndName(fid, "s"), false)
	requirePendingRecord(t, fdir, requestEndName(fid, "s"), false)
	requirePendingRecord(t, repoEndsDir(rc(t, a, r).Store), endName("s"), true)
	requirePendingRecord(t, fdir, entryName(fid, "s", r), true)
	if _, first, err := claimEnd(fdir, launchEndName(fid, "s"), endRecord{Session: "s", Folder: fid}); err != nil || !first {
		t.Fatalf("publish stale L: first=%v err=%v", first, err)
	}
	releaseWriter()
	releaseClaims, ok, err := store.TryLockFile(endClaimsLockPath(a.Store))
	if err != nil || !ok {
		t.Fatalf("hold claims lock: %v, %v", ok, err)
	}
	t.Cleanup(releaseClaims)
	second, _ := stopScratch(t, s, a, "s")
	if second.Failure != nil || launches != 1 {
		t.Fatalf("second Stop = %+v; launches = %d, want no failure and one launch", second, launches)
	}
	requireSessionEnd(t, rc(t, a, r), "s", true, t1)
	requirePendingRecord(t, repoEndsDir(rc(t, a, r).Store), endName("s"), false)
	requirePendingRecord(t, fdir, entryName(fid, "s", r), false)
	requirePendingRecord(t, fdir, folderEndName(fid, "s"), true)
	requirePendingRecord(t, fdir, launchEndName(fid, "s"), true)
	releaseClaims()
	start(t, a, "next", "startup")
	if launches != 1 {
		t.Fatalf("cleanup requested %d end runs; want one", launches)
	}
	requireNoPendingEnds(t, fdir, repoEndsDir(rc(t, a, r).Store))
	requireSessionEnd(t, a.AllRepos()[0], "s", true, t1)
	requireSessionEnd(t, rc(t, a, r), "s", true, t1)
}

func TestScratchBackgroundRunRetriesAClaimsLockMiss(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	var launches int
	a.EndLater = func(string) { launches++ }
	previousAcquire := acquireEndLock
	acquireEndLock = func(ctx context.Context, covered []coveredRepo, try bool) (func(), bool, error) {
		if !try {
			return nil, false, context.DeadlineExceeded
		}
		return previousAcquire(ctx, covered, try)
	}
	first, _ := stopScratch(t, s, a, "s")
	acquireEndLock = previousAcquire
	if !errors.Is(first.Failure, context.DeadlineExceeded) || launches != 1 {
		t.Fatalf("Stop = %+v; launches = %d, want timeout and one launch", first, launches)
	}
	fdir, fid := folderEndsDir(a.Store), folderID(a.Store)
	if _, ok, err := readEnd(fdir, launchEndName(fid, "s")); err != nil || !ok {
		t.Fatalf("L = %v, %v; want present", ok, err)
	}
	if _, ok, err := readEnd(fdir, folderEndName(fid, "s")); err != nil || ok {
		t.Fatalf("F = %v, %v; want absent", ok, err)
	}
	if _, ok, err := readEnd(fdir, requestEndName(fid, "s")); err != nil || ok {
		t.Fatalf("R = %v, %v; want absent", ok, err)
	}
	releaseClaims, ok, err := store.TryLockFile(endClaimsLockPath(a.Store))
	if err != nil || !ok {
		t.Fatalf("hold claims lock: %v, %v", ok, err)
	}
	waits := 0
	previousWait := rosterRetryWait
	rosterRetryWait = func(context.Context) error {
		waits++
		if waits == 1 {
			releaseClaims()
			return nil
		}
		return errors.New("unexpected retry wait")
	}
	t.Cleanup(func() {
		acquireEndLock = previousAcquire
		rosterRetryWait = previousWait
		releaseClaims()
	})
	if err := EndRosterSessions(context.Background(), a, "s"); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Fatalf("retry waits = %d, want one", waits)
	}
	if launches != 1 {
		t.Fatalf("background recovery requested %d end runs; want one", launches)
	}
	folder := sessionRows(t, a.AllRepos()[0], "s")
	repo := sessionRows(t, rc(t, a, r), "s")
	if len(folder) != 1 || !folder[0].end.Valid || len(repo) != 1 || !repo[0].end.Valid || folder[0].end.Int64 != repo[0].end.Int64 {
		t.Fatalf("recovered rows differ: folder=%+v repo=%+v", folder, repo)
	}
	for _, dir := range []string{fdir, repoEndsDir(rc(t, a, r).Store)} {
		if entries, err := listEnds(dir); err != nil || len(entries) != 0 {
			t.Fatalf("records in %s = %+v, %v", dir, entries, err)
		}
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
	if row := sessionRows(t, rc(t, a, bPath), "s")[0]; !row.end.Valid {
		t.Fatal("fixture: repo B must already be closed")
	}
	unstick()
	release, ok, err := store.TryLockFile(graph.PendingEndsLockPath(rc(t, a, bPath).Store))
	if err != nil || !ok {
		t.Fatalf("hold repo B's retention lock: %v, %v", ok, err)
	}
	defer release()
	if stop {
		if res, _ := stopScratch(t, s, a, "s"); res.Failure != nil {
			t.Fatalf("next Stop failure = %v", res.Failure)
		}
	} else {
		start(t, a, "new", "startup")
	}
	if got := sessionRows(t, rc(t, a, aPath), "s")[0].end; !got.Valid || got.Int64 != first {
		t.Fatalf("free repo A end = %+v, want %d", got, first)
	}
	if entries, err := listEnds(folderEndsDir(a.Store)); err != nil || len(entries) != 0 {
		t.Fatalf("folder ends = %+v, %v", entries, err)
	}
	if entries, err := listEnds(repoEndsDir(rc(t, a, aPath).Store)); err != nil || len(entries) != 0 {
		t.Fatalf("repo A ends = %+v, %v", entries, err)
	}
}

func TestScratchBackgroundRunWritesPendingEnds(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	release := holdWriter(t, rc(t, a, r).Store)
	_, t1 := stopScratch(t, s, a, "s")
	prev := rosterRetryWait
	t.Cleanup(func() { rosterRetryWait = prev })
	called := false
	rosterRetryWait = func(context.Context) error {
		if !called {
			called = true
			release()
		}
		return nil
	}
	if err := EndRosterSessions(context.Background(), a, "s"); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("background close = %+v, want %d", rows, t1)
	}
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
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), context.Canceled.Error()) || strings.Contains(err.Error(), "no recorded end") {
		t.Fatalf("retry error = %v, want refused and cancellation without no-recorded-end", err)
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
	if rows := sessionRows(t, a.AllRepos()[0], "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("second Stop did not write folder end T1: %+v", rows)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("repo row = %+v, want open", rows)
	}
	_, _ = stopScratch(t, s, b, "s")
	if aRuns != 1 || bRuns != 0 {
		t.Fatalf("later Stop started a run: A %d, B %d", aRuns, bRuns)
	}
	releaseRepo()
	if err := EndRosterSessions(context.Background(), a, "s"); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("repo end = %+v, want %d", rows, t1)
	}
}

func TestScratchWritersBetweenClaimAndMarker(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	s.clk.Advance(time.Second)
	t1 := s.clk.Now().UnixMilli()
	if _, first, err := claimEnd(folderEndsDir(a.Store), folderEndName(folderID(a.Store), "s"), endRecord{Session: "s", Folder: folderID(a.Store), EndedAt: t1, Repos: []string{r}}); err != nil || !first {
		t.Fatalf("claim F: first=%v err=%v", first, err)
	}
	release := holdWriter(t, rc(t, a, r).Store)
	if err := endPendingConversations(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if marker, ok, err := readEnd(repoEndsDir(rc(t, a, r).Store), endName("s")); err != nil || !ok || marker.EndedAt != t1 {
		t.Fatalf("repo marker = %+v %v %v", marker, ok, err)
	}
	if _, ok, err := readEnd(folderEndsDir(a.Store), folderEndName(folderID(a.Store), "s")); err != nil || !ok {
		t.Fatalf("F missing before repo row is written: %v %v", ok, err)
	}
	_, t2 := stopScratch(t, s, a, "s")
	if t2 <= t1 {
		t.Fatalf("test did not advance Stop time: %d then %d", t1, t2)
	}
	release()
	if err := endPendingConversations(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("repo end = %+v, want original %d", rows, t1)
	}
	if entries, err := listEnds(folderEndsDir(a.Store)); err != nil || len(entries) != 0 {
		t.Fatalf("folder ends = %+v, %v", entries, err)
	}
	if entries, err := listEnds(repoEndsDir(rc(t, a, r).Store)); err != nil || len(entries) != 0 {
		t.Fatalf("repo ends = %+v, %v", entries, err)
	}
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
	gone := s.open(s.folder, r1, r2)
	release1 := holdWriter(t, rc(t, gone, r1).Store)
	_, t1 := stopScratch(t, s, gone, "s")
	if f, ok, err := readEnd(folderEndsDir(gone.Store), folderEndName(folderID(gone.Store), "s")); err != nil || !ok || !slices.Equal(f.Repos, []string{r1}) {
		t.Fatalf("first F = %+v, %v, %v", f, ok, err)
	}
	release1()
	if err := os.Rename(moved, r2); err != nil {
		t.Fatal(err)
	}
	back := s.open(s.folder, r1, r2)
	release2 := holdWriter(t, rc(t, back, r2).Store)
	_, t2 := stopScratch(t, s, back, "s")
	if rows := sessionRows(t, rc(t, back, r1), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t1 {
		t.Fatalf("repo one end = %+v, want %d", rows, t1)
	}
	if rows := sessionRows(t, rc(t, back, r2), "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("repo two row = %+v, want open", rows)
	}
	if _, ok, err := readEnd(folderEndsDir(back.Store), folderEndName(folderID(back.Store), "s")); err != nil || ok {
		t.Fatalf("F still exists after its listed repos ended: %v %v", ok, err)
	}
	if d, ok, err := readEnd(folderEndsDir(back.Store), entryName(folderID(back.Store), "s", r2)); err != nil || !ok || d.EndedAt != t2 {
		t.Fatalf("D for repo two = %+v, %v, %v; want end %d", d, ok, err, t2)
	}
	release2()
	start(t, back, "m", "startup")
	if rows := sessionRows(t, rc(t, back, r2), "s"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != t2 {
		t.Fatalf("repo two end = %+v, want %d", rows, t2)
	}
	if entries, err := listEnds(folderEndsDir(back.Store)); err != nil || len(entries) != 0 {
		t.Fatalf("folder ends = %+v, %v", entries, err)
	}
}

func TestScratchSessionStartEndsAnInterruptedPublication(t *testing.T) {
	s := newScratch(t)
	aPath, bPath := s.goRepo("repo-a"), s.goRepo("repo-b")
	a := s.open(s.folder, aPath, bPath)
	start(t, a, "s", "startup")
	if err := os.Rename(bPath, bPath+"-moved"); err != nil {
		t.Fatal(err)
	}
	stopScratch(t, s, s.open(s.folder, aPath, bPath), "s")
	if err := os.Rename(bPath+"-moved", bPath); err != nil {
		t.Fatal(err)
	}
	s.clk.Advance(time.Second)
	when := s.clk.Now().UnixMilli()
	fid := folderID(a.Store)
	dir := folderEndsDir(a.Store)
	if _, exists, err := readEnd(dir, folderEndName(fid, "s")); err != nil || exists {
		t.Fatalf("fixture: F exists=%v, err=%v", exists, err)
	}
	if _, first, err := claimEnd(dir, entryName(fid, "s", bPath), endRecord{
		Session: "s", Folder: fid, Worktree: bPath, EndedAt: when,
	}); err != nil || !first {
		t.Fatalf("publish D: first=%v, err=%v", first, err)
	}
	s.clk.Advance(time.Second)
	start(t, a, "new", "startup")
	if got := sessionRows(t, rc(t, a, bPath), "s")[0].end; !got.Valid || got.Int64 != when {
		t.Fatalf("repo B end = %+v, want %d", got, when)
	}
	if entries, err := listEnds(dir); err != nil || len(entries) != 0 {
		t.Fatalf("folder ends = %+v, %v", entries, err)
	}
	if entries, err := listEnds(repoEndsDir(rc(t, a, bPath).Store)); err != nil || len(entries) != 0 {
		t.Fatalf("repo B ends = %+v, %v", entries, err)
	}
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

func runDeferredEndReport(t *testing.T, deferred bool, writer string, missingRow bool) deferredEndReport {
	t.Helper()
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	repoCtx := rc(t, a, r)
	index(t, repoCtx)
	file := filepath.Join(r, "repo", "a.go")
	edit := func(session, name string) {
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
	start(t, a, "A", "startup")
	edit("A", "First")
	stopScratch(t, s, a, "A")
	start(t, a, "B", "startup")
	if missingRow {
		if _, err := repoCtx.Store.DB.Exec(`DELETE FROM sessions WHERE id='B'`); err != nil {
			t.Fatal(err)
		}
		edit("B", "WithinB")
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
		edit("B", "AfterEnd")
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
	if rows := sessionRows(t, repoCtx, "B"); len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != tB {
		t.Fatalf("B row = %+v, want end %d", rows, tB)
	}
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

func TestScratchStopAndPruneSerializeEndTime(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	a := s.open(s.folder, r)
	start(t, a, "s", "startup")
	rc := rc(t, a, r)
	base := s.clk.Now().UnixMilli()
	bStart := base - 10_000
	seedStopAndPruneRows(t, a, rc, r, bStart)

	pruneChecked := make(chan struct{})
	continuePrune := make(chan struct{})
	pruneFinished := make(chan struct{})
	pruneDone := make(chan error, 1)
	var holdCalls int
	go func() {
		pruneDone <- rc.Graph.PruneUnreachableDeltas(context.Background(), r, "pending", bStart+200, bStart+300, func(store.Conn) (bool, error) {
			holdCalls++
			if holdCalls == 2 {
				close(pruneChecked)
				<-continuePrune
			}
			return false, nil
		})
	}()
	<-pruneChecked

	previousAcquire := acquireEndLock
	lockAttempted := make(chan struct{})
	lockAcquired := make(chan struct{})
	allowEnd := make(chan struct{})
	acquireEndLock = func(ctx context.Context, covered []coveredRepo, try bool) (func(), bool, error) {
		if !try {
			close(lockAttempted)
			<-pruneFinished
		}
		release, ok, err := previousAcquire(ctx, covered, try)
		if !try && err == nil && ok {
			close(lockAcquired)
			<-allowEnd
		}
		return release, ok, err
	}
	t.Cleanup(func() { acquireEndLock = previousAcquire })
	type stopped struct {
		result Result
		err    error
	}
	stopDone := make(chan stopped, 1)
	stopAt := s.clk.Now().UnixMilli()
	go func() {
		result, err := Handle(context.Background(), a, Event{Kind: KindStop, SessionID: "s", StopActive: true})
		stopDone <- stopped{result: result, err: err}
	}()
	<-lockAttempted
	if _, ok, err := readEnd(folderEndsDir(a.Store), folderEndName(folderID(a.Store), "s")); err != nil || ok {
		t.Fatalf("Stop published its end before the prune released the lock: %v %v", ok, err)
	}
	if release, ok, err := store.TryLockFile(graph.PendingEndsLockPath(rc.Store)); err != nil || ok {
		if ok {
			release()
		}
		t.Fatalf("prune did not hold the end coordination lock: ok=%v err=%v", ok, err)
	}
	close(continuePrune)
	if err := <-pruneDone; err != nil {
		t.Fatal(err)
	}
	close(pruneFinished)
	<-lockAcquired
	var older int
	if err := rc.Store.DB.QueryRow(`SELECT COUNT(*) FROM graph_deltas WHERE id='delta-A'`).Scan(&older); err != nil || older != 0 {
		t.Fatalf("prune kept the unreachable earlier delta: count=%d err=%v", older, err)
	}
	s.clk.Advance(time.Second)
	wantEnd := s.clk.Now().UnixMilli()
	if wantEnd <= stopAt {
		t.Fatalf("clock did not advance past Stop start: %d <= %d", wantEnd, stopAt)
	}
	close(allowEnd)
	got := <-stopDone
	if got.err != nil || got.result.Failure != nil {
		t.Fatalf("Stop = %+v, %v", got.result, got.err)
	}
	rows := sessionRows(t, rc, "s")
	if len(rows) != 1 || !rows[0].end.Valid || rows[0].end.Int64 != wantEnd {
		t.Fatalf("repo end = %+v, want serialized time %d", rows, wantEnd)
	}
	baseline, ok, err := rc.Graph.DeltaBaseline(context.Background(), r, wantEnd)
	if err != nil || !ok || baseline != bStart {
		t.Fatalf("report baseline = %d %v %v, want B at %d", baseline, ok, err, bStart)
	}
	deltas, total, err := rc.Graph.DeltasSince(context.Background(), r, baseline-1, 200)
	if err != nil || total != 1 || len(deltas) != 1 || deltas[0].QualifiedName != "During" {
		t.Fatalf("reported deltas = %+v total %d err %v, want During", deltas, total, err)
	}
}

func seedStopAndPruneRows(t *testing.T, a *app.App, rc *app.RepoContext, worktree string, bStart int64) {
	t.Helper()
	if _, err := rc.Store.DB.Exec(`UPDATE sessions SET started_at=? WHERE id='s'`, bStart); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.DB.Exec(`UPDATE sessions SET started_at=? WHERE id='s'`, bStart); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Store.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at, ended_at)
		VALUES ('A', 'test', ?, ?, ?)`, worktree, bStart-10, bStart-1); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		startedAt := bStart + 10 + int64(i)
		if _, err := rc.Store.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at, ended_at)
			VALUES (?, 'test', ?, ?, ?)`, fmt.Sprintf("noop-%d", i), worktree, startedAt, startedAt+1); err != nil {
			t.Fatal(err)
		}
	}
	latest := bStart + 100
	if _, err := rc.Store.DB.Exec(`INSERT INTO sessions(id, agent, worktree, started_at, ended_at)
		VALUES ('latest', 'test', ?, ?, ?)`, worktree, latest, latest+1); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []struct {
		id, name string
		at       int64
	}{{"delta-A", "Earlier", bStart - 5}, {"delta-B", "During", bStart + 5}} {
		if _, err := rc.Store.DB.Exec(`INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at)
			VALUES (?, ?, 'a.go', ?, 'function', 'added', ?)`, delta.id, worktree, delta.name, delta.at); err != nil {
			t.Fatal(err)
		}
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
