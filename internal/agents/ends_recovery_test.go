package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/store"
)

// journalRows returns wt's journal rows in rc by content.
func journalRows(t *testing.T, rc *app.RepoContext, wt string) []string {
	t.Helper()
	rows, err := rc.Store.DB.Query(`SELECT path, qualified_name, kind, change, at FROM graph_deltas
		WHERE worktree=? ORDER BY at, path, qualified_name, kind, change`, wt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var path, name, kind, change string
		var at int64
		if err := rows.Scan(&path, &name, &kind, &change, &at); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %d", path, name, kind, change, at))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rowsBeforeTenth counts wt's journal rows older than its 10th-most-recent session start.
func rowsBeforeTenth(t *testing.T, rc *app.RepoContext, wt string) int {
	t.Helper()
	var n int
	if err := rc.Store.DB.QueryRow(`SELECT COUNT(*) FROM graph_deltas WHERE worktree=?1 AND at <
		(SELECT started_at FROM sessions WHERE worktree=?1 ORDER BY started_at DESC LIMIT 1 OFFSET 9)`, wt).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// prunableJournal gives wt in rc a journal row older than ten ended sessions
// after it, the last of which journaled a row of its own, and returns the rows
// a prune that is not held keeps.
func prunableJournal(t *testing.T, s *scratch, rc *app.RepoContext, wt string) []string {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := rc.Store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	delta := `INSERT INTO graph_deltas(id, worktree, path, qualified_name, kind, change, at) VALUES (?, ?, 'old.go', ?, 'function', 'added', ?)`
	s.clk.Advance(time.Second)
	exec(delta, "old-"+wt, wt, "Old", s.clk.Now().UnixMilli())
	for i := range 10 {
		s.clk.Advance(time.Second)
		at := s.clk.Now().UnixMilli()
		exec(`INSERT INTO sessions(id, agent, worktree, started_at, ended_at) VALUES (?, 'test', ?, ?, ?)`,
			fmt.Sprintf("seed-%d", i), wt, at, at+500)
	}
	exec(delta, "recent-"+wt, wt, "Recent", s.clk.Now().UnixMilli()+100)
	s.clk.Advance(time.Second)
	return []string{fmt.Sprintf("old.go Recent function added %d", s.clk.Now().UnixMilli()-900)}
}

// requirePruned fails unless wt's journal in rc holds just want.
func requirePruned(t *testing.T, rc *app.RepoContext, wt string, want []string) {
	t.Helper()
	if got := journalRows(t, rc, wt); !slices.Equal(got, want) || rowsBeforeTenth(t, rc, wt) != 0 {
		t.Fatalf("journal of %s = %v; want %v", wt, got, want)
	}
}

// deadFolderEnd publishes f as a Stop that died after publishing it leaves it:
// its liveness file there and unlocked.
func deadFolderEnd(t *testing.T, fdir string, f folderEndRecord) folderEndRecord {
	t.Helper()
	k, err := createUnique(fdir, liveFilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	f.Live = filepath.Base(k.Name())
	got, created, err := claimEnd(fdir, folderEndName(gitctx.NonGitID(f.Dir), f.Session), f)
	if err != nil || !created {
		t.Fatalf("publish F: %v %v", created, err)
	}
	return got
}

// folderEndPath is the path of F of conversation session in folder.
func folderEndPath(a *app.App, folder, session string) string {
	return filepath.Join(folderEndsDir(a.Store), folderEndName(gitctx.NonGitID(folder), session))
}

func TestUnreadableSharedRecordsAffectNothing(t *testing.T) {
	run := func(t *testing.T, unreadable bool) []string {
		s := newScratch(t)
		r := s.goRepo("repo")
		in := s.open(r)
		repo := rc(t, in, r)
		index(t, repo)
		file := filepath.Join(r, "repo", "a.go")
		for i := range 11 {
			inRepoSessionWithEdit(t, s, in, i, file)
		}
		fdir := folderEndsDir(in.Store)
		bad := []string{filepath.Join(fdir, "empty"), filepath.Join(fdir, "garbage")}
		if unreadable {
			if err := os.MkdirAll(fdir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bad[0], nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bad[1], []byte("not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for i := 11; i < 17; i++ {
			inRepoSessionWithEdit(t, s, in, i, file)
		}
		rows := journalRows(t, repo, r)
		if n := rowsBeforeTenth(t, repo, r); n != 0 {
			t.Fatalf("%d journal rows older than the 10th-most-recent session start", n)
		}
		if !unreadable {
			return rows
		}

		f := s.open(s.folder, r)
		start(t, f, "fs", "startup")
		release := holdWriter(t, f.Store)
		res, at := stopScratch(t, s, f, "fs")
		release()
		if res.Failure != nil {
			t.Fatal(res.Failure)
		}
		requireSessionEnd(t, folderRC(f), "fs", false, 0)
		start(t, f, "fn", "startup")
		requireSessionEnd(t, folderRC(f), "fs", true, at)

		exists := func(want bool) {
			t.Helper()
			for _, p := range bad {
				if _, err := os.Stat(p); (err == nil) != want {
					t.Fatalf("%s present=%v, want %v", p, err == nil, want)
				}
			}
		}
		for _, p := range bad {
			age(t, s, p, EndRunLimit-time.Second)
		}
		start(t, in, "young", "startup")
		exists(true)
		for _, p := range bad {
			age(t, s, p, EndRunLimit+time.Second)
		}
		start(t, in, "old", "startup")
		exists(false)
		return rows
	}
	var control, withFiles []string
	t.Run("control", func(t *testing.T) { control = run(t, false) })
	t.Run("unreadable files", func(t *testing.T) { withFiles = run(t, true) })
	if len(control) == 0 || !slices.Equal(control, withFiles) {
		t.Fatalf("journal rows differ:\ncontrol: %v\nwith unreadable files: %v", control, withFiles)
	}
}

func TestAbandonedRepoEndInRepoSessionStart(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	release := holdWriter(t, rc(t, f, r).Store)
	_, at := stopScratch(t, s, f, "s")
	release()
	want := prunableJournal(t, s, rc(t, f, r), r)
	in := s.open(r)
	start(t, in, "in", "startup")
	requireSessionEnd(t, rc(t, in, r), "s", true, at)
	requirePruned(t, rc(t, in, r), r, want)
	requireNoPendingFiles(t, in, rc(t, in, r))
}

func TestAnotherWorktreeRecoversTheEnd(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("main")
	w := filepath.Join(s.base, "w")
	gitIn(t, r, "worktree", "add", "-q", "-b", "wbranch", w)
	f := s.open(s.folder, w)
	start(t, f, "s", "startup")
	rStore := rc(t, f, w)
	release := holdWriter(t, rStore.Store)
	_, at := stopScratch(t, s, f, "s")
	release()

	if err := os.RemoveAll(w); err != nil {
		t.Fatal(err)
	}
	s.goRepo("w")
	g := s.open(s.folder, w)
	q := rc(t, g, w)
	if q.Git.RepoID == rStore.Git.RepoID {
		t.Fatal("fixture: Q shares R's store")
	}
	index(t, q)
	start(t, g, "n", "startup")
	if res, _ := stopScratch(t, s, g, "s"); res.Failure != nil {
		t.Fatal(res.Failure)
	}
	start(t, g, "s", "resume")
	if rows := sessionRows(t, q, "s"); len(rows) != 0 {
		t.Fatalf("Q holds rows of the original conversation: %+v", rows)
	}
	requireSessionEnd(t, rStore, "s", false, 0)

	in := s.open(r)
	start(t, in, "in", "startup")
	requireSessionEnd(t, rc(t, in, r), "s", true, at)
	requireNoPendingFiles(t, in, rc(t, in, r), q)
}

// replaceable is a checkout path whose repo a test swaps.
type replaceable struct {
	t       *testing.T
	path, q string
}

// toQ moves the repo at path to path-<from> and puts a clone of q there.
func (p replaceable) toQ(from string) {
	p.t.Helper()
	if err := os.Rename(p.path, p.path+"-"+from); err != nil {
		p.t.Fatal(err)
	}
	gitIn(p.t, filepath.Dir(p.path), "clone", "-q", p.q, p.path)
}

// swap moves the repo at path to path-<to> and moves path-<from> back.
func (p replaceable) swap(to, from string) {
	p.t.Helper()
	if err := os.Rename(p.path, p.path+"-"+to); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Rename(p.path+"-"+from, p.path); err != nil {
		p.t.Fatal(err)
	}
}

func TestReplacedMainCheckoutKeepsTheEnd(t *testing.T) {
	setup := func(t *testing.T) (*scratch, replaceable, string, string) {
		s := newScratch(t)
		r := s.goRepo("r")
		q := s.goRepo("q")
		start(t, s.open(r), "warm", "startup")
		rootR, rootQ := gitctx.RootCommit(gitctx.Real{Dir: r}), gitctx.RootCommit(gitctx.Real{Dir: q})
		if rootR == rootQ {
			t.Fatal("fixture: R and Q share a root")
		}
		return s, replaceable{t, r, q}, rootR, rootQ
	}
	recordFor := func(t *testing.T, a *app.App, path, session string) repoEndRecord {
		t.Helper()
		m, ok, err := readEnd[repoEndRecord](pendingDir(rc(t, a, path)), endName(session))
		if err != nil || !ok {
			t.Fatalf("pending end of %s: %v, %v", session, ok, err)
		}
		return m
	}

	t.Run("replaced after the first Stop", func(t *testing.T) {
		s, p, rootR, _ := setup(t)
		f := s.open(s.folder, p.path)
		start(t, f, "s", "startup")
		release := holdWriter(t, rc(t, f, p.path).Store)
		_, at := stopScratch(t, s, f, "s")
		release()
		p.toQ("r")
		qa := s.open(p.path)
		if rc(t, qa, p.path).Git.RootCommit != rootR {
			t.Fatal("fixture: the identity cache was not stale")
		}
		want := prunableJournal(t, s, rc(t, qa, p.path), p.path)
		start(t, qa, "q1", "startup")
		requireSessionEnd(t, rc(t, qa, p.path), "s", false, 0)
		if m := recordFor(t, qa, p.path, "s"); m.Root != rootR || m.EndedAt != at {
			t.Fatalf("R's pending end = %+v", m)
		}
		requirePruned(t, rc(t, qa, p.path), p.path, want)
		if res, _ := stopScratch(t, s, s.open(s.folder, p.path), "s"); res.Failure != nil {
			t.Fatal(res.Failure)
		}
		requireSessionEnd(t, rc(t, qa, p.path), "s", false, 0)
		p.swap("q", "r")
		in := s.open(p.path)
		start(t, in, "back", "startup")
		requireSessionEnd(t, rc(t, in, p.path), "s", true, at)
		requireNoPendingFiles(t, in, rc(t, in, p.path))
	})

	t.Run("replaced before the first Stop", func(t *testing.T) {
		s, p, rootR, _ := setup(t)
		f := s.open(s.folder, p.path)
		start(t, f, "s", "startup")
		p.toQ("r")
		f2 := s.open(s.folder, p.path)
		res, at := stopScratch(t, s, f2, "s")
		if res.Failure != nil {
			t.Fatal(res.Failure)
		}
		requireSessionEnd(t, folderRC(f2), "s", true, at)
		if m := recordFor(t, f2, p.path, "s"); m.Root != rootR || m.EndedAt != at || m.Worktree != p.path {
			t.Fatalf("R's pending end = %+v", m)
		}
		qa := s.open(p.path)
		want := prunableJournal(t, s, rc(t, qa, p.path), p.path)
		start(t, qa, "q1", "startup")
		requireSessionEnd(t, rc(t, qa, p.path), "s", false, 0)
		requirePruned(t, rc(t, qa, p.path), p.path, want)
		p.swap("q", "r")
		in := s.open(p.path)
		start(t, in, "back", "startup")
		requireSessionEnd(t, rc(t, in, p.path), "s", true, at)
		requireNoPendingFiles(t, in, rc(t, in, p.path))
	})

	t.Run("a conversation started while Q occupies the path", func(t *testing.T) {
		s, p, rootR, rootQ := setup(t)
		p.toQ("r")
		f := s.open(s.folder, p.path)
		if rc(t, f, p.path).Git.RootCommit != rootR {
			t.Fatal("fixture: the identity cache was not stale")
		}
		start(t, f, "n", "startup")
		roster, err := readRoster(context.Background(), f.Ledger, "n")
		if err != nil || len(roster) != 1 || roster[0].root != rootQ {
			t.Fatalf("roster = %+v, %v; want Q's root %s", roster, err, rootQ)
		}
		release := holdWriter(t, rc(t, f, p.path).Store)
		_, at := stopScratch(t, s, f, "n")
		release()
		p.swap("q", "r")
		in := s.open(p.path)
		start(t, in, "r1", "startup")
		requireSessionEnd(t, rc(t, in, p.path), "n", false, 0)
		p.swap("r", "q")
		qa := s.open(p.path)
		start(t, qa, "q1", "startup")
		requireSessionEnd(t, rc(t, qa, p.path), "n", true, at)
		requireNoPendingFiles(t, qa, rc(t, qa, p.path))
	})
}

func TestBranchWithAnotherRootIsTheSameRepository(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("main")
	rootA := gitctx.RootCommit(gitctx.Real{Dir: r})
	w := filepath.Join(s.base, "w")
	gitIn(t, r, "worktree", "add", "-q", "-b", "wbranch", w)
	f := s.open(s.folder, w)
	start(t, f, "s", "startup")
	release := holdWriter(t, rc(t, f, w).Store)
	_, at := stopScratch(t, s, f, "s")
	release()
	gitIn(t, w, "checkout", "-q", "--orphan", "other")
	gitIn(t, w, "commit", "-q", "--allow-empty", "-m", "another root")
	if gitctx.RootCommit(gitctx.Real{Dir: w}) == rootA {
		t.Fatal("fixture: the orphan branch kept the root")
	}
	in := s.open(w)
	wc := rc(t, in, w)
	if !sameRepository(wc, rootA) {
		t.Fatal("a worktree on a branch with another root is not the same repository")
	}
	if held, err := endsHoldFor(context.Background(), wc)(wc.Store.DB); err != nil || !held {
		t.Fatalf("hold = %v, %v; want held", held, err)
	}
	start(t, in, "in", "startup")
	requireSessionEnd(t, wc, "s", true, at)
	q := s.goRepo("q")
	if sameRepository(rc(t, s.open(q), q), rootA) {
		t.Fatal("an unrelated repository is the same repository")
	}
}

func TestFolderDeletedDropsItsPendingEnd(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("a")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	releaseFolder := holdWriter(t, f.Store)
	releaseRepo := holdWriter(t, rc(t, f, r).Store)
	_, at := stopScratch(t, s, f, "s")
	releaseFolder()
	releaseRepo()
	if _, err := os.Stat(folderEndPath(f, s.folder, "s")); err != nil {
		t.Fatalf("fixture: no pending folder end: %v", err)
	}
	if err := os.RemoveAll(s.folder); err != nil {
		t.Fatal(err)
	}
	in := s.open(r)
	start(t, in, "in", "startup")
	requireSessionEnd(t, rc(t, in, r), "s", true, at)
	requireNoPendingFiles(t, in, rc(t, in, r))
}

func TestCheckoutGoneForGood(t *testing.T) {
	s := newScratch(t)
	ra, rb := s.goRepo("a"), s.goRepo("b")
	f := s.open(s.folder, ra, rb)
	start(t, f, "s", "startup")
	b := rc(t, f, rb)
	releaseA := holdWriter(t, rc(t, f, ra).Store)
	releaseB := holdWriter(t, b.Store)
	_, at := stopScratch(t, s, f, "s")
	releaseA()
	releaseB()
	if err := os.RemoveAll(rb); err != nil {
		t.Fatal(err)
	}
	want := prunableJournal(t, s, rc(t, f, ra), ra)
	in := s.open(ra)
	start(t, in, "in", "startup")
	requireSessionEnd(t, rc(t, in, ra), "s", true, at)
	requirePruned(t, rc(t, in, ra), ra, want)
	requireNoPendingFiles(t, in, rc(t, in, ra))
	if m, ok, err := readEnd[repoEndRecord](pendingDir(b), endName("s")); err != nil || !ok || m.EndedAt != at {
		t.Fatalf("B's pending end = %+v, %v, %v; want it kept at %d", m, ok, err, at)
	}
}

func TestRecordsCannotBeCreatedStoresFree(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	launches := 0
	f.EndLater = func(string) { launches++ }
	unwritable(t, folderEndsDir(f.Store))
	res, at := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireSessionEnd(t, rc(t, f, r), "s", true, at)
	requireEmpty(t, stopClaimsDir(f.Store))
	if !strings.Contains(failure(res), folderEndsDir(f.Store)) || launches != 0 {
		t.Fatalf("failure = %q, launches %d; want the path named and no run", failure(res), launches)
	}
}

func TestRecordsCannotBeCreatedStoreBusy(t *testing.T) {
	t.Run("folder", func(t *testing.T) {
		s := newScratch(t)
		r := s.goRepo("repo")
		f := s.open(s.folder, r)
		start(t, f, "s", "startup")
		unwritable(t, folderEndsDir(f.Store))
		release := holdWriter(t, f.Store)
		res, at := stopScratch(t, s, f, "s")
		release()
		requireSessionEnd(t, rc(t, f, r), "s", true, at)
		requireSessionEnd(t, folderRC(f), "s", false, 0)
		requireEmpty(t, stopClaimsDir(f.Store))
		if !strings.Contains(failure(res), "session end not recorded in "+s.folder) {
			t.Fatalf("failure = %q; want the folder's store named", failure(res))
		}
		_, at2 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, folderRC(f), "s", true, at2)
		requireSessionEnd(t, rc(t, f, r), "s", true, at)
		requireEmpty(t, stopClaimsDir(f.Store))
	})
	t.Run("repo", func(t *testing.T) {
		s := newScratch(t)
		r := s.goRepo("repo")
		f := s.open(s.folder, r)
		start(t, f, "s", "startup")
		unwritable(t, pendingDir(rc(t, f, r)))
		release := holdWriter(t, rc(t, f, r).Store)
		res, at := stopScratch(t, s, f, "s")
		release()
		requireSessionEnd(t, folderRC(f), "s", true, at)
		requireSessionEnd(t, rc(t, f, r), "s", false, 0)
		requireEmpty(t, stopClaimsDir(f.Store))
		if !strings.Contains(failure(res), "session end not recorded in "+r) {
			t.Fatalf("failure = %q; want the repo named", failure(res))
		}
		_, at2 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, rc(t, f, r), "s", true, at2)
		requireNoPendingFiles(t, f)
	})
}

func TestDirectWriteKeepsTheCommonTime(t *testing.T) {
	t.Run("one Stop", testDirectWriteOneStop)
	t.Run("two overlapping first Stops", testDirectWriteTwoOverlappingFirstStops)
	t.Run("a second Stop while the first is still running", testDirectWriteASecondStopWhileTheFirstIsStillRunning)
	t.Run("the next Stop after the first has returned", testDirectWriteTheNextStopAfterTheFirstHasReturned)
	t.Run("a sweep between the creator's folder commit and its repo write", testDirectWriteASweepBetweenTheCreatorSFolderCommitAndItsRepoWrite)
	t.Run("two sweeps and a new claim around one F", testDirectWriteTwoSweepsAndANewClaimAroundOneF)
	t.Run("the next Stop during another writer's removal of F", testDirectWriteTheNextStopDuringAnotherWriterSRemovalOfF)
	t.Run("an unrelated removal or lock does not make a removal miss its file", testDirectWriteAnUnrelatedRemovalOrLockDoesNotMakeARemovalMissItsFile)
	t.Run("a creator that died after publishing F", testDirectWriteACreatorThatDiedAfterPublishingF)
}

func directWriteSetup(t *testing.T) (*scratch, *app.App, string) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	return s, f, r
}

func testDirectWriteOneStop(t *testing.T) {
	s, f, r := directWriteSetup(t)
	release := holdWriter(t, rc(t, f, r).Store)
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		if isGuard(name) {
			return nil, false, errors.New("injected")
		}
		if isFolder(dir) {
			s.clk.Advance(time.Second)
			release()
		}
		return linkEnd(dir, name, data)
	})
	_, at := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireSessionEnd(t, rc(t, f, r), "s", true, at)
}

func testDirectWriteTwoOverlappingFirstStops(t *testing.T) {
	s, f, r := directWriteSetup(t)
	unwritable(t, pendingDir(rc(t, f, r)))
	inner := false
	var t2 int64
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		if isClaim(dir) && !inner {
			inner = true
			_, t2 = stopScratch(t, s, f, "s")
			inner = false
		}
		return linkEnd(dir, name, data)
	})
	stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, t2)
	requireSessionEnd(t, rc(t, f, r), "s", true, t2)
	requireNoPendingFiles(t, f)
}

func testDirectWriteASecondStopWhileTheFirstIsStillRunning(t *testing.T) {
	s, f, r := directWriteSetup(t)
	unwritable(t, pendingDir(rc(t, f, r)))
	inner := false
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		existing, created, err := linkEnd(dir, name, data)
		if isFolder(dir) && !inner {
			inner = true
			s.clk.Advance(time.Second)
			stopScratch(t, s, f, "s")
			inner = false
		}
		return existing, created, err
	})
	_, t1 := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t1)
	requireNoPendingFiles(t, f)
}

func testDirectWriteTheNextStopAfterTheFirstHasReturned(t *testing.T) {
	s, f, r := directWriteSetup(t)
	unwritable(t, pendingDir(rc(t, f, r)))
	releaseFolder := holdWriter(t, f.Store)
	releaseRepo := holdWriter(t, rc(t, f, r).Store)
	res, t1 := stopScratch(t, s, f, "s")
	if !strings.Contains(failure(res), "session end not recorded in "+r) {
		t.Fatalf("failure = %q", failure(res))
	}
	releaseFolder()
	releaseRepo()
	s.clk.Advance(100 * time.Millisecond)
	t2 := s.clk.Now().UnixMilli()
	if _, err := Handle(context.Background(), f, Event{Kind: KindStop, SessionID: "s", StopActive: true}); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t2)
	requireNoPendingFiles(t, f)
}

func testDirectWriteASweepBetweenTheCreatorSFolderCommitAndItsRepoWrite(t *testing.T) {
	s, f, r := directWriteSetup(t)
	u := s.goRepo("unrelated")
	ua := s.open(u)
	unwritable(t, pendingDir(rc(t, f, r)))
	launches := 0
	f.EndLater = func(string) { launches++ }
	folderDone := make(chan struct{})
	var once sync.Once
	inner := false
	var sweep, innerStop Result
	var sweepErr, innerErr, lockErr error
	prev := endWrite
	t.Cleanup(func() { endWrite = prev })
	endWrite = func(slot string, write func() error) error {
		if slot == s.folder {
			err := write()
			once.Do(func() { close(folderDone) })
			return err
		}
		if slot == r && !inner {
			<-folderDone
			inner = true
			sweep, sweepErr = Handle(context.Background(), ua, Event{Kind: KindSessionStart, SessionID: "u1", Source: "startup"})
			// The sweep kept F and left no lock on it.
			release, ok, err := store.TryLockFile(folderEndPath(f, s.folder, "s"))
			if err == nil && ok {
				release()
			} else {
				lockErr = fmt.Errorf("lock F after the sweep: %v, %v", ok, err)
			}
			s.clk.Advance(2 * time.Second)
			innerStop, innerErr = Handle(context.Background(), f, Event{Kind: KindStop, SessionID: "s", StopActive: true})
			inner = false
		}
		return write()
	}
	res, t1 := stopScratch(t, s, f, "s")
	if err := errors.Join(sweepErr, sweep.Failure, lockErr, innerErr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failure(res)+failure(innerStop), "not recorded") || launches != 0 {
		t.Fatalf("Stops reported %q / %q, launches %d", failure(res), failure(innerStop), launches)
	}
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t1)
	requireNoPendingFiles(t, f)
}

func testDirectWriteTwoSweepsAndANewClaimAroundOneF(t *testing.T) {
	for _, variant := range []string{"new file", "same file overwritten", "removal under way"} {
		t.Run(variant, func(t *testing.T) {
			s := newScratch(t)
			u := s.goRepo("unrelated")
			ua := s.open(u)
			fdir := folderEndsDir(ua.Store)
			gone := filepath.Join(s.base, "gone")
			old := deadFolderEnd(t, fdir, folderEndRecord{Session: "x", Dir: gone, EndedAt: 5})
			fpath := folderEndPath(ua, gone, "x")
			ctx := context.Background()
			var newF folderEndRecord
			var releaseNew func()
			var s2Err error
			inner := false
			prev := removeFile
			t.Cleanup(func() { removeFile = prev })
			removeFile = func(path string, match func(f *os.File, fi fs.FileInfo) bool) (bool, error) {
				if path != fpath || inner {
					return prev(path, match)
				}
				inner = true
				defer func() { inner = false }()
				switch variant {
				case "new file":
					s2Err = endPendingFolderEnds(ctx, ua, false)
					var created bool
					var err error
					newF, created, releaseNew, err = claimFolderEnd(fdir, folderEndRecord{Session: "x", Dir: gone, EndedAt: 6})
					if err != nil || !created {
						t.Fatalf("new claim: %v %v", created, err)
					}
				case "same file overwritten":
					k, err := createUnique(fdir, liveFilePrefix)
					if err != nil {
						t.Fatal(err)
					}
					_ = k.Close()
					unlock, ok, err := store.TryLockFile(k.Name())
					if err != nil || !ok {
						t.Fatalf("lock K: %v %v", ok, err)
					}
					releaseNew = unlock
					newF = folderEndRecord{Session: "x", Dir: gone, EndedAt: 6, Live: filepath.Base(k.Name())}
					data := fmt.Sprintf(`{"session":"x","dir":%q,"ended_at":6,"live":%q}`, gone, newF.Live)
					if err := os.WriteFile(fpath, []byte(data), 0o644); err != nil {
						t.Fatal(err)
					}
				case "removal under way":
					unlock, ok, err := store.TryLockFile(fpath)
					if err != nil || !ok {
						t.Fatalf("lock F: %v %v", ok, err)
					}
					s2Err = endPendingFolderEnds(ctx, ua, false)
					unlock()
				}
				return prev(path, match)
			}
			if err := endPendingFolderEnds(ctx, ua, false); err != nil || s2Err != nil {
				t.Fatalf("sweeps: %v, %v", err, s2Err)
			}
			if variant == "removal under way" {
				for _, p := range []string{fpath, filepath.Join(fdir, old.Live)} {
					if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("%s left: %v", p, err)
					}
				}
				return
			}
			defer releaseNew()
			got, ok, err := readEnd[folderEndRecord](fdir, filepath.Base(fpath))
			if err != nil || !ok || got.Live != newF.Live {
				t.Fatalf("F after the sweeps = %+v, %v, %v; want the new one", got, ok, err)
			}
			if running, err := fCreatorRunning(fdir, got); err != nil || !running {
				t.Fatalf("new F's creator running = %v, %v", running, err)
			}
		})
	}
}

func testDirectWriteTheNextStopDuringAnotherWriterSRemovalOfF(t *testing.T) {
	s, f, r := directWriteSetup(t)
	unwritable(t, pendingDir(rc(t, f, r)))
	releaseFolder := holdWriter(t, f.Store)
	releaseRepo := holdWriter(t, rc(t, f, r).Store)
	_, t1 := stopScratch(t, s, f, "s")
	releaseFolder()
	releaseRepo()
	fpath := folderEndPath(f, s.folder, "s")
	inner := false
	var b Result
	var t2 int64
	prev := removeFile
	t.Cleanup(func() { removeFile = prev })
	removeFile = func(path string, match func(f *os.File, fi fs.FileInfo) bool) (bool, error) {
		if path == fpath && !inner {
			inner = true
			release, ok, err := store.TryLockFile(path)
			if err != nil || !ok {
				t.Fatalf("lock F: %v %v", ok, err)
			}
			b, t2 = stopScratch(t, s, f, "s")
			release()
			inner = false
		}
		return prev(path, match)
	}
	if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failure(b), folderEndsDir(f.Store)) {
		t.Fatalf("Stop B reported %q", failure(b))
	}
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t2)
	requireNoPendingFiles(t, f)
}

func testDirectWriteAnUnrelatedRemovalOrLockDoesNotMakeARemovalMissItsFile(t *testing.T) {
	s, f, r := directWriteSetup(t)
	u := s.goRepo("unrelated")
	ua := s.open(u)
	fdir := folderEndsDir(f.Store)
	settled := func(name string) {
		t.Helper()
		dir := filepath.Join(s.base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		o := s.open(dir, r)
		start(t, o, name, "startup")
		fe := deadFolderEnd(t, fdir, folderEndRecord{Session: name, Dir: dir, EndedAt: 7})
		if err := os.Remove(filepath.Join(fdir, fe.Live)); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Store.DB.Exec(`UPDATE sessions SET ended_at=7 WHERE id=?`, name); err != nil {
			t.Fatal(err)
		}
	}
	settled("b")
	fpath := folderEndPath(f, s.folder, "s")
	inner := false
	var sweep Result
	var sweepErr error
	prev := removeFile
	t.Cleanup(func() { removeFile = prev })
	removeFile = func(path string, match func(f *os.File, fi fs.FileInfo) bool) (bool, error) {
		if path == fpath && !inner {
			inner = true
			release, ok, err := store.TryLockFile(path)
			if err != nil || !ok {
				t.Fatalf("lock F: %v %v", ok, err)
			}
			sweep, sweepErr = Handle(context.Background(), ua, Event{Kind: KindSessionStart, SessionID: "u1", Source: "startup"})
			release()
			inner = false
		}
		return prev(path, match)
	}
	res, at := stopScratch(t, s, f, "s")
	if err := errors.Join(res.Failure, sweepErr, sweep.Failure); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireNoPendingFiles(t, f)

	settled("c")
	var locks []func()
	for _, name := range []string{"junk", ".claim-young"} {
		release, ok, err := store.TryLockFile(filepath.Join(fdir, name))
		if err != nil || !ok {
			t.Fatalf("lock %s: %v %v", name, ok, err)
		}
		locks = append(locks, release)
	}
	start(t, ua, "u2", "startup")
	for _, release := range locks {
		release()
	}
	if _, err := os.Stat(folderEndPath(f, filepath.Join(s.base, "c"), "c")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("settled F left: %v", err)
	}
}

func testDirectWriteACreatorThatDiedAfterPublishingF(t *testing.T) {
	s, f, r := directWriteSetup(t)
	unwritable(t, pendingDir(rc(t, f, r)))
	s.clk.Advance(time.Second)
	t1 := s.clk.Now().UnixMilli()
	fe := deadFolderEnd(t, folderEndsDir(f.Store), folderEndRecord{Session: "s", Dir: s.folder, EndedAt: t1})
	release, ok, err := store.TryLockFile(filepath.Join(folderEndsDir(f.Store), fe.Live))
	if err != nil || !ok {
		t.Fatalf("lock K: %v %v", ok, err)
	}
	release() // as the OS does when its holder exits
	_, t2 := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t2)
	requireNoPendingFiles(t, f)
}

func TestOneBusyStoreLeavesTheOthersEnded(t *testing.T) {
	t.Run("only repo B held", func(t *testing.T) {
		s := newScratch(t)
		ra, rb := s.goRepo("a"), s.goRepo("b")
		f := s.open(s.folder, ra, rb)
		start(t, f, "s", "startup")
		launches := 0
		f.EndLater = func(string) { launches++ }
		release := holdWriter(t, rc(t, f, rb).Store)
		res, at := stopScratch(t, s, f, "s")
		if res.Failure != nil || launches != 1 {
			t.Fatalf("Stop = %v, launches %d", res.Failure, launches)
		}
		requireSessionEnd(t, folderRC(f), "s", true, at)
		requireSessionEnd(t, rc(t, f, ra), "s", true, at)
		requireSessionEnd(t, rc(t, f, rb), "s", false, 0)
		if m, ok, err := readEnd[repoEndRecord](pendingDir(rc(t, f, rb)), endName("s")); err != nil || !ok || m.EndedAt != at {
			t.Fatalf("B's pending end = %+v, %v, %v", m, ok, err)
		}
		releaseOnRetry(t, release)
		if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
			t.Fatal(err)
		}
		requireSessionEnd(t, rc(t, f, rb), "s", true, at)
		requireNoPendingFiles(t, f, rc(t, f, ra), rc(t, f, rb))
	})
	t.Run("only the folder store held", func(t *testing.T) {
		s := newScratch(t)
		ra, rb := s.goRepo("a"), s.goRepo("b")
		f := s.open(s.folder, ra, rb)
		start(t, f, "s", "startup")
		launches := 0
		f.EndLater = func(string) { launches++ }
		release := holdWriter(t, f.Store)
		res, at := stopScratch(t, s, f, "s")
		if res.Failure != nil || launches != 1 {
			t.Fatalf("Stop = %v, launches %d", res.Failure, launches)
		}
		requireSessionEnd(t, rc(t, f, ra), "s", true, at)
		requireSessionEnd(t, rc(t, f, rb), "s", true, at)
		if _, ok, err := readEnd[folderEndRecord](folderEndsDir(f.Store), folderEndName(folderID(f.Store), "s")); err != nil || !ok {
			t.Fatalf("F = %v, %v; want it kept", ok, err)
		}
		waits := releaseOnRetry(t, release)
		if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
			t.Fatal(err)
		}
		if *waits != 1 {
			t.Fatalf("background run waited %d times, want 1", *waits)
		}
		requireSessionEnd(t, folderRC(f), "s", true, at)
		requireNoPendingFiles(t, f, rc(t, f, ra), rc(t, f, rb))
	})
}

func TestOverlappingStopsRecordOneTime(t *testing.T) {
	setup := func(t *testing.T) (*scratch, *app.App, string) {
		s := newScratch(t)
		r := s.goRepo("repo")
		f := s.open(s.folder, r)
		start(t, f, "s", "startup")
		return s, f, r
	}
	t.Run("inner Stop after the outer claimed F, before its M", func(t *testing.T) {
		s, f, r := setup(t)
		inner := false
		onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
			if isFinal(dir, name) && !inner {
				inner = true
				stopScratch(t, s, f, "s")
				inner = false
			}
			return linkEnd(dir, name, data)
		})
		_, t1 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, folderRC(f), "s", true, t1)
		requireSessionEnd(t, rc(t, f, r), "s", true, t1)
		requireNoPendingFiles(t, f, rc(t, f, r))
	})
	for _, mFails := range []bool{false, true} {
		name := "inner Stop after the outer linked G, before its T"
		if mFails {
			name = "the inner Stop's M link fails"
		}
		t.Run(name, func(t *testing.T) {
			s, f, r := setup(t)
			var release func()
			if mFails {
				release = holdWriter(t, rc(t, f, r).Store)
			}
			inner := false
			var t1 int64
			onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
				if inner && mFails && isFinal(dir, name) {
					return nil, false, errors.New("injected")
				}
				existing, created, err := linkEnd(dir, name, data)
				if isGuard(name) && !inner {
					inner = true
					_, t1 = stopScratch(t, s, f, "s")
					inner = false
					s.clk.Advance(time.Second)
				}
				return existing, created, err
			})
			stopScratch(t, s, f, "s")
			requireSessionEnd(t, folderRC(f), "s", true, t1)
			if mFails {
				if m, ok, err := readEnd[repoEndRecord](pendingDir(rc(t, f, r)), endName("s")); err != nil || !ok || m.EndedAt != t1 {
					t.Fatalf("M = %+v, %v, %v; want the stamped time %d adopted", m, ok, err, t1)
				}
				releaseOnRetry(t, release)
				if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
					t.Fatal(err)
				}
			}
			requireSessionEnd(t, rc(t, f, r), "s", true, t1)
			requireNoPendingFiles(t, f, rc(t, f, r))
		})
	}
}

func TestOverlappingStopsShareTheStopClaim(t *testing.T) {
	t.Run("stores free", func(t *testing.T) { testStopClaimOverlap(t, false) })
	t.Run("folder store busy", func(t *testing.T) { testStopClaimOverlap(t, true) })
	t.Run("the shared folder takes F only from the later Stop", testStopClaimSharedFolderFromTheLaterStop)
	t.Run("a holder that dies before publishing F", testStopClaimHolderDiesBeforeF)
	t.Run("the holder links F after B read the claims", testStopClaimHolderLinksFLate)
	t.Run("a dead claim", testStopClaimDead)
}

// runStop runs a Stop of conversation s in a and sends its result.
func runStop(a *app.App, done chan<- startResult) {
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, SessionID: "s", StopActive: true})
	done <- startResult{res, err}
}

// testStopClaimOverlap runs Stop A to its M link, then Stop B to its store
// writes, then A to its end, then B, while the shared folder takes no F.
func testStopClaimOverlap(t *testing.T, folderBusy bool) {
	s, f, r := directWriteSetup(t)
	unwritable(t, folderEndsDir(f.Store))
	releaseFolder := func() {}
	if folderBusy {
		releaseFolder = holdWriter(t, f.Store)
	}
	firstM, resumeA := make(chan struct{}), make(chan struct{})
	var claimed atomic.Bool
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		if isFinal(dir, name) && claimed.CompareAndSwap(false, true) {
			close(firstM)
			<-resumeA
		}
		return linkEnd(dir, name, data)
	})
	beforeWritesB, resumeB := make(chan string, 2), make(chan struct{})
	var mu sync.Mutex
	entered := map[string]bool{}
	prev := endWrite
	t.Cleanup(func() { endWrite = prev })
	endWrite = func(slot string, write func() error) error {
		mu.Lock()
		first := !entered[slot]
		entered[slot] = true
		mu.Unlock()
		if first {
			beforeWritesB <- slot
			<-resumeB
		}
		return write()
	}
	s.clk.Advance(time.Second)
	at := s.clk.Now().UnixMilli()
	doneA, doneB := make(chan startResult, 1), make(chan startResult, 1)
	go runStop(f, doneA)
	<-firstM
	s.clk.Advance(time.Second)
	go runStop(f, doneB)
	<-beforeWritesB
	<-beforeWritesB
	close(resumeA)
	a := <-doneA
	close(resumeB)
	b := <-doneB
	if err := errors.Join(a.err, b.err); err != nil {
		t.Fatal(err)
	}
	repo := rc(t, f, r)
	requireSessionEnd(t, repo, "s", true, at)
	requireEmpty(t, stopClaimsDir(f.Store))
	requireEmpty(t, pendingDir(repo))
	if !folderBusy {
		requireSessionEnd(t, folderRC(f), "s", true, at)
		return
	}
	for _, res := range []startResult{a, b} {
		if !strings.Contains(failure(res.res), "session end not recorded in "+s.folder) {
			t.Fatalf("failure = %q; want the folder named", failure(res.res))
		}
	}
	requireSessionEnd(t, folderRC(f), "s", false, 0)
	releaseFolder()
	endWrite = prev
	s.clk.Advance(time.Second)
	t3 := s.clk.Now().UnixMilli()
	if _, err := Handle(context.Background(), f, Event{Kind: KindStop, SessionID: "s", StopActive: true}); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, folderRC(f), "s", true, t3)
}

// testStopClaimSharedFolderFromTheLaterStop pauses Stop B before its claim's
// link, runs Stop A, whose F is refused, to its M link, then lets B, which
// borrows A's claim, run to its return before A goes on.
func testStopClaimSharedFolderFromTheLaterStop(t *testing.T) {
	s, f, r := directWriteSetup(t)
	bAtClaim, resumeB := make(chan struct{}), make(chan struct{})
	aAtM, resumeA := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	counts := map[string]int{}
	times := map[string][]int64{}
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		kind := filepath.Base(dir)
		if isFinal(dir, name) {
			kind = "M"
		}
		mu.Lock()
		counts[kind]++
		n := counts[kind]
		if isFolder(dir) || kind == "M" {
			var rec struct {
				EndedAt int64 `json:"ended_at"`
			}
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Error(err)
			}
			times[kind] = append(times[kind], rec.EndedAt)
		}
		mu.Unlock()
		switch {
		case isClaim(dir) && n == 1:
			close(bAtClaim)
			<-resumeB
		case isFolder(dir) && n == 1:
			return nil, false, errors.New("injected")
		case kind == "M" && n == 1:
			close(aAtM)
			<-resumeA
		}
		return linkEnd(dir, name, data)
	})
	s.clk.Advance(time.Second)
	doneA, doneB := make(chan startResult, 1), make(chan startResult, 1)
	go runStop(f, doneB)
	<-bAtClaim
	s.clk.Advance(time.Second)
	at := s.clk.Now().UnixMilli()
	go runStop(f, doneA)
	<-aAtM
	close(resumeB)
	b := <-doneB
	close(resumeA)
	a := <-doneA
	for _, res := range []startResult{a, b} {
		if res.err != nil || strings.Contains(failure(res.res), "not recorded") {
			t.Fatalf("Stop = %v, %v", res.err, res.res.Failure)
		}
	}
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireSessionEnd(t, rc(t, f, r), "s", true, at)
	// B borrowed A's claim, so it published neither F nor M with it.
	if want := []int64{at}; !slices.Equal(times["pending-ends"], want) || !slices.Equal(times["M"], want) {
		t.Fatalf("F publications carried %v, M publications %v; want only A's, at %d", times["pending-ends"], times["M"], at)
	}
	requireNoPendingFiles(t, f, rc(t, f, r))
}

// stopClaimHolder makes the fixture of a Stop A that claimed its time and has
// tried no store yet: a guard of s covering the repo and A's held claim, with
// the folder's and the repo's liveness files held. It returns that claim and
// the function that releases its locks.
func stopClaimHolder(t *testing.T, s *scratch, f *app.App, r string) (folderEndRecord, func()) {
	t.Helper()
	repo := rc(t, f, r)
	row, _, err := folderSession(context.Background(), f.Store, "s")
	if err != nil {
		t.Fatal(err)
	}
	g := repoEndRecord{Session: "s", Agent: row.agent, Worktree: r, StartedAt: row.startedAt,
		Root: repo.Git.RootCommit, Dir: s.folder, Nonce: newNonce()}
	if _, _, err := claimEnd(pendingDir(repo), guardName("s"), g); err != nil {
		t.Fatal(err)
	}
	s.clk.Advance(time.Second)
	writes, releases := map[string]string{}, []func(){}
	for _, key := range []string{folderID(f.Store), repo.Git.RepoID} {
		name, release, err := newLiveFile(stopClaimsDir(f.Store))
		if err != nil {
			t.Fatal(err)
		}
		writes[key], releases = name, append(releases, release)
	}
	claim, created, release, err := claimFolderEnd(stopClaimsDir(f.Store), folderEndRecord{Session: "s", Dir: s.folder,
		EndedAt: s.clk.Now().UnixMilli(), Covered: []string{repo.Git.RepoID}, Writes: writes})
	if err != nil || !created {
		t.Fatalf("claim: %v %v", created, err)
	}
	return claim, func() {
		release()
		for _, r := range releases {
			r()
		}
	}
}

// requireFolderPendingThenEnded checks a Stop that left the folder's end pending
// at at, then lets the held claim's holder die, frees the folder's store and
// runs the background run and a session start in the folder.
func requireFolderPendingThenEnded(t *testing.T, f *app.App, r string, res Result, launches int, at int64, die ...func()) {
	t.Helper()
	if failure(res) != "" || launches != 0 {
		t.Fatalf("Stop failure = %q, launches %d; want none", failure(res), launches)
	}
	repo := rc(t, f, r)
	requireSessionEnd(t, repo, "s", true, at)
	requireSessionEnd(t, folderRC(f), "s", false, 0)
	for _, d := range die {
		d()
	}
	if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
		t.Fatal(err)
	}
	requireSessionEnd(t, folderRC(f), "s", true, at)
	start(t, f, "next", "startup")
	requireNoPendingFiles(t, f, repo)
}

// testStopClaimHolderDiesBeforeF runs Stop B while the claim's holder still
// tries both stores, then lets the holder die before it linked F.
func testStopClaimHolderDiesBeforeF(t *testing.T) {
	s, f, r := directWriteSetup(t)
	repo := rc(t, f, r)
	launches := 0
	f.EndLater = func(string) { launches++ }
	_, releaseClaim := stopClaimHolder(t, s, f, r)
	releaseFolder := holdWriter(t, f.Store)
	res, _ := stopScratch(t, s, f, "s")
	if failure(res) != "" || launches != 0 {
		t.Fatalf("Stop failure = %q, launches %d; want none", failure(res), launches)
	}
	// B borrowed the claim: it recorded nothing with its time.
	if _, ok, err := readEnd[folderEndRecord](folderEndsDir(f.Store), folderEndName(folderID(f.Store), "s")); ok || err != nil {
		t.Fatalf("F = %v, %v; want none", ok, err)
	}
	if _, ok, err := readEnd[repoEndRecord](pendingDir(repo), endName("s")); ok || err != nil {
		t.Fatalf("M = %v, %v; want none", ok, err)
	}
	requireSessionEnd(t, folderRC(f), "s", false, 0)
	requireSessionEnd(t, repo, "s", false, 0)
	releaseClaim()
	releaseFolder()
	_, at := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireSessionEnd(t, repo, "s", true, at)
	requireNoPendingFiles(t, f, repo)
}

// testStopClaimHolderLinksFLate links the claim holder's F at Stop B's first
// store write, after B read the claims.
func testStopClaimHolderLinksFLate(t *testing.T) {
	s, f, r := directWriteSetup(t)
	launches := 0
	f.EndLater = func(string) { launches++ }
	claim, releaseClaim := stopClaimHolder(t, s, f, r)
	releaseFolder := holdWriter(t, f.Store)
	var link sync.Once
	releaseF := func() {}
	prev := endWrite
	t.Cleanup(func() { endWrite = prev })
	endWrite = func(slot string, write func() error) error {
		link.Do(func() {
			hf := claim
			hf.Writes = nil
			_, created, release, err := claimFolderEnd(folderEndsDir(f.Store), hf)
			if err != nil || !created {
				t.Errorf("the holder's F: %v %v", created, err)
				return
			}
			releaseF = release
		})
		return write()
	}
	res, _ := stopScratch(t, s, f, "s")
	endWrite = prev
	requireFolderPendingThenEnded(t, f, r, res, launches, claim.EndedAt, releaseF, releaseClaim, releaseFolder)
}

func testStopClaimDead(t *testing.T) {
	s, f, r := directWriteSetup(t)
	cdir := stopClaimsDir(f.Store)
	s.clk.Advance(time.Second)
	dead := folderEndRecord{Session: "s", Dir: s.folder, EndedAt: s.clk.Now().UnixMilli()}
	deadFolderEnd(t, cdir, dead)
	start(t, f, "other", "startup")
	requireEmpty(t, cdir)
	requireSessionEnd(t, folderRC(f), "s", false, 0)
	deadFolderEnd(t, cdir, dead)
	_, at := stopScratch(t, s, f, "s")
	requireSessionEnd(t, folderRC(f), "s", true, at)
	requireSessionEnd(t, rc(t, f, r), "s", true, at)
	requireNoPendingFiles(t, f, rc(t, f, r))
}

// The Stops of a borrowedClaim run, by the phase the test lets run.
const (
	stopA int32 = iota
	stopB
	stopC
)

// How Stop B's first write of a slot is held: before its write function runs,
// after it returned, or not at all once it returned.
const (
	holdBefore = iota
	holdAfter
	runThrough
)

// gate holds a goroutine at a point until the test opens it.
type gate struct{ at, open chan struct{} }

func newGate() gate { return gate{make(chan struct{}), make(chan struct{})} }

func (g gate) pass() {
	close(g.at)
	<-g.open
}

// borrowedClaim is Stop A, by default paused right after its F° link, and Stop
// B, a second later, borrowing A's claim. Only the Stop the test let run
// publishes or writes, so the phase tells whose hook runs.
type borrowedClaim struct {
	s       *scratch
	f       *app.App
	r, r2   string // the served repos; r2 only when twoRepos is set
	repo    *app.RepoContext
	repo2   *app.RepoContext
	phase   atomic.Int32
	twoRepo bool

	failGuard    [3]bool // per Stop: its guard link fails
	failF        [3]bool // per Stop: its F publication fails
	noClaimPause bool    // A does not pause after its F° link
	aBefore      map[string]gate
	aAfter       map[string]chan error
	bHold        map[string]int
	bSecond      func(slot string, write func() error) error // B's later writes; nil runs them
	cWrite       func(slot string, write func() error) error // C's writes
	aReturned    atomic.Bool
	bClaimsLate  atomic.Int32 // B's F° links after A returned
	bRecords     atomic.Int32 // B's F and M publications

	tA, tB           int64
	aPaused, resumeA chan struct{}
	bSlots           chan string
	resumeB          map[string]chan struct{}
	bReleased        map[string]bool
	doneA, doneB     chan startResult
	mu               sync.Mutex
	bEntered         map[string]bool
}

// newBorrowedClaim sets up the served repos, the hooks, and the writers held as
// asked. Every guard link fails unless c changes it.
func newBorrowedClaim(t *testing.T, c *borrowedClaim) *borrowedClaim {
	if c.twoRepo {
		c.s = newScratch(t)
		c.r, c.r2 = c.s.goRepo("x"), c.s.goRepo("y")
		c.f = c.s.open(c.s.folder, c.r, c.r2)
		start(t, c.f, "s", "startup")
		c.repo2 = rc(t, c.f, c.r2)
	} else {
		c.s, c.f, c.r = directWriteSetup(t)
	}
	c.repo = rc(t, c.f, c.r)
	c.aPaused, c.resumeA = make(chan struct{}), make(chan struct{})
	c.resumeB, c.bReleased = map[string]chan struct{}{}, map[string]bool{}
	for _, slot := range []string{c.s.folder, c.r, c.r2} {
		c.resumeB[slot] = make(chan struct{})
	}
	c.bSlots = make(chan string, 4)
	c.doneA, c.doneB = make(chan startResult, 1), make(chan startResult, 1)
	c.bEntered = map[string]bool{}
	onPublish(t, c.publish)
	prev := endWrite
	endWrite = c.write
	t.Cleanup(func() { endWrite = prev })
	return c
}

func (c *borrowedClaim) publish(dir, name string, data []byte) ([]byte, bool, error) {
	p := c.phase.Load()
	if p == stopB && (isFolder(dir) || isFinal(dir, name)) {
		c.bRecords.Add(1)
	}
	switch {
	case isGuard(name) && c.failGuard[p]:
		return nil, false, errors.New("injected guard failure")
	case isFolder(dir) && c.failF[p]:
		return nil, false, errors.New("injected F failure")
	case isClaim(dir) && p == stopB && c.aReturned.Load():
		c.bClaimsLate.Add(1)
	}
	existing, created, err := linkEnd(dir, name, data)
	if isClaim(dir) && created && p == stopA && !c.noClaimPause {
		close(c.aPaused)
		<-c.resumeA
	}
	return existing, created, err
}

func (c *borrowedClaim) write(slot string, write func() error) error {
	switch c.phase.Load() {
	case stopA:
		return c.writeA(slot, write)
	case stopC:
		return c.cWrite(slot, write)
	}
	c.mu.Lock()
	first := !c.bEntered[slot]
	c.bEntered[slot] = true
	c.mu.Unlock()
	if !first {
		if c.bSecond != nil {
			return c.bSecond(slot, write)
		}
		return write()
	}
	c.mu.Lock()
	resume := c.resumeB[slot]
	c.mu.Unlock()
	if c.bHold[slot] == holdBefore {
		c.bSlots <- slot
		<-resume
		return write()
	}
	err := write()
	c.bSlots <- slot
	if c.bHold[slot] == holdAfter {
		<-resume
	}
	return err
}

// writeA runs one of A's writes through the gate before it and the signal
// after it that the test set for its slot, each once.
func (c *borrowedClaim) writeA(slot string, write func() error) error {
	c.mu.Lock()
	before, hold := c.aBefore[slot]
	delete(c.aBefore, slot)
	after := c.aAfter[slot]
	delete(c.aAfter, slot)
	c.mu.Unlock()
	if hold {
		before.pass()
	}
	err := write()
	if after != nil {
		after <- err
	}
	return err
}

// gateA holds A's writes of slots before they run, and returns their gates.
func (c *borrowedClaim) gateA(slots ...string) map[string]gate {
	gates := map[string]gate{}
	c.aBefore = map[string]gate{}
	for _, slot := range slots {
		gates[slot] = newGate()
		c.aBefore[slot] = gates[slot]
	}
	return gates
}

// signalA returns the channel that gets the outcome of A's write of slot.
func (c *borrowedClaim) signalA(slot string) chan error {
	if c.aAfter == nil {
		c.aAfter = map[string]chan error{}
	}
	c.aAfter[slot] = make(chan error, 1)
	return c.aAfter[slot]
}

// runA starts A one second later.
func (c *borrowedClaim) runA() {
	c.s.clk.Advance(time.Second)
	c.tA = c.s.clk.Now().UnixMilli()
	go runStop(c.f, c.doneA)
}

// runB starts B one second later and waits until n of its slots reached their hold.
func (c *borrowedClaim) runB(n int) {
	c.phase.Store(stopB)
	c.s.clk.Advance(time.Second)
	c.tB = c.s.clk.Now().UnixMilli()
	go runStop(c.f, c.doneB)
	for range n {
		<-c.bSlots
	}
}

// open runs A to its pause after its F° link, then starts B and waits until n
// of B's slots reached their hold.
func (c *borrowedClaim) open(n int) {
	c.runA()
	<-c.aPaused
	c.runB(n)
}

// finishA lets A run to its return.
func (c *borrowedClaim) finishA() startResult {
	c.phase.Store(stopA)
	close(c.resumeA)
	a := <-c.doneA
	c.aReturned.Store(true)
	return a
}

// releaseB lets B's held write of slot go on.
func (c *borrowedClaim) releaseB(slot string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.bReleased[slot] {
		c.bReleased[slot] = true
		close(c.resumeB[slot])
	}
}

// finishB lets B run to its return.
func (c *borrowedClaim) finishB() startResult {
	c.phase.Store(stopB)
	for _, slot := range []string{c.s.folder, c.r, c.r2} {
		c.releaseB(slot)
	}
	return <-c.doneB
}

// require checks that no Stop failed, that B named no store as not recorded,
// that the stores ended at folderAt and repoAt, and that no record remains.
func (c *borrowedClaim) require(t *testing.T, folderAt, repoAt int64, b startResult, others ...startResult) {
	t.Helper()
	for _, res := range append(others, b) {
		if res.err != nil {
			t.Fatal(res.err)
		}
	}
	if strings.Contains(failure(b.res), "not recorded") {
		t.Fatalf("B's failure = %q; want no store named", failure(b.res))
	}
	requireSessionEnd(t, folderRC(c.f), "s", true, folderAt)
	requireSessionEnd(t, c.repo, "s", true, repoAt)
	requireNoPendingFiles(t, c.f, c.repo)
}

// requireNamed fails unless res names exactly the stores named as not recorded.
func (c *borrowedClaim) requireNamed(t *testing.T, res startResult, folder, repo bool) {
	t.Helper()
	got := failure(res.res)
	if strings.Contains(got, "session end not recorded in "+c.s.folder) != folder ||
		strings.Contains(got, "session end not recorded in "+c.r) != repo {
		t.Fatalf("A's failure = %q; want folder named %v, repo named %v", got, folder, repo)
	}
}

// held takes the folder's writer, and the repo's when repo is set, and returns
// the function that releases them.
func (c *borrowedClaim) held(t *testing.T, repo bool) func() {
	release := []func(){holdWriter(t, c.f.Store)}
	if repo {
		release = append(release, holdWriter(t, c.repo.Store))
	}
	return func() {
		for _, r := range release {
			r()
		}
	}
}

func TestBorrowedClaimAfterItsCreatorReturned(t *testing.T) {
	t.Run("a newer Stop claims first", testBorrowedNewerStop)
	t.Run("no newer Stop", func(t *testing.T) { testBorrowedNoNewerStop(t, false) })
	t.Run("no newer Stop, B's own guard", func(t *testing.T) { testBorrowedNoNewerStop(t, true) })
	t.Run("slots straddle A's return", testBorrowedSlotsStraddle)
	t.Run("a takeover after the repo is settled", testBorrowedRepoSettled)
	t.Run("a late F before the borrower decides again", testBorrowedLateF)
	t.Run("a late F that does not cover B's own guard", testBorrowedLateFUncovered)
	t.Run("a borrower publishes nothing from the claim", testBorrowedPublishesNothing)
	t.Run("the creator has stopped trying a store", func(t *testing.T) {
		t.Run("before the borrower's first decision", func(t *testing.T) { testBorrowedCreatorStopped(t, false) })
		t.Run("between the borrower's two decisions", func(t *testing.T) { testBorrowedCreatorStopped(t, true) })
	})
	t.Run("the creator records a time between the borrower's read and its probe", testBorrowedRecordBeforeProbe)
	t.Run("a busy store does not delay another store's retry", testBorrowedBusyStoreApart)
	t.Run("the creator records a time and still tries the store at B's deadline", testBorrowedRecordedAtDeadline)
}

// testBorrowedRecordedAtDeadline lets A publish F and M and write the folder
// after B's first decisions, while A still holds its repo lock when B's wait
// for it ends.
func testBorrowedRecordedAtDeadline(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{})
	gates := c.gateA(c.r)
	folderTried := c.signalA(c.s.folder)
	c.bHold = map[string]int{c.s.folder: holdAfter, c.r: holdAfter}
	c.open(2)
	c.phase.Store(stopA)
	close(c.resumeA)
	if err := <-folderTried; err != nil {
		t.Fatalf("A's folder write = %v; want nil", err)
	}
	<-gates[c.r].at
	b := c.finishB()
	requireSessionEnd(t, c.repo, "s", true, c.tA)
	close(gates[c.r].open)
	a := <-c.doneA
	c.aReturned.Store(true)
	c.requireNamed(t, a, false, false)
	c.require(t, c.tA, c.tA, b, a)
}

func testBorrowedNewerStop(t *testing.T) {
	cFolder, resumeC := make(chan struct{}), make(chan struct{})
	cRepo := make(chan error, 1)
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, true, false}})
	c.cWrite = func(slot string, write func() error) error {
		if slot == c.r {
			err := write()
			cRepo <- err
			return err
		}
		close(cFolder)
		<-resumeC
		return write()
	}
	unwritable(t, folderEndsDir(c.f.Store))
	release := c.held(t, true)
	c.open(2)
	a := c.finishA()
	c.requireNamed(t, a, true, true)
	release()
	c.phase.Store(stopC)
	c.s.clk.Advance(time.Second)
	tC := c.s.clk.Now().UnixMilli()
	doneC := make(chan startResult, 1)
	go runStop(c.f, doneC)
	<-cFolder
	if err := <-cRepo; err != nil {
		t.Fatal(err)
	}
	b := c.finishB()
	close(resumeC)
	c.require(t, tC, tC, b, a, <-doneC)
}

func testBorrowedNoNewerStop(t *testing.T, ownGuard bool) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, !ownGuard, false}})
	unwritable(t, folderEndsDir(c.f.Store))
	release := c.held(t, true)
	c.open(2)
	a := c.finishA()
	c.requireNamed(t, a, true, true)
	release()
	c.require(t, c.tB, c.tB, c.finishB(), a)
}

func testBorrowedSlotsStraddle(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, true, false}})
	c.bHold = map[string]int{c.s.folder: holdAfter}
	unwritable(t, folderEndsDir(c.f.Store))
	release := c.held(t, true)
	c.open(2)
	a := c.finishA()
	c.requireNamed(t, a, true, true)
	release()
	c.require(t, c.tB, c.tB, c.finishB(), a)
}

func testBorrowedRepoSettled(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{noClaimPause: true})
	gates := c.gateA(c.s.folder, c.r)
	c.bHold = map[string]int{c.s.folder: holdAfter, c.r: holdAfter}
	unwritable(t, folderEndsDir(c.f.Store))
	release := c.held(t, false)
	c.runA()
	for _, g := range gates {
		<-g.at
	}
	c.runB(2)
	requireSessionEnd(t, c.repo, "s", true, c.tA)
	for _, g := range gates {
		close(g.open)
	}
	a := <-c.doneA
	c.aReturned.Store(true)
	c.requireNamed(t, a, true, false)
	release()
	c.require(t, c.tB, c.tA, c.finishB(), a)
}

func testBorrowedLateF(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, true, false}})
	c.bHold = map[string]int{c.s.folder: holdAfter, c.r: holdAfter}
	release := c.held(t, false)
	c.open(2)
	a := c.finishA()
	release()
	b := c.finishB()
	if n := c.bClaimsLate.Load(); n != 0 {
		t.Fatalf("B linked %d claims after A returned; want none", n)
	}
	c.require(t, c.tA, c.tA, b, a)
}

func testBorrowedLateFUncovered(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, false, false}})
	repoDone := make(chan struct{})
	c.bHold = map[string]int{c.s.folder: holdAfter, c.r: holdAfter}
	c.bSecond = func(slot string, write func() error) error {
		if slot == c.r {
			defer close(repoDone)
			return write()
		}
		<-repoDone
		return write()
	}
	release := c.held(t, true)
	c.open(2)
	a := c.finishA()
	c.requireNamed(t, a, false, true)
	release()
	c.require(t, c.tA, c.tB, c.finishB(), a)
}

// testBorrowedPublishesNothing lets B run to its return while A, whose F
// cannot be published, is paused after its claim; C then claims anew.
func testBorrowedPublishesNothing(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, true, false}, failF: [3]bool{true, false, false}})
	c.cWrite = func(_ string, write func() error) error { return write() }
	c.bHold = map[string]int{c.s.folder: runThrough, c.r: runThrough}
	release := c.held(t, true)
	c.open(2)
	b := <-c.doneB
	if n := c.bRecords.Load(); n != 0 {
		t.Fatalf("B published %d F or M records; want none", n)
	}
	a := c.finishA()
	c.requireNamed(t, a, true, true)
	release()
	c.phase.Store(stopC)
	c.s.clk.Advance(time.Second)
	tC := c.s.clk.Now().UnixMilli()
	doneC := make(chan startResult, 1)
	go runStop(c.f, doneC)
	c.require(t, tC, tC, b, a, <-doneC)
}

// testBorrowedCreatorStopped runs B after A's repo write found the store busy,
// or, between, after B's first decision, while A still tries the folder.
func testBorrowedCreatorStopped(t *testing.T, between bool) {
	c := newBorrowedClaim(t, &borrowedClaim{noClaimPause: true})
	unwritable(t, folderEndsDir(c.f.Store))
	unwritable(t, pendingDir(c.repo))
	releaseRepo := holdWriter(t, c.repo.Store)
	slots := []string{c.s.folder}
	if between {
		slots = append(slots, c.r)
	}
	gates := c.gateA(slots...)
	repoTried := c.signalA(c.r)
	c.bHold = map[string]int{c.s.folder: runThrough, c.r: runThrough}
	c.runA()
	if between {
		c.bHold[c.r] = holdAfter
		<-gates[c.r].at
		<-gates[c.s.folder].at
		c.runB(2)
		close(gates[c.r].open)
	} else {
		<-gates[c.s.folder].at
	}
	if err := <-repoTried; !store.IsBusy(err) {
		t.Fatalf("A's repo write = %v; want busy", err)
	}
	releaseRepo()
	var b startResult
	if between {
		b = c.finishB()
	} else {
		c.runB(2)
		b = <-c.doneB
	}
	requireSessionEnd(t, c.repo, "s", true, c.tB)
	close(gates[c.s.folder].open)
	a := <-c.doneA
	c.requireNamed(t, a, false, false)
	c.require(t, c.tA, c.tB, b, a)
}

// testBorrowedRecordBeforeProbe runs A's F publication and its busy folder
// write between B's first read of the records and its probe of A's folder lock.
func testBorrowedRecordBeforeProbe(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{failGuard: [3]bool{true, true, false}})
	gates := c.gateA(c.r)
	folderTried := c.signalA(c.s.folder)
	c.bHold = map[string]int{c.s.folder: runThrough, c.r: holdBefore}
	releaseFolder := c.held(t, false)
	probed, goOn := make(chan struct{}), make(chan struct{})
	var once sync.Once
	prev := creatorTries
	t.Cleanup(func() { creatorTries = prev })
	creatorTries = func(fdir string, f folderEndRecord, key string) (bool, error) {
		if key == folderID(c.f.Store) {
			once.Do(func() {
				close(probed)
				<-goOn
			})
		}
		return prev(fdir, f, key)
	}
	c.open(1)
	<-probed
	c.phase.Store(stopA)
	close(c.resumeA)
	if err := <-folderTried; !store.IsBusy(err) {
		t.Fatalf("A's folder write = %v; want busy", err)
	}
	<-gates[c.r].at
	releaseFolder()
	close(goOn)
	<-c.bSlots
	requireSessionEnd(t, folderRC(c.f), "s", true, c.tA)
	close(gates[c.r].open)
	a := <-c.doneA
	c.aReturned.Store(true)
	c.requireNamed(t, a, false, false)
	c.require(t, c.tA, c.tA, c.finishB(), a)
}

// testBorrowedBusyStoreApart holds B's write of repo Y through its window while
// A stops trying repo X, and checks that B ends X meanwhile.
func testBorrowedBusyStoreApart(t *testing.T) {
	c := newBorrowedClaim(t, &borrowedClaim{twoRepo: true, noClaimPause: true})
	unwritable(t, folderEndsDir(c.f.Store))
	unwritable(t, pendingDir(c.repo))
	unwritable(t, pendingDir(c.repo2))
	releaseX := holdWriter(t, c.repo.Store)
	gates := c.gateA(c.s.folder, c.r, c.r2)
	xTried := c.signalA(c.r)
	xDone := make(chan error, 1)
	c.bSecond = func(slot string, write func() error) error {
		err := write()
		if slot == c.r {
			xDone <- err
		}
		return err
	}
	c.bHold = map[string]int{c.r2: holdBefore, c.s.folder: runThrough, c.r: holdAfter}
	c.runA()
	for _, g := range gates {
		<-g.at
	}
	c.runB(3)
	close(gates[c.r].open)
	if err := <-xTried; !store.IsBusy(err) {
		t.Fatalf("A's write of X = %v; want busy", err)
	}
	releaseX()
	c.releaseB(c.r)
	if err := await(t, "B's second write of X while its write of Y is held", xDone); err != nil {
		t.Fatal(err)
	}
	// B's write of Y is still held.
	requireSessionEnd(t, c.repo, "s", true, c.tB)
	close(gates[c.s.folder].open)
	close(gates[c.r2].open)
	a := <-c.doneA
	c.aReturned.Store(true)
	b := c.finishB()
	for _, res := range []startResult{a, b} {
		if res.err != nil || strings.Contains(failure(res.res), "not recorded") {
			t.Fatalf("Stop = %v, %q; want no store named", res.err, failure(res.res))
		}
	}
	requireSessionEnd(t, folderRC(c.f), "s", true, c.tA)
	requireSessionEnd(t, c.repo2, "s", true, c.tA)
	requireNoPendingFiles(t, c.f, c.repo, c.repo2)
}

func TestLateGuardGetsTheRunningStopsTime(t *testing.T) {
	t.Run("stores free", func(t *testing.T) { testLateGuard(t, false) })
	t.Run("repo store held through both Stops", func(t *testing.T) { testLateGuard(t, true) })
}

// testLateGuard runs Stop B inside Stop A's publication of F, after A's guard
// link failed and a prune ran: B links the guard A's claim does not cover.
func testLateGuard(t *testing.T, held bool) {
	s, f, r := directWriteSetup(t)
	in := s.open(r)
	inner := false
	var t2, pruneAt int64
	var b Result
	release := func() {}
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		if isGuard(name) && !inner {
			return nil, false, errors.New("injected")
		}
		existing, created, err := linkEnd(dir, name, data)
		if isFolder(dir) && !inner {
			inner = true
			s.clk.Advance(time.Second)
			pruneAt = s.clk.Now().UnixMilli()
			start(t, in, "p", "startup")
			if held {
				release = holdWriter(t, rc(t, f, r).Store)
			}
			b, t2 = stopScratch(t, s, f, "s")
			inner = false
		}
		return existing, created, err
	})
	a, t1 := stopScratch(t, s, f, "s")
	if t1 >= pruneAt || pruneAt >= t2 {
		t.Fatalf("fixture times %d, %d, %d", t1, pruneAt, t2)
	}
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	repo := rc(t, f, r)
	if !held {
		requireSessionEnd(t, repo, "s", true, t1)
		requireNoPendingFiles(t, f, repo)
		return
	}
	for _, res := range []Result{a, b} {
		if !strings.Contains(failure(res), "session end not recorded in "+r) {
			t.Fatalf("failure = %q; want the repo named", failure(res))
		}
	}
	requireEmpty(t, folderEndsDir(f.Store))
	g, gok, gerr := readEnd[repoEndRecord](pendingDir(repo), guardName("s"))
	_, mok, merr := readEnd[repoEndRecord](pendingDir(repo), endName("s"))
	if gerr != nil || merr != nil || !gok || g.EndedAt != 0 || mok {
		t.Fatalf("guard = %+v, %v, %v; M %v, %v; want B's guard alone, with no time", g, gok, gerr, mok, merr)
	}
	release()
	_, t3 := stopScratch(t, s, f, "s")
	requireSessionEnd(t, repo, "s", true, t3)
	requireNoPendingFiles(t, f, repo)
}

func TestRecoveryFinishesARecordedTime(t *testing.T) {
	setup := func(t *testing.T) (*scratch, *app.App, string) {
		s := newScratch(t)
		r := s.goRepo("repo")
		f := s.open(s.folder, r)
		start(t, f, "s", "startup")
		return s, f, r
	}
	for _, writer := range []string{"next Stop", "in-repo start"} {
		t.Run("M publication fails for the whole first Stop/"+writer, func(t *testing.T) {
			s, f, r := setup(t)
			release := holdWriter(t, rc(t, f, r).Store)
			failing := true
			onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
				if failing && isFinal(dir, name) {
					return nil, false, errors.New("injected")
				}
				return linkEnd(dir, name, data)
			})
			res, t1 := stopScratch(t, s, f, "s")
			failing = false
			if res.Failure != nil {
				t.Fatal(res.Failure)
			}
			requireSessionEnd(t, folderRC(f), "s", true, t1)
			if g, ok, err := readEnd[repoEndRecord](pendingDir(rc(t, f, r)), guardName("s")); err != nil || !ok || g.EndedAt != t1 {
				t.Fatalf("guard = %+v, %v, %v; want stamped at %d", g, ok, err, t1)
			}
			requireNoPendingFiles(t, f)
			release()
			if writer == "next Stop" {
				stopScratch(t, s, f, "s")
			} else {
				start(t, s.open(r), "in", "startup")
			}
			requireSessionEnd(t, rc(t, f, r), "s", true, t1)
			requireNoPendingFiles(t, f, rc(t, f, r))
		})
	}

	t.Run("both M and the stamp fail", func(t *testing.T) {
		s, f, r := setup(t)
		pdir := pendingDir(rc(t, f, r))
		release := holdWriter(t, rc(t, f, r).Store)
		onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
			if isFinal(dir, name) {
				if err := os.Rename(pdir, pdir+"-aside"); err != nil {
					t.Fatal(err)
				}
				unwritable(t, pdir)
				return nil, false, errors.New("injected")
			}
			return linkEnd(dir, name, data)
		})
		res, t1 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, folderRC(f), "s", true, t1)
		if !strings.Contains(failure(res), "session end not recorded in "+r) {
			t.Fatalf("failure = %q", failure(res))
		}
		requireNoPendingFiles(t, f)
		release()
		_, t2 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, rc(t, f, r), "s", true, t2)
	})

	publish := publishDeadEnd
	t.Run("a publisher that died after F", func(t *testing.T) {
		s, f, r := setup(t)
		fe := publish(t, s, f, r, "s")
		start(t, s.open(r), "in", "startup")
		requireSessionEnd(t, rc(t, f, r), "s", true, fe.EndedAt)
		if err := EndRosterSessions(context.Background(), f, "s"); err != nil {
			t.Fatal(err)
		}
		requireSessionEnd(t, folderRC(f), "s", true, fe.EndedAt)
		requireNoPendingFiles(t, f, rc(t, f, r))
	})

	t.Run("a writer killed after committing the folder row", func(t *testing.T) {
		s, f, r := setup(t)
		fe := publish(t, s, f, r, "s")
		if _, err := f.Store.DB.Exec(`UPDATE sessions SET ended_at=? WHERE id='s'`, fe.EndedAt); err != nil {
			t.Fatal(err)
		}
		u := s.goRepo("unrelated")
		start(t, s.open(u), "u", "startup")
		requireNoPendingFiles(t, f)
		start(t, s.open(r), "in", "startup")
		requireSessionEnd(t, rc(t, f, r), "s", true, fe.EndedAt)
		requireNoPendingFiles(t, f, rc(t, f, r))
	})

	t.Run("an F abandoned with its folder row open", func(t *testing.T) {
		s, f, r := setup(t)
		fe := publish(t, s, f, r, "s")
		u := s.goRepo("unrelated")
		ua := s.open(u)
		fpath := folderEndPath(f, s.folder, "s")
		age(t, s, fpath, EndRunLimit-time.Second)
		start(t, ua, "young", "startup")
		requireSessionEnd(t, folderRC(f), "s", false, 0)
		if _, err := os.Stat(fpath); err != nil {
			t.Fatalf("young F removed: %v", err)
		}
		age(t, s, fpath, EndRunLimit+time.Second)
		start(t, ua, "old", "startup")
		requireSessionEnd(t, folderRC(f), "s", true, fe.EndedAt)
		requireNoPendingFiles(t, f)
	})

	t.Run("a covered guard whose time cannot be copied", testCoveredGuardTimeNotCopied)
}

// publishDeadEnd writes a plain guard of session and an F covering it, as a
// Stop that died after publishing F leaves them, and returns F.
func publishDeadEnd(t *testing.T, s *scratch, f *app.App, r, session string) folderEndRecord {
	t.Helper()
	row, _, err := folderSession(context.Background(), f.Store, session)
	if err != nil {
		t.Fatal(err)
	}
	repo := rc(t, f, r)
	roster, err := readRoster(context.Background(), f.Ledger, session)
	if err != nil {
		t.Fatal(err)
	}
	var mark sql.NullInt64
	for _, e := range roster {
		if e.wt == r {
			mark = e.blockMark
		}
	}
	g := repoEndRecord{Session: session, Agent: row.agent, Worktree: r, StartedAt: row.startedAt,
		Root: repo.Git.RootCommit, Dir: s.folder, Nonce: newNonce(), BlockMark: mark}
	if _, _, err := claimEnd(pendingDir(repo), guardName(session), g); err != nil {
		t.Fatal(err)
	}
	s.clk.Advance(time.Second)
	return deadFolderEnd(t, folderEndsDir(f.Store), folderEndRecord{Session: session, Dir: s.folder,
		EndedAt: s.clk.Now().UnixMilli(), Covered: []string{repo.Git.RepoID}})
}

// failCopies makes every copy of a time into pdir fail, as a folder that takes
// no file would, even for root: the M link puts a regular file in the folder's
// place, so the stamp fails too. restore puts the folder back.
func failCopies(t *testing.T, pdir string) (restore func()) {
	t.Helper()
	aside := pdir + "-aside"
	onPublish(t, func(dir, name string, data []byte) ([]byte, bool, error) {
		if dir == pdir && isFinal(dir, name) {
			if fi, err := os.Stat(pdir); err == nil && fi.IsDir() {
				if err := os.Rename(pdir, aside); err != nil {
					t.Fatal(err)
				}
				unwritable(t, pdir)
			}
			return nil, false, errors.New("injected")
		}
		return linkEnd(dir, name, data)
	})
	return func() {
		t.Helper()
		if err := os.Remove(pdir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(aside, pdir); err != nil {
			t.Fatal(err)
		}
	}
}

// requireCopyFailure fails unless failure names the failed copy for worktree r.
func requireCopyFailure(t *testing.T, failure, r string) {
	t.Helper()
	if !strings.Contains(failure, "record the pending session end for "+r) {
		t.Fatalf("failure = %q; want the failed copy for %s named", failure, r)
	}
}

func testCoveredGuardTimeNotCopied(t *testing.T) {
	ctx := context.Background()
	t.Run("in-repo start", func(t *testing.T) {
		s, f, r := directWriteSetup(t)
		fe := publishDeadEnd(t, s, f, r, "s")
		repo := rc(t, f, r)
		restore := failCopies(t, pendingDir(repo))
		in := s.open(r)
		res, err := Handle(ctx, in, Event{Kind: KindSessionStart, SessionID: "in", Source: "startup"})
		if err != nil {
			t.Fatal(err)
		}
		requireCopyFailure(t, failure(res), r)
		requireSessionEnd(t, repo, "s", true, fe.EndedAt)
		restore()
		start(t, in, "in2", "startup")
		requireEmpty(t, pendingDir(repo))
	})
	t.Run("background run", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a folder its user may not write")
		}
		s, f, r := directWriteSetup(t)
		fe := publishDeadEnd(t, s, f, r, "s")
		repo := rc(t, f, r)
		pdir := pendingDir(repo)
		if err := os.Chmod(pdir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(pdir, 0o755) })
		err := EndRosterSessions(ctx, f, "s")
		requireCopyFailure(t, fmt.Sprint(err), r)
		requireSessionEnd(t, repo, "s", true, fe.EndedAt)
		requireSessionEnd(t, folderRC(f), "s", true, fe.EndedAt)
		requireEmpty(t, folderEndsDir(f.Store))
		if _, ok, err := readEnd[repoEndRecord](pdir, guardName("s")); err != nil || !ok {
			t.Fatalf("guard = %v, %v; want it kept until the folder is writable", ok, err)
		}
		if err := os.Chmod(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		start(t, s.open(r), "in", "startup")
		requireNoPendingFiles(t, f, repo)
	})
	t.Run("abandoned F swept elsewhere", func(t *testing.T) {
		s, f, r := directWriteSetup(t)
		fe := publishDeadEnd(t, s, f, r, "s")
		repo := rc(t, f, r)
		restore := failCopies(t, pendingDir(repo))
		age(t, s, folderEndPath(f, s.folder, "s"), EndRunLimit+time.Second)
		res, err := Handle(ctx, s.open(s.goRepo("unrelated")), Event{Kind: KindSessionStart, SessionID: "u", Source: "startup"})
		if err != nil {
			t.Fatal(err)
		}
		requireCopyFailure(t, failure(res), r)
		requireSessionEnd(t, folderRC(f), "s", true, fe.EndedAt)
		requireEmpty(t, folderEndsDir(f.Store))
		restore()
		_, at := stopScratch(t, s, f, "s")
		requireSessionEnd(t, repo, "s", true, at)
		requireNoPendingFiles(t, f, repo)
	})
}

func TestOrphanGuardIsFinishedByTheNextStopOrExpires(t *testing.T) {
	setup := func(t *testing.T) (*scratch, *app.App, string, repoEndRecord) {
		s := newScratch(t)
		r := s.goRepo("repo")
		f := s.open(s.folder, r)
		start(t, f, "s", "startup")
		if _, err := f.Store.DB.Exec(`UPDATE sessions SET ended_at=started_at+1 WHERE id='s'`); err != nil {
			t.Fatal(err)
		}
		row, _, err := folderSession(context.Background(), f.Store, "s")
		if err != nil {
			t.Fatal(err)
		}
		g := repoEndRecord{Session: "s", Agent: row.agent, Worktree: r, StartedAt: row.startedAt,
			Root: rc(t, f, r).Git.RootCommit, Dir: s.folder, Nonce: newNonce()}
		if _, _, err := claimEnd(pendingDir(rc(t, f, r)), guardName("s"), g); err != nil {
			t.Fatal(err)
		}
		return s, f, r, g
	}
	t.Run("next Stop", func(t *testing.T) {
		s, f, r, _ := setup(t)
		_, at := stopScratch(t, s, f, "s")
		requireSessionEnd(t, rc(t, f, r), "s", true, at)
		requireNoPendingFiles(t, f, rc(t, f, r))
	})
	t.Run("expiry", func(t *testing.T) {
		s, f, r, _ := setup(t)
		repo := rc(t, f, r)
		want := prunableJournal(t, s, repo, r)
		in := s.open(r)
		start(t, in, "young", "startup")
		if rowsBeforeTenth(t, repo, r) == 0 {
			t.Fatal("a young orphan guard did not hold pruning")
		}
		age(t, s, filepath.Join(pendingDir(repo), guardName("s")), EndRunLimit+time.Second)
		start(t, in, "old", "startup")
		requireNoPendingFiles(t, in, repo)
		requirePruned(t, repo, r, want)
	})
}

func TestNoLongerServedRepoEndWrittenByTheFolder(t *testing.T) {
	s := newScratch(t)
	r, o := s.goRepo("repo"), s.goRepo("other")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	release := holdWriter(t, rc(t, f, r).Store)
	_, at := stopScratch(t, s, f, "s")
	release()
	g := s.open(s.folder, o)
	start(t, g, "n", "startup")
	requireSessionEnd(t, rc(t, f, r), "s", true, at)
	requireNoPendingFiles(t, g, rc(t, f, r))
}

func TestNudgeOncePerConversationWithFolderStoreBusy(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	if err := f.Ledger.Incr(context.Background(), "s", "edits", stopNudgeEditThreshold); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindStop, SessionID: "s"}
	release := holdWriter(t, f.Store)
	first, err := Handle(context.Background(), f, ev)
	release()
	if err != nil || !first.Block || first.Failure != nil {
		t.Fatalf("first Stop = %+v, %v; want the nudge", first, err)
	}
	second, err := Handle(context.Background(), f, ev)
	if err != nil || second.Block {
		t.Fatalf("second Stop = %+v, %v; want no second nudge", second, err)
	}
}

func TestLaterStopRecordsNothingAndStartsNoRun(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	launches := 0
	f.EndLater = func(string) { launches++ }
	release := holdWriter(t, rc(t, f, r).Store)
	var counts []int
	_, t1 := stopScratch(t, s, f, "s")
	counts = append(counts, launches)
	stopScratch(t, s, f, "s")
	counts = append(counts, launches-counts[0])
	release()
	stopScratch(t, s, f, "s")
	counts = append(counts, launches-counts[0]-counts[1])
	if !slices.Equal(counts, []int{1, 0, 0}) {
		t.Fatalf("runs started per Stop = %v, want [1 0 0]", counts)
	}
	requireSessionEnd(t, rc(t, f, r), "s", true, t1)
	requireNoPendingFiles(t, f, rc(t, f, r))
}

func TestStaleFilesWithoutReadPermission(t *testing.T) {
	t.Run("removed", testStaleUnreadableRemoved)
	t.Run("young", testStaleUnreadableYoung)
	t.Run("not empty, not a record's name", func(t *testing.T) { testStaleUnreadableContent(t, "garbage", "not json") })
	t.Run("not empty, at a record's name", func(t *testing.T) {
		testStaleUnreadableContent(t, folderEndName(gitctx.NonGitID("/gone"), "x"), "not json"+strings.Repeat(" ", 100))
	})
	t.Run("readable, but its removal is refused", testStaleReadableRemovalRefused)
	t.Run("a file that takes the name in the gap", func(t *testing.T) {
		t.Run("a valid F", func(t *testing.T) { testStaleNameTakenInTheGap(t, false) })
		t.Run("a symlink", func(t *testing.T) { testStaleNameTakenInTheGap(t, true) })
	})
	t.Run("a liveness file", func(t *testing.T) { testStaleUnreadableContent(t, ".live-x", "") })
	t.Run("lock held elsewhere", testStaleUnreadableLockHeld)
}

// staleSetup opens a repo whose session starts sweep the shared pending-ends
// folder, and returns that folder.
func staleSetup(t *testing.T, unreadable bool) (*scratch, *app.App, string) {
	t.Helper()
	if unreadable && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
		t.Skip("needs a file its owner may not read")
	}
	s := newScratch(t)
	a := s.open(s.goRepo("repo"))
	d := folderEndsDir(a.Store)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return s, a, d
}

// putStale writes data at path with mode, d old by the fake clock.
func putStale(t *testing.T, s *scratch, path, data string, mode fs.FileMode, d time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	age(t, s, path, d)
}

// sweepStart runs a session start in a and returns its failure.
func sweepStart(t *testing.T, a *app.App, id string) string {
	t.Helper()
	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, SessionID: id, Source: "startup"})
	if err != nil {
		t.Fatal(err)
	}
	return failure(res)
}

// present reports whether path names a file, a symlink included.
func present(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

const stale = EndRunLimit + time.Second

func testStaleUnreadableRemoved(t *testing.T) {
	s, a, d := staleSetup(t, true)
	p := filepath.Join(d, "unreadable")
	putStale(t, s, p, "", 0, stale)
	if f := sweepStart(t, a, "next"); f != "" || present(t, p) {
		t.Fatalf("failure %q, file present %v", f, present(t, p))
	}
	entries, err := os.ReadDir(d)
	if err != nil || len(entries) != 1 || entries[0].Name() != staleLockName {
		t.Fatalf("folder holds %v, %v; want the stale-removal lock alone", entries, err)
	}
	if recs, other, err := listEnds[folderEndRecord](d); err != nil || len(recs)+len(other) != 0 {
		t.Fatalf("listing = %v, %v, %v; want nothing", recs, other, err)
	}
}

func testStaleUnreadableYoung(t *testing.T) {
	s, a, d := staleSetup(t, true)
	p := filepath.Join(d, "unreadable")
	putStale(t, s, p, "", 0, EndRunLimit-time.Second)
	if f := sweepStart(t, a, "next"); f != "" || !present(t, p) {
		t.Fatalf("failure %q, file present %v", f, present(t, p))
	}
}

func testStaleUnreadableContent(t *testing.T, name, data string) {
	s, a, d := staleSetup(t, true)
	p := filepath.Join(d, name)
	putStale(t, s, p, data, 0, stale)
	if f := sweepStart(t, a, "next"); f != "" || present(t, p) {
		t.Fatalf("failure %q, file present %v", f, present(t, p))
	}
}

func testStaleReadableRemovalRefused(t *testing.T) {
	s, a, d := staleSetup(t, false)
	refused, other := filepath.Join(d, "refused"), filepath.Join(d, "other")
	for _, p := range []string{refused, other} {
		putStale(t, s, p, "not json", 0o644, stale)
	}
	prev := removeFile
	t.Cleanup(func() { removeFile = prev })
	removeFile = func(path string, match func(f *os.File, fi fs.FileInfo) bool) (bool, error) {
		if path == refused {
			return false, &fs.PathError{Op: "remove", Path: path, Err: fs.ErrPermission}
		}
		return prev(path, match)
	}
	f := sweepStart(t, a, "next")
	if !strings.Contains(f, refused) || present(t, other) {
		t.Fatalf("failure %q, other present %v; want refused named and other removed", f, present(t, other))
	}
	if data, err := os.ReadFile(refused); err != nil || string(data) != "not json" {
		t.Fatalf("refused = %q, %v; want it kept unchanged", data, err)
	}
	removeFile = prev
	if f := sweepStart(t, a, "again"); f != "" || present(t, refused) {
		t.Fatalf("failure %q, refused present %v", f, present(t, refused))
	}
}

func testStaleNameTakenInTheGap(t *testing.T, symlink bool) {
	s, a, d := staleSetup(t, true)
	gone := filepath.Join(s.base, "gone")
	p := filepath.Join(d, folderEndName(gitctx.NonGitID(gone), "x"))
	putStale(t, s, p, "", 0, stale)
	target := filepath.Join(t.TempDir(), "target")
	valid := fmt.Sprintf(`{"session":"x","dir":%q,"ended_at":5,"live":".live-k"}`, gone)
	prev := removeFile
	t.Cleanup(func() { removeFile = prev })
	replaced := false
	removeFile = func(path string, match func(f *os.File, fi fs.FileInfo) bool) (bool, error) {
		removed, err := prev(path, match)
		if path != p || replaced {
			return removed, err
		}
		replaced = true
		// Moved aside, not removed, so its file ID stays taken and the new file gets another.
		if rerr := os.Rename(p, filepath.Join(t.TempDir(), "listed")); rerr != nil {
			t.Fatal(rerr)
		}
		if symlink {
			putStale(t, s, target, "", 0, stale)
			if lerr := os.Symlink(target, p); lerr != nil {
				t.Fatal(lerr)
			}
		} else if werr := os.WriteFile(p, []byte(valid), 0o600); werr != nil {
			t.Fatal(werr)
		}
		return removed, err
	}
	if f := sweepStart(t, a, "next"); f != "" || !replaced {
		t.Fatalf("failure %q, replaced %v", f, replaced)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if symlink {
		ti, err := os.Stat(target)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 || ti.Mode().Perm() != 0 {
			t.Fatalf("symlink %v, target %v, %v; want both kept", fi.Mode(), ti, err)
		}
		return
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != valid || fi.Mode().Perm() != 0o600 {
		t.Fatalf("F = %q, %v, mode %v; want it kept unchanged", data, err, fi.Mode())
	}
}

func testStaleUnreadableLockHeld(t *testing.T) {
	s, a, d := staleSetup(t, true)
	p := filepath.Join(d, "unreadable")
	putStale(t, s, p, "", 0, stale)
	release, ok, err := store.TryLockFile(filepath.Join(d, staleLockName))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	if f := sweepStart(t, a, "next"); f != "" || !present(t, p) {
		t.Fatalf("failure %q, file present %v", f, present(t, p))
	}
	release()
	if f := sweepStart(t, a, "again"); f != "" || present(t, p) {
		t.Fatalf("failure %q, file present %v", f, present(t, p))
	}
}
