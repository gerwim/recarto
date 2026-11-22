package agents

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		if !strings.Contains(failure(res), "session end not recorded in "+s.folder) {
			t.Fatalf("failure = %q; want the folder's store named", failure(res))
		}
		_, at2 := stopScratch(t, s, f, "s")
		requireSessionEnd(t, folderRC(f), "s", true, at2)
		requireSessionEnd(t, rc(t, f, r), "s", true, at)
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
		if isFolder(dir) && !inner {
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

func TestFailedGuardLinkNeverTakesAnEarlierTime(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("repo")
	f := s.open(s.folder, r)
	start(t, f, "s", "startup")
	in := s.open(r)
	inner := false
	var t2, pruneAt int64
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
			_, t2 = stopScratch(t, s, f, "s")
			inner = false
		}
		return existing, created, err
	})
	_, t1 := stopScratch(t, s, f, "s")
	if !(t1 < pruneAt && pruneAt < t2) {
		t.Fatalf("fixture times %d, %d, %d", t1, pruneAt, t2)
	}
	requireSessionEnd(t, folderRC(f), "s", true, t1)
	requireSessionEnd(t, rc(t, f, r), "s", true, t2)
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

	// publish writes a plain guard of s and an F covering it, as a Stop that
	// died after publishing F leaves them, and returns F.
	publish := func(t *testing.T, s *scratch, f *app.App, r string) folderEndRecord {
		t.Helper()
		row, _, err := folderSession(context.Background(), f.Store, "s")
		if err != nil {
			t.Fatal(err)
		}
		repo := rc(t, f, r)
		g := repoEndRecord{Session: "s", Agent: row.agent, Worktree: r, StartedAt: row.startedAt,
			Root: repo.Git.RootCommit, Dir: s.folder, Nonce: newNonce()}
		if _, _, err := claimEnd(pendingDir(repo), guardName("s"), g); err != nil {
			t.Fatal(err)
		}
		s.clk.Advance(time.Second)
		return deadFolderEnd(t, folderEndsDir(f.Store), folderEndRecord{Session: "s", Dir: s.folder,
			EndedAt: s.clk.Now().UnixMilli(), Covered: []string{repo.Git.RepoID}})
	}

	t.Run("a publisher that died after F", func(t *testing.T) {
		s, f, r := setup(t)
		fe := publish(t, s, f, r)
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
		fe := publish(t, s, f, r)
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
		fe := publish(t, s, f, r)
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
