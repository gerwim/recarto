package agents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/syncwire"
)

// scratch is a code-less folder beside git repos, in one cache, on one fake clock.
type scratch struct {
	t      *testing.T
	base   string
	folder string
	clk    *clock.Fake
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	clearRecartoEnv(t)
	// The first-index wait ends when a test says so or the hook ends, never by the real timer.
	endWaitWhen(t, make(chan struct{}))
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &scratch{t: t, base: base, folder: filepath.Join(base, "_scratch"), clk: clock.NewFake(time.UnixMilli(1_700_000_000_000))}
	if err := os.MkdirAll(s.folder, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// repo makes a committed git repo at base/rel holding files.
func (s *scratch) repo(rel string, files map[string]string) string {
	s.t.Helper()
	dir := filepath.Join(s.base, rel)
	writeTree(s.t, dir, files)
	gitIn(s.t, dir, "init", "-q")
	gitIn(s.t, dir, "add", "-A")
	gitIn(s.t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// goRepo makes a repo whose one module, named after it, holds one function.
func (s *scratch) goRepo(rel string) string {
	name := filepath.Base(rel)
	return s.repo(rel, map[string]string{name + "/a.go": "package " + name + "\n\nfunc Here() {}\n"})
}

// open opens an App in dir with RECARTO_REPOS serving repos.
func (s *scratch) open(dir string, repos ...string) *app.App {
	s.t.Helper()
	s.t.Setenv("RECARTO_REPOS", strings.Join(repos, string(os.PathListSeparator)))
	a, err := app.Open(app.Config{CWD: dir, Clock: s.clk})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = a.Close() })
	markServing(s.t, a)
	return a
}

// rc returns a's context for worktree wt, the primary included.
func rc(t *testing.T, a *app.App, wt string) *app.RepoContext {
	t.Helper()
	for _, r := range a.AllRepos() {
		if r.Git.Worktree == wt {
			return r
		}
	}
	t.Fatalf("%s is not served", wt)
	return nil
}

// index brings r's graph up to date, as recarto warm does.
func index(t *testing.T, r *app.RepoContext) {
	t.Helper()
	wt := r.Git.Worktree
	if err := r.Graph.ReconcileIfIdle(context.Background(), wt, graph.NewSource(wt),
		func(attempt string) string { return graph.WorktreeToken(wt, attempt) }); err != nil {
		t.Fatal(err)
	}
}

// remember records a memory in r, branch-scoped unless global.
func remember(t *testing.T, r *app.RepoContext, kind memory.Kind, title, body string, refs ...memory.Ref) string {
	t.Helper()
	id, _, _, err := r.Engine.Record(context.Background(), memory.NewMemory{
		Kind: kind, Title: title, Body: body, Branch: r.Git.Branch, Worktree: r.Git.Worktree,
		Source: "mcp", Pinned: kind == memory.KindPreference, Refs: refs})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sessionRow is a sessions row's start and end.
type sessionRow struct{ start, end sql.NullInt64 }

// sessionRows returns r's rows for session id.
func sessionRows(t *testing.T, r *app.RepoContext, id string) []sessionRow {
	t.Helper()
	rows, err := r.Store.DB.Query(`SELECT started_at, ended_at FROM sessions WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.start, &row.end); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// appendFunc appends one empty function per name to file.
func appendFunc(t *testing.T, file string, names ...string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := fmt.Fprintf(f, "\nfunc %s() {}\n", name); err != nil {
			_ = f.Close() // the write failure is the one to report
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// editAdd appends a function to file in r's worktree and reports the edit.
func (s *scratch) editAdd(a *app.App, file, fn string) {
	s.t.Helper()
	editIn(s.t, s, a, "s", file, fn)
}

// stop ends a turn of session id.
func (s *scratch) stop(a *app.App, id string) {
	s.t.Helper()
	s.clk.Advance(time.Second)
	handle(s.t, a, Event{Kind: KindStop, SessionID: id, StopActive: true})
}

// startResult is what a session start returned.
type startResult struct {
	res Result
	err error
}

// startOrGiveUp runs a session start in a goroutine and awaits it with the
// hang-only timeout. giveUp ends everything the test holds the start on: its
// fake first-index deadline, source gates and held writers. A cleanup calls
// giveUp and waits for the start to return, so a start that hangs fails the
// test and has returned before the test's App closes.
func startOrGiveUp(t *testing.T, a *app.App, session, source string, giveUp func()) string {
	t.Helper()
	got := make(chan startResult, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: session, Source: source})
		got <- startResult{res, err}
	}()
	t.Cleanup(func() {
		giveUp()
		select {
		case <-returned:
		case <-time.After(hang):
			t.Errorf("session start %s still running after giving up", source)
		}
	})
	r := await(t, "session start "+source, got)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.res.Failure != nil {
		t.Fatalf("reported failure: %v", r.res.Failure)
	}
	return r.res.Context
}

// waitIdle registers a cleanup that calls giveUp and waits, within the
// hang-only timeout, for the test's fake index goroutines in bg.
func waitIdle(t *testing.T, bg *sync.WaitGroup, giveUp func()) {
	t.Cleanup(func() {
		giveUp()
		idle := make(chan struct{})
		go func() {
			bg.Wait()
			close(idle)
		}()
		select {
		case <-idle:
		case <-time.After(hang):
			t.Errorf("the fake indexes did not end")
		}
	})
}

func TestScratchOneServedRepoIsTheInRepoBlock(t *testing.T) {
	for name, big := range map[string]bool{"default": false, "large": true} {
		t.Run(name, func(t *testing.T) {
			s := newScratch(t)
			t.Setenv("RECARTO_WORKSPACE", "ws")
			r := s.goRepo("recarto")
			in := s.open(r, r)
			rr := rc(t, in, r)
			index(t, rr)
			remember(t, rr, memory.KindDoNotRepeat, "never skip the lock", "always take the lock")
			remember(t, rr, memory.KindPreference, "short names", "prefer short names")
			remember(t, rr, memory.KindDecision, "one store per repo", "keep it")
			remember(t, rr, memory.KindTodo, "write the docs", "soon")
			remember(t, rr, memory.KindBug, "index lost an edge", "fixed")
			if big {
				for i := range 5 {
					remember(t, rr, memory.KindBug, fmt.Sprintf("bug %d %s", i, strings.Repeat("t", 150)), "fixed")
				}
				for i := range 10 {
					remember(t, rr, memory.KindLearning, fmt.Sprintf("learning %d", i), strings.Repeat(fmt.Sprintf("w%d ", i), 190))
				}
			}
			if _, _, _, err := in.Workspace.Engine.Record(context.Background(), memory.NewMemory{
				Kind: memory.KindDecision, Title: "WS-DECISION", Body: "shared", Source: "mcp"}); err != nil {
				t.Fatal(err)
			}
			want := start(t, in, "in", "startup")
			got := start(t, s.open(s.folder, r), "f", "startup")
			got = strings.Replace(got, alsoServing([]string{"recarto"}), "", 1)
			if got != want {
				t.Fatalf("folder block:\n%s\n\nin-repo block:\n%s", got, want)
			}
			for _, w := range []string{"never skip the lock", "short names", "one store per repo", "write the docs", "index lost an edge", codeMapHeader, "WS-DECISION"} {
				if !strings.Contains(want, w) {
					t.Errorf("block lacks %q:\n%s", w, want)
				}
			}
			if strings.Contains(want, indexingLine) {
				t.Errorf("indexing line in the block:\n%s", want)
			}
		})
	}
}

func TestScratchDecidedOncePerConversation(t *testing.T) {
	for _, first := range []string{"startup", "compact"} {
		t.Run(first, func(t *testing.T) {
			s := newScratch(t)
			r := s.goRepo("recarto")
			a := s.open(s.folder, r)
			rr := rc(t, a, r)
			index(t, rr)
			remember(t, rr, memory.KindDecision, "REPO-DECISION", "x")
			t0 := s.clk.Now().UnixMilli()
			if b := start(t, a, "s", first); !strings.Contains(b, "REPO-DECISION") {
				t.Fatalf("first block lacks the repo:\n%s", b)
			}
			file := filepath.Join(r, "recarto", "a.go")
			s.editAdd(a, file, "helperA")
			writeTree(t, s.folder, map[string]string{"x.go": "package x\n"})
			if b := start(t, a, "s", "compact"); !strings.Contains(b, "REPO-DECISION") {
				t.Fatalf("compact lost the repo:\n%s", b)
			}
			s.editAdd(a, file, "helperB")
			if first == "compact" {
				start(t, a, "s", "resume")
			}
			s.stop(a, "s")
			rows := sessionRows(t, rr, "s")
			if len(rows) != 1 || rows[0].start.Int64 != t0 || rows[0].end.Int64 != s.clk.Now().UnixMilli() {
				t.Fatalf("repo sessions %+v, want one from %d to the Stop", rows, t0)
			}
			s.clk.Advance(time.Second)
			in := start(t, s.open(r), "later", "startup")
			if !strings.Contains(in, "helperA") || !strings.Contains(in, "helperB") {
				t.Errorf("in-repo session lacks both edits:\n%s", in)
			}
			if b := start(t, s.open(s.folder, r), "new", "startup"); strings.Contains(b, "REPO-DECISION") {
				t.Errorf("a new conversation in a folder with code shows the repo:\n%s", b)
			}
		})
	}
}

func TestScratchNoReposAtTheFirstStart(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	remember(t, rc(t, s.open(r), r), memory.KindDecision, "REPO-DECISION", "x")
	start(t, s.open(s.folder), "s", "startup")
	a := s.open(s.folder, r)
	if b := start(t, a, "s", "compact"); strings.Contains(b, "REPO-DECISION") {
		t.Fatalf("compact switched to the repo:\n%s", b)
	}
	if rows := sessionRows(t, rc(t, a, r), "s"); len(rows) != 0 {
		t.Errorf("repo sessions %+v, want none", rows)
	}
	if b := start(t, a, "new", "startup"); !strings.Contains(b, "REPO-DECISION") {
		t.Errorf("a new conversation lacks the repo:\n%s", b)
	}
}

func TestScratchRosterStaysWithTheConversation(t *testing.T) {
	s := newScratch(t)
	r, second := s.goRepo("recarto"), s.goRepo("second")
	a := s.open(s.folder, r)
	index(t, rc(t, a, r))
	start(t, a, "s", "startup")

	b := s.open(s.folder, second)
	var started []string
	var mu sync.Mutex
	b.StartIndex = func(wt string) (<-chan error, error) {
		mu.Lock()
		started = append(started, wt)
		mu.Unlock()
		return nil, fmt.Errorf("no index here")
	}
	rr, err := app.OpenRepo(r, s.clk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rr.Close() }()
	remember(t, rr, memory.KindLearning, "RECARTO-NEW", "x")
	remember(t, rc(t, b, second), memory.KindLearning, "SECOND-NEW", "x")
	mark := r + " (no longer served)"
	res := start(t, b, "s", "resume")
	if !strings.Contains(res, "RECARTO-NEW") || strings.Contains(res, "SECOND-NEW") || !strings.Contains(res, "Recarto memory for "+mark) {
		t.Fatalf("resume:\n%s", res)
	}
	full := start(t, b, "s", "compact")
	if !strings.Contains(full, "Repo: "+r+" (no longer served; graph and memory tools cannot reach it).") || strings.Contains(full, "SECOND-NEW") {
		t.Fatalf("compact:\n%s", full)
	}
	if strings.Contains(full, "Also serving") {
		t.Errorf("an unserved repo offered as repo value:\n%s", full)
	}
	if slices.Contains(started, r) {
		t.Errorf("an index attempt at the unserved repo")
	}

	s.editAdd(b, filepath.Join(second, "second", "a.go"), "added")
	s.stop(b, "s")
	if rows := sessionRows(t, rc(t, b, second), "s"); len(rows) != 0 {
		t.Errorf("second got a session: %+v", rows)
	}
	if rows := sessionRows(t, rr, "s"); len(rows) != 1 || !rows[0].end.Valid {
		t.Errorf("recarto's session not ended: %+v", rows)
	}
	if blk := start(t, b, "new", "startup"); !strings.Contains(blk, alsoServing([]string{"second"})) {
		t.Errorf("a new conversation serves second:\n%s", blk)
	}
}

func TestScratchCheckoutGoneAndBack(t *testing.T) {
	s := newScratch(t)
	r, second := s.goRepo("recarto"), s.goRepo("second")
	a := s.open(s.folder, r, second)
	index(t, rc(t, a, r))
	index(t, rc(t, a, second))
	if b := start(t, a, "s", "startup"); !strings.Contains(b, "\nsecond: ") || !strings.Contains(b, "\nrecarto: ") {
		t.Fatalf("first block:\n%s", b)
	}
	moved := second + "-moved"
	if err := os.Rename(second, moved); err != nil {
		t.Fatal(err)
	}
	gone := s.open(s.folder, r, second)
	if b := start(t, gone, "s", "compact"); strings.Contains(b, "second") || !strings.Contains(b, alsoServing([]string{"recarto"})) {
		t.Fatalf("with second gone:\n%s", b)
	}
	if err := os.Rename(moved, second); err != nil {
		t.Fatal(err)
	}
	if b := start(t, s.open(s.folder, r, second), "s", "compact"); !strings.Contains(b, "\nsecond: ") || !strings.Contains(b, "\nrecarto: ") {
		t.Fatalf("with second back:\n%s", b)
	}
}

func TestScratchNoRosterRepoLeft(t *testing.T) {
	s := newScratch(t)
	t.Setenv("RECARTO_WORKSPACE", "ws")
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{Kind: memory.KindLearning, Title: "FOLDER-NOTE", Body: "x", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Workspace.Engine.Record(context.Background(), memory.NewMemory{Kind: memory.KindDecision, Title: "WS-DECISION", Body: "x", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	remember(t, rc(t, a, r), memory.KindDecision, "REPO-DECISION", "x")
	start(t, a, "s", "startup")
	moved := r + "-moved"
	if err := os.Rename(r, moved); err != nil {
		t.Fatal(err)
	}
	g := s.open(s.folder, r)
	b := start(t, g, "s", "compact")
	if !strings.Contains(b, "WS-DECISION") || !strings.Contains(b, r+": unavailable; its checkout cannot be opened.") ||
		strings.Contains(b, "FOLDER-NOTE") || strings.Contains(b, "REPO-DECISION") || strings.Contains(b, codeMapHeader) {
		t.Fatalf("no repo left:\n%s", b)
	}
	if _, _, _, err := g.Workspace.Engine.Record(context.Background(), memory.NewMemory{Kind: memory.KindDecision, Title: "WS-NEW", Body: "y", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if res := start(t, g, "s", "resume"); !strings.Contains(res, "WS-NEW") || strings.Contains(res, "WS-DECISION") {
		t.Fatalf("resume:\n%s", res)
	}
	s.stop(g, "s")
	if err := os.Rename(moved, r); err != nil {
		t.Fatal(err)
	}
	back := s.open(s.folder, r)
	rr := rc(t, back, r)
	if rows := sessionRows(t, rr, "s"); len(rows) != 1 || rows[0].end.Valid {
		t.Fatalf("after a Stop while gone: %+v, want one open session", rows)
	}
	if b := start(t, back, "s", "compact"); !strings.Contains(b, "REPO-DECISION") {
		t.Fatalf("back:\n%s", b)
	}
	if rows := sessionRows(t, rr, "s"); len(rows) != 1 {
		t.Fatalf("compact added a session: %+v", rows)
	}
	s.stop(back, "s")
	if rows := sessionRows(t, rr, "s"); !rows[0].end.Valid {
		t.Errorf("the next Stop did not end the session")
	}
}

func TestScratchSeveralRepos(t *testing.T) {
	s := newScratch(t)
	t.Setenv("RECARTO_WORKSPACE", "ws")
	r, second := s.goRepo("recarto"), s.goRepo("second")
	a := s.open(s.folder, r, second)
	for _, wt := range []string{r, second} {
		x := rc(t, a, wt)
		index(t, x)
		name := strings.ToUpper(filepath.Base(wt))
		remember(t, x, memory.KindDoNotRepeat, name+"-PIN", "x")
		remember(t, x, memory.KindDecision, name+"-DECISION", "x")
		remember(t, x, memory.KindBug, name+"-BUG", "x")
	}
	if _, _, _, err := a.Workspace.Engine.Record(context.Background(), memory.NewMemory{Kind: memory.KindDecision, Title: "WS-DECISION", Body: "x", Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	b := start(t, a, "s", "startup")
	for _, w := range []string{
		"[do_not_repeat, recarto] RECARTO-PIN", "[decision, second] SECOND-DECISION", "- RECARTO-BUG", "- SECOND-BUG",
		"\nrecarto: 1 files, 1 symbols\n", "\nsecond: 1 files, 1 symbols\n", "- recarto: 1 files", "- second: 1 files",
		"WS-DECISION", "with one of: recarto, second",
	} {
		if !strings.Contains(b, w) {
			t.Errorf("block lacks %q", w)
		}
	}
	if t.Failed() {
		t.Fatalf("block:\n%s", b)
	}

	remember(t, rc(t, a, second), memory.KindLearning, "NEW-LEARNING", "x")
	res := start(t, a, "s", "resume")
	if !strings.Contains(res, "[learning, second] NEW-LEARNING") || strings.Contains(res, "DECISION") ||
		strings.Contains(res, codeMapHeader) || strings.Contains(res, "files,") {
		t.Fatalf("resume:\n%s", res)
	}
	if res := start(t, a, "s", "resume"); res != "" {
		t.Fatalf("second resume:\n%s", res)
	}
}

func TestScratchFirstIndexesSideBySide(t *testing.T) {
	for _, order := range []string{"second first", "big first"} {
		t.Run(order, func(t *testing.T) {
			s := newScratch(t)
			second, big := s.goRepo("second"), s.goRepo("big")
			repos := []string{second, big}
			if order == "big first" {
				repos = []string{big, second}
			}
			a := s.open(s.folder, repos...)
			remember(t, rc(t, a, second), memory.KindLearning, "NoSuchHelper wraps the retries", "x", memory.Ref{Type: memory.RefSymbol, Value: "NoSuchHelper"})
			src := newGated(graph.NewSource(big), false)
			stop := make(chan struct{})
			openGate := sync.OnceFunc(func() { close(src.gate) })
			giveUp := sync.OnceFunc(func() { close(stop); openGate() })
			secondDone := make(chan struct{})
			var secondOnce sync.Once
			ch := make(chan struct{})
			go func() {
				defer close(ch)
				for _, sig := range []chan struct{}{secondDone, src.entered} {
					select {
					case <-sig:
					case <-stop:
						return
					}
				}
			}()
			endWaitWhen(t, ch)
			var bg sync.WaitGroup
			bigRan := make(chan error, 1)
			var mu sync.Mutex
			starts := map[string]int{}
			a.StartIndex = func(wt string) (<-chan error, error) {
				mu.Lock()
				starts[wt]++
				mu.Unlock()
				done := make(chan error, 1)
				x := rc(t, a, wt)
				bg.Go(func() {
					switch wt {
					case big:
						err := x.Graph.ReconcileIfIdle(context.Background(), wt, src, func(at string) string { return graph.WorktreeToken(wt, at) })
						bigRan <- err
						done <- err
					default:
						index(t, x)
						if wt == second {
							secondOnce.Do(func() { close(secondDone) })
						}
						done <- nil
					}
				})
				return done, nil
			}
			waitIdle(t, &bg, giveUp)
			b := startOrGiveUp(t, a, "s", "startup", giveUp)
			for _, w := range []string{"- second: 1 files", "\nbig: " + indexingLine, "references code not present in this worktree"} {
				if !strings.Contains(b, w) {
					t.Errorf("block lacks %q", w)
				}
			}
			if strings.Contains(b, "- big:") || starts[big] != 1 || starts[second] != 1 {
				t.Errorf("starts %v", starts)
			}
			if t.Failed() {
				t.Fatalf("block:\n%s", b)
			}
			openGate()
			if err := await(t, "big's index", bigRan); err != nil {
				t.Fatal(err)
			}
			// Nothing waits now; the deadline ends only if the test gives up, so the
			// folder's code check cannot run out.
			endWaitWhen(t, stop)
			if b := startOrGiveUp(t, a, "s2", "startup", giveUp); !strings.Contains(b, "- big: 1 files") || strings.Contains(b, indexingLine) {
				t.Fatalf("after big's index:\n%s", b)
			}
		})
	}
}

func TestScratchFailedFirstIndexAndCodeMapOff(t *testing.T) {
	s := newScratch(t)
	second, big := s.goRepo("second"), s.goRepo("big")
	a := s.open(s.folder, second, big)
	index(t, rc(t, a, second))
	var calls []string
	var mu sync.Mutex
	a.StartIndex = func(wt string) (<-chan error, error) {
		mu.Lock()
		calls = append(calls, wt)
		mu.Unlock()
		done := make(chan error, 1)
		done <- fmt.Errorf("git ls-files: exit status 1")
		return done, nil
	}
	b := start(t, a, "s", "startup")
	if !strings.Contains(b, "\nbig: "+unbuiltLine) || !strings.Contains(b, "- second: 1 files") || !slices.Equal(calls, []string{big}) {
		t.Fatalf("calls %v, block:\n%s", calls, b)
	}
	a.Features = features.Set{features.CodeMap: false}
	calls = nil
	b = start(t, a, "s2", "startup")
	if len(calls) != 0 || !strings.Contains(b, "\nsecond\n") || !strings.Contains(b, "\nbig\n") || strings.Contains(b, "files,") || strings.Contains(b, unbuiltLine) {
		t.Fatalf("code map off: calls %v, block:\n%s", calls, b)
	}
}

func TestScratchRecentChangesAcrossScratchSessions(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	ctx := context.Background()
	if _, ok, err := rr.Graph.DeltaBaseline(ctx, r, s.clk.Now().UnixMilli()); err != nil || ok {
		t.Fatalf("baseline before any session: %v %v", ok, err)
	}
	start(t, a, "A", "startup")
	s.editAdd(a, filepath.Join(r, "recarto", "a.go"), "editedHelper")
	s.stop(a, "A")
	s.clk.Advance(time.Second)
	if b := start(t, a, "B", "startup"); !strings.Contains(b, "editedHelper") {
		t.Fatalf("next folder session lacks the change:\n%s", b)
	}
	if _, ok, err := rr.Graph.DeltaBaseline(ctx, r, s.clk.Now().UnixMilli()); err != nil || !ok {
		t.Fatalf("baseline: %v %v", ok, err)
	}
	s.clk.Advance(time.Second)
	if b := start(t, s.open(r), "C", "startup"); !strings.Contains(b, "editedHelper") {
		t.Fatalf("in-repo session lacks the change:\n%s", b)
	}
}

func TestScratchNamesThatCollide(t *testing.T) {
	s := newScratch(t)
	x, y := s.goRepo("a/app"), s.goRepo("b/app")
	a := s.open(s.folder, x, y)
	index(t, rc(t, a, x))
	index(t, rc(t, a, y))
	b := start(t, a, "s", "startup")
	if !strings.Contains(b, "\n"+x+": 1 files") || !strings.Contains(b, "\n"+y+": 1 files") {
		t.Fatalf("two repos named app:\n%s", b)
	}

	s = newScratch(t)
	s.folder = filepath.Join(s.base, "second")
	if err := os.MkdirAll(s.folder, 0o755); err != nil {
		t.Fatal(err)
	}
	r, second := s.goRepo("recarto"), s.goRepo("repos/second")
	a = s.open(s.folder, r, second)
	b = start(t, a, "s", "startup")
	if !strings.Contains(b, "\nrecarto: ") || !strings.Contains(b, "\n"+second+": ") || !strings.Contains(b, "with one of: recarto, "+second+";") {
		t.Fatalf("folder named second:\n%s", b)
	}
	if sel, ok := a.Selector(rc(t, a, second)); !ok || !rc(t, a, second).AnswersTo(sel) || a.AllRepos()[0].AnswersTo(sel) {
		t.Errorf("selector %q reaches the folder", sel)
	}
}

// syncedMemory applies one learning, as sync delivers it, to each repo's
// store: copies with one id.
func syncedMemory(t *testing.T, now int64, id, title string, repos ...*app.RepoContext) {
	t.Helper()
	for _, r := range repos {
		if _, _, err := r.Engine.SyncApply(context.Background(), syncwire.Record{
			ID: id, Kind: string(memory.KindLearning), Title: title, Body: "body of " + title, Source: "mcp",
			Status: "active", Confidence: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScratchOneMemoryInTwoStores(t *testing.T) {
	s := newScratch(t)
	one, two := s.goRepo("app-1"), s.goRepo("app-2")
	a := s.open(s.folder, one, two)
	r1, r2 := rc(t, a, one), rc(t, a, two)
	syncedMemory(t, s.clk.Now().UnixMilli(), "shared-1", "SHARED", r1, r2)
	remember(t, r1, memory.KindLearning, "OWN-1", "the first clone's own note on retries")
	remember(t, r2, memory.KindLearning, "OWN-2", "the second clone's own note on caching")
	b := start(t, a, "s", "startup")
	if strings.Count(b, "] SHARED\n") != 1 || !strings.Contains(b, "[learning, app-1] SHARED") ||
		!strings.Contains(b, "[learning, app-1] OWN-1") || !strings.Contains(b, "[learning, app-2] OWN-2") {
		t.Fatalf("block:\n%s", b)
	}
	if res := start(t, a, "s", "resume"); res != "" {
		t.Fatalf("resume with nothing new:\n%s", res)
	}
	syncedMemory(t, s.clk.Now().UnixMilli(), "shared-2", "SHARED-2", r2, r1)
	if res := start(t, a, "s", "resume"); strings.Count(res, "] SHARED-2\n") != 1 || !strings.Contains(res, "[learning, app-1] SHARED-2") {
		t.Fatalf("resume:\n%s", res)
	}
	if res := start(t, a, "s", "resume"); res != "" {
		t.Fatalf("second resume:\n%s", res)
	}
}

func TestScratchRosterRowsAfterTheRoster(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	t0 := s.clk.Now().UnixMilli()

	release := holdWriter(t, rr.Store)
	// A busy writer is not reported: warm writes the row.
	if b := startOrGiveUp(t, a, "s", "startup", release); !strings.Contains(b, alsoServing([]string{"recarto"})) {
		t.Fatalf("block:\n%s", b)
	}
	if rows := sessionRows(t, rr, "s"); len(rows) != 0 {
		t.Fatalf("a row written past the held writer: %+v", rows)
	}
	startOrGiveUp(t, a, "s", "resume", release)

	attempted, goOn := make(chan struct{}), make(chan struct{})
	prev := rosterRetryWait
	t.Cleanup(func() { rosterRetryWait = prev })
	var once sync.Once
	rosterRetryWait = func(context.Context) error {
		once.Do(func() { close(attempted) })
		<-goOn
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- RecordRosterSessions(context.Background(), a, "s") }()
	await(t, "a failed attempt", attempted)
	release()
	close(goOn)
	if err := await(t, "the retries", done); err != nil {
		t.Fatal(err)
	}
	if rows := sessionRows(t, rr, "s"); len(rows) != 1 || rows[0].start.Int64 != t0 || rows[0].end.Valid {
		t.Fatalf("rows %+v", rows)
	}
	if err := RecordRosterSessions(context.Background(), a, "s"); err != nil || len(sessionRows(t, rr, "s")) != 1 {
		t.Fatalf("again: %v", err)
	}

	// Another configuration's start of the same conversation keeps its roster.
	other := s.goRepo("other")
	b := s.open(s.folder, other)
	if blk := start(t, b, "s", "startup"); !strings.Contains(blk, "Repo: "+r+" (no longer served;") {
		t.Fatalf("roster lost:\n%s", blk)
	}
	if rows := sessionRows(t, rc(t, b, other), "s"); len(rows) != 0 {
		t.Fatalf("a repo outside the roster got a row")
	}

	// A Stop writes a missing row, ended.
	start(t, a, "t", "startup")
	if _, err := rr.Store.DB.Exec(`DELETE FROM sessions WHERE id='t'`); err != nil {
		t.Fatal(err)
	}
	s.stop(a, "t")
	if rows := sessionRows(t, rr, "t"); len(rows) != 1 || !rows[0].end.Valid {
		t.Fatalf("Stop with the row missing: %+v", rows)
	}
}

func TestScratchFirstBlockCountsItsPendingSession(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	start(t, a, "A", "startup")
	s.editAdd(a, filepath.Join(r, "recarto", "a.go"), "editedHelper")
	s.stop(a, "A")
	s.clk.Advance(time.Second)
	release := holdWriter(t, rr.Store)
	got := start(t, a, "B", "startup")
	release()
	if !strings.Contains(got, "editedHelper") || len(sessionRows(t, rr, "B")) != 0 {
		t.Fatalf("block with the row missing:\n%s", got)
	}
	want := start(t, s.open(r), "in", "startup")
	if tierLines(got) == "" || tierLines(got) != tierLines(want) {
		t.Fatalf("changes differ:\n%s\n---\n%s", got, want)
	}
}

// tierLines is the recent-changes tier of a block.
func tierLines(block string) string {
	i := strings.Index(block, "Changed since")
	if i < 0 {
		return ""
	}
	return block[i:]
}

func TestScratchFolderBecomesACheckout(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	remember(t, rr, memory.KindDecision, "D-ANCHORED", "x", memory.Ref{Type: memory.RefFile, Value: "recarto/a.go"})
	if b := start(t, a, "s", "startup"); !strings.Contains(b, "D-ANCHORED") {
		t.Fatalf("first block:\n%s", b)
	}
	gitIn(t, s.folder, "init", "-q")
	g := s.open(s.folder, r)
	if got := handle(t, g, Event{Kind: KindPostRead, SessionID: "s", FilePath: filepath.Join(r, "recarto", "a.go"), ToolResult: "x"}); strings.Contains(got, "D-ANCHORED") {
		t.Fatalf("shown again:\n%s", got)
	}
	if b := start(t, g, "s", "compact"); !strings.Contains(b, alsoServing([]string{"recarto"})) {
		t.Fatalf("compact left roster mode:\n%s", b)
	}
	if n := len(sessionRows(t, rc(t, g, r), "s")); n != 1 {
		t.Fatalf("repo rows %d", n)
	}
	s.stop(g, "s")
	if rows := sessionRows(t, rc(t, g, r), "s"); !rows[0].end.Valid {
		t.Fatalf("not ended")
	}
	if rows := sessionRows(t, g.AllRepos()[0], "s"); len(rows) != 0 {
		t.Fatalf("the new checkout recorded the conversation")
	}
	if b := start(t, g, "new", "startup"); !strings.Contains(b, alsoServing([]string{"recarto"})) || strings.Contains(b, "D-ANCHORED") {
		t.Fatalf("a new conversation:\n%s", b)
	}
}

func TestScratchIndexIfCode(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	var got []string
	a.IndexIfCode = func(wt string) { got = append(got, wt) }
	for _, src := range []string{"startup", "compact", "resume"} {
		start(t, a, "s", src)
	}
	if len(got) != 3 || got[0] != s.folder {
		t.Fatalf("IndexIfCode calls %v", got)
	}
	in := s.open(r)
	in.IndexIfCode = func(wt string) { t.Errorf("a git worktree's start called IndexIfCode") }
	start(t, in, "t", "startup")
}

func TestScratchUnselectableRepo(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	upper := filepath.Join(s.base, "RECARTO")
	if _, err := os.Stat(upper); err == nil {
		t.Skip("case-insensitive file system")
	}
	a := s.open(s.folder, r)
	a.Features = features.Set{features.SessionMemories: false}
	start(t, a, "s", "startup")
	// Put a repo answering to r's path, ignoring case, before it.
	other := s.repo("RECARTO", map[string]string{"a.go": "package a\n"})
	b := s.open(s.folder, other, r)
	b.Features = features.Set{features.SessionMemories: false}
	blk := start(t, b, "s", "compact")
	if !strings.Contains(blk, "Repo: "+r+" (the repo argument cannot select it; graph and memory tools cannot reach it).") ||
		strings.Contains(blk, "Also serving") || strings.Contains(blk, "recarto MCP tool") {
		t.Fatalf("block:\n%s", blk)
	}
}

func TestScratchReposIndexSideBySide(t *testing.T) {
	s := newScratch(t)
	x, y := s.goRepo("x"), s.goRepo("y")
	a := s.open(s.folder, x, y)
	rx, ry := rc(t, a, x), rc(t, a, y)
	// x, listed first, has its writer held and an index start that never ends.
	release := holdWriter(t, rx.Store)
	defer release()
	yDone, stop, ch := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ch)
		select {
		case <-yDone:
		case <-stop:
		}
	}()
	endWaitWhen(t, ch)
	giveUp := sync.OnceFunc(func() { close(stop); release() })
	var bg sync.WaitGroup
	a.StartIndex = func(wt string) (<-chan error, error) {
		done := make(chan error, 1)
		if wt == y {
			bg.Go(func() {
				index(t, ry)
				close(yDone)
				done <- nil
			})
		}
		return done, nil
	}
	waitIdle(t, &bg, giveUp)
	b := startOrGiveUp(t, a, "s", "startup", giveUp)
	if !strings.Contains(b, "- y: 1 files") || !strings.Contains(b, "\nx: "+unbuiltLine) {
		t.Fatalf("block:\n%s", b)
	}
}

func TestScratchResumeAfterLostBlockMarks(t *testing.T) {
	s := newScratch(t)
	r, second := s.goRepo("recarto"), s.goRepo("second")
	a := s.open(s.folder, r, second)
	remember(t, rc(t, a, r), memory.KindDecision, "REPO-DECISION", "x")
	start(t, a, "s", "startup")
	if err := a.Ledger.Delete(context.Background(), "session:s", blockMetric, shownMetric); err != nil {
		t.Fatal(err)
	}
	a.StartIndex = func(string) (<-chan error, error) {
		t.Errorf("a resume started an index")
		return nil, fmt.Errorf("no")
	}
	res := start(t, a, "s", "resume")
	if !strings.Contains(res, "REPO-DECISION") || strings.Contains(res, codeMapHeader) || strings.Contains(res, "files,") {
		t.Fatalf("resume:\n%s", res)
	}
}

func TestScratchSeveralReposOneUnselectable(t *testing.T) {
	s := newScratch(t)
	r, second := s.goRepo("recarto"), s.goRepo("second")
	if _, err := os.Stat(filepath.Join(s.base, "RECARTO")); err == nil {
		t.Skip("case-insensitive file system")
	}
	a := s.open(s.folder, r, second)
	remember(t, rc(t, a, r), memory.KindBug, "RECARTO-BUG", "x")
	start(t, a, "s", "startup")
	upper := s.goRepo("RECARTO")
	b := s.open(s.folder, upper, r, second)
	blk := start(t, b, "s", "compact")
	label := r + " (the repo argument cannot select it)"
	if !strings.Contains(blk, "with one of: second;") || !strings.Contains(blk, "\n"+label+": ") ||
		!strings.Contains(blk, "\nRecent bugs:\n- RECARTO-BUG") {
		t.Fatalf("block:\n%s", blk)
	}
}

// TestScratchRefreshEndingInTheWaitShowsItsMap: a refresh of an indexed repo
// that ends while another repo's first index is waited for shows its map.
func TestScratchRefreshEndingInTheWaitShowsItsMap(t *testing.T) {
	s := newScratch(t)
	x, y := s.goRepo("x"), s.goRepo("y")
	a := s.open(s.folder, x, y)
	rx := rc(t, a, x)
	index(t, rx)
	writeTree(t, x, map[string]string{"x/b.go": "package x\n\nfunc B() {}\n"})
	src := newGated(graph.FSSource{Root: x}, true)
	refreshDone := make(chan struct{})
	var refreshErr error
	go func() {
		defer close(refreshDone)
		refreshErr = rx.Graph.ReconcileIfStale(context.Background(), x, src, graph.WorktreeToken(x, ""))
	}()
	stop := make(chan struct{})
	openGate := sync.OnceFunc(func() { close(src.gate) })
	giveUp := sync.OnceFunc(func() { close(stop); openGate() })
	t.Cleanup(func() {
		giveUp()
		select {
		case <-refreshDone:
		case <-time.After(hang):
			t.Errorf("the refresh did not end")
		}
	})
	await(t, "the refresh", src.entered)

	prev := tierDecided
	t.Cleanup(func() { tierDecided = prev })
	tierDecided = func(tr *mapTier) {
		if tr.rc.Git.Worktree == x && tr.line == indexingLine {
			openGate()
		}
	}
	yStarted := make(chan struct{})
	a.StartIndex = func(wt string) (<-chan error, error) {
		if wt == y {
			close(yStarted)
		}
		return make(chan error), nil
	}
	end := make(chan struct{})
	go func() {
		defer close(end)
		for _, sig := range []chan struct{}{refreshDone, yStarted} {
			select {
			case <-sig:
			case <-stop:
				return
			}
		}
	}()
	endWaitWhen(t, end)
	b := startOrGiveUp(t, a, "s", "startup", giveUp)
	await(t, "the refresh", refreshDone)
	if refreshErr != nil {
		t.Fatalf("refresh: %v", refreshErr)
	}
	if !strings.Contains(b, "\nx: 2 files, 2 symbols\n") || !strings.Contains(b, "- x: 2 files") {
		t.Fatalf("block:\n%s", b)
	}
}

// TestScratchIndexCutShortShowsTheLabel: an index cut short, with none
// running, has no counts to show.
func TestScratchIndexCutShortShowsTheLabel(t *testing.T) {
	s := newScratch(t)
	x, y := s.goRepo("x"), s.goRepo("y")
	a := s.open(s.folder, x, y)
	rx := rc(t, a, x)
	index(t, rx)
	index(t, rc(t, a, y))
	writeTree(t, x, map[string]string{"x/b.go": "package x\n\nfunc B() {}\n"})
	src := newGated(graph.FSSource{Root: x}, true)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- rx.Graph.ReconcileIfStale(ctx, x, src, graph.WorktreeToken(x, "")) }()
	await(t, "the refresh", src.entered)
	cancel()
	close(src.gate)
	if err := await(t, "the refresh", ran); err == nil {
		t.Fatal("the refresh was meant to be cut short")
	}
	b := start(t, a, "s", "startup")
	if !strings.Contains(b, "\nx\n") || strings.Contains(b, "x: 0 files") || strings.Contains(b, "- x:") ||
		!strings.Contains(b, "\ny: 1 files, 1 symbols\n") || !strings.Contains(b, "- y: 1 files") {
		t.Fatalf("block:\n%s", b)
	}
}

// TestScratchMarkdownRepoAddsNoWait: a repo holding nothing to index adds no
// wait, no map and no indexing line; its one line names it with zero counts.
func TestScratchMarkdownRepoAddsNoWait(t *testing.T) {
	s := newScratch(t)
	second, docs := s.goRepo("second"), s.repo("docs", map[string]string{"README.md": "# d"})
	a := s.open(s.folder, second, docs)
	index(t, rc(t, a, second))
	var mu sync.Mutex
	var calls []string
	a.StartIndex = func(wt string) (<-chan error, error) {
		mu.Lock()
		calls = append(calls, wt)
		mu.Unlock()
		done := make(chan error, 1)
		done <- fmt.Errorf("no index")
		return done, nil
	}
	b := startOrGiveUp(t, a, "s", "startup", func() {})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(b, "\ndocs: 0 files, 0 symbols\n") || strings.Contains(b, "- docs:") ||
		strings.Contains(b, "docs: "+indexingLine) || strings.Contains(b, "docs: "+unbuiltLine) || slices.Contains(calls, docs) {
		t.Fatalf("calls %v, block:\n%s", calls, b)
	}
}

// TestScratchRosterRowFailureIsReported: a roster repo's session row that
// cannot be written is reported, and the block and Stop are still delivered.
func TestScratchRosterRowFailureIsReported(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("r")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	remember(t, rr, memory.KindDecision, "R-DECISION", "x")
	ctx := context.Background()
	stop := func(id string) Result {
		t.Helper()
		s.clk.Advance(time.Second)
		res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	start(t, a, "ref", "startup")
	want := stop("ref")

	if _, err := rr.Store.DB.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: KindSessionStart, SessionID: "s", Source: "startup"}
	var out strings.Builder
	err := RunHook(ctx, Opened(a), fixedAdapter{ev}, KindSessionStart, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), r) || !strings.Contains(out.String(), "R-DECISION") {
		t.Fatalf("err %v, output %s", err, out.String())
	}
	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s", Source: "compact"})
	if err != nil || !strings.Contains(res.Context, "R-DECISION") || res.Failure == nil || !strings.Contains(res.Failure.Error(), "refused") {
		t.Fatalf("err %v, failure %v, block:\n%s", err, res.Failure, res.Context)
	}
	got := stop("s")
	if got.Failure == nil || !strings.Contains(got.Failure.Error(), "refused") || got.SystemMessage != want.SystemMessage || got.SystemMessage == "" {
		t.Fatalf("stop %+v, want message %q", got, want.SystemMessage)
	}
	var ended bool
	if err := a.Store.DB.QueryRow(`SELECT ended_at IS NOT NULL FROM sessions WHERE id='s'`).Scan(&ended); err != nil || !ended {
		t.Fatalf("folder row not ended: %v", err)
	}
}

// scratchLate is a folder app serving a repo whose index another process holds
// at its listing.
type scratchLate struct {
	s       *scratch
	a       *app.App
	r       string
	rr      *app.RepoContext
	x, y    string
	src     gatedSource
	indexed chan error
	cancel  context.CancelFunc
	once    sync.Once
}

func newScratchLate(t *testing.T) *scratchLate {
	t.Helper()
	s := newScratch(t)
	r := s.repo("recarto", map[string]string{
		"pkg/x.go": "package pkg\n\nfunc XBase() {}\n",
		"pkg/y.go": "package pkg\n\nfunc YBase() {}\n",
	})
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	s.clk.Advance(time.Second)
	src := newGated(graph.FSSource{Root: r}, false)
	indexed := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { indexed <- rr.Graph.ReconcileIfStale(ctx, r, src, "other-token") }()
	await(t, "index", src.entered)
	return &scratchLate{s: s, a: a, r: r, rr: rr,
		x: filepath.Join(r, "pkg", "x.go"), y: filepath.Join(r, "pkg", "y.go"),
		src: src, indexed: indexed, cancel: cancel}
}

func (f *scratchLate) release() {
	f.s.t.Helper()
	f.once.Do(func() {
		close(f.src.gate)
		if err := await(f.s.t, "index", f.indexed); err != nil {
			f.s.t.Fatal(err)
		}
	})
}

func TestScratchLateIndexedEditShowsAtTheFirstStartAfterIt(t *testing.T) {
	f := newScratchLate(t)
	start(t, f.a, "S1", "startup")
	editIn(t, f.s, f.a, "S1", f.x, "Extra")
	f.s.stop(f.a, "S1")
	start(t, f.a, "S2", "startup")
	f.release()
	editIn(t, f.s, f.a, "S2", f.y, "Other")
	f.s.stop(f.a, "S2")
	block := start(t, f.a, "S3", "startup")
	for _, want := range []string{"Extra", "Other"} {
		if !strings.Contains(block, want) {
			t.Errorf("S3's block lacks %s:\n%s", want, block)
		}
	}
	if got := toolRead(t, f.a, f.rr); got["Extra"] != "added" || got["Other"] != "added" {
		t.Errorf("the tool in S3 = %v; want Extra and Other", got)
	}
}

func TestScratchChangeIndexedWhileTheBlockIsBuiltShowsOnce(t *testing.T) {
	f := newScratchLate(t)
	start(t, f.a, "S1", "startup")
	editIn(t, f.s, f.a, "S1", f.x, "Extra")
	f.s.stop(f.a, "S1")
	start(t, f.a, "S2", "startup")
	editIn(t, f.s, f.a, "S2", f.y, "Other")
	f.s.stop(f.a, "S2")
	// S3's start reads its creation marks, then the background index records
	// both edits before the block reads.
	f.a.IndexIfCode = func(string) { f.release() }
	block := start(t, f.a, "S3", "startup")
	for _, want := range []string{"Extra", "Other"} {
		if !strings.Contains(block, want) {
			t.Errorf("S3's block lacks %s:\n%s", want, block)
		}
	}
	if got := toolRead(t, f.a, f.rr); got["Extra"] != "added" {
		t.Errorf("the tool in S3 = %v; want Extra", got)
	}
	editIn(t, f.s, f.a, "S3", f.y, "S3s")
	f.s.stop(f.a, "S3") // its open row takes the Stop's raised mark
	block4 := start(t, f.a, "S4", "startup")
	if strings.Contains(block4, "Extra") || !strings.Contains(block4, "S3s") {
		t.Errorf("S4's block = want S3s without Extra:\n%s", block4)
	}
}

func TestScratchDeferredRepoRowKeepsItsMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		warm bool
	}{{"warm", true}, {"Stop", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScratch(t)
			r := s.goRepo("recarto")
			a := s.open(s.folder, r)
			rr := rc(t, a, r)
			index(t, rr)
			start(t, a, "S1", "startup")
			editIn(t, s, a, "S1", filepath.Join(r, "recarto", "a.go"), "Extra")
			s.stop(a, "S1")

			release := holdWriter(t, rr.Store)
			giveUp := sync.OnceFunc(release)
			block := startOrGiveUp(t, a, "S2", "startup", giveUp)
			if !strings.Contains(block, "Extra") {
				t.Fatalf("S2's block lacks Extra:\n%s", block)
			}
			if rows := sessionRows(t, rr, "S2"); len(rows) != 0 {
				t.Fatalf("S2 got a repo row while the writer was held: %+v", rows)
			}
			release()
			if tc.warm {
				if err := RecordRosterSessions(context.Background(), a, "S2"); err != nil {
					t.Fatal(err)
				}
				if rows := sessionRows(t, rr, "S2"); len(rows) != 1 {
					t.Fatalf("warm rows = %+v", rows)
				}
			}
			editIn(t, s, a, "S2", filepath.Join(r, "recarto", "a.go"), "Other")
			s.stop(a, "S2")
			if rows := sessionRows(t, rr, "S2"); len(rows) != 1 || !rows[0].end.Valid {
				t.Fatalf("S2's ended row = %+v", rows)
			}
			if rec := readRepoRecords(pendingDir(rr), "S2"); rec.mok || rec.gok {
				t.Errorf("S2's pending end is still recorded")
			}
			block3 := start(t, a, "S3", "startup")
			if strings.Contains(block3, "Extra") || !strings.Contains(block3, "Other") {
				t.Errorf("S3's block = want Other without Extra:\n%s", block3)
			}
		})
	}
}

func TestScratchNextStartBeforeTheRowIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  bool
	}{{"no row", false}, {"a row with its start's mark", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScratchLate(t)
			start(t, f.a, "S1", "startup")
			editIn(t, f.s, f.a, "S1", f.x, "Extra")
			f.s.stop(f.a, "S1")
			start(t, f.a, "S2", "startup")
			if !tc.row {
				f.release()
			}
			editIn(t, f.s, f.a, "S2", f.y, "Other")
			f.s.stop(f.a, "S2")

			if tc.row {
				// S3 has its row already; the held index records Extra and Other
				// after that row's mark and before S3's block reads.
				f.a.IndexIfCode = func(string) { f.release() }
				block := start(t, f.a, "S3", "startup")
				if !strings.Contains(block, "Extra") || !strings.Contains(block, "Other") {
					t.Fatalf("S3's block lacks Extra or Other:\n%s", block)
				}
				if rows := sessionRows(t, f.rr, "S3"); len(rows) != 1 || rows[0].end.Valid {
					t.Fatalf("S3's open row = %+v", rows)
				}
			} else {
				release := holdWriter(t, f.rr.Store)
				block := startOrGiveUp(t, f.a, "S3", "startup", sync.OnceFunc(release))
				if !strings.Contains(block, "Extra") || !strings.Contains(block, "Other") {
					t.Fatalf("S3's block lacks Extra or Other:\n%s", block)
				}
				if rows := sessionRows(t, f.rr, "S3"); len(rows) != 0 {
					t.Fatalf("S3 got a repo row while the writer was held: %+v", rows)
				}
				release()
			}

			editIn(t, f.s, f.a, "S3", f.y, "Z")
			release := holdWriter(t, f.rr.Store)
			f.s.stop(f.a, "S3") // its end waits in a record
			if rec := readRepoRecords(pendingDir(f.rr), "S3"); !rec.mok && !rec.gok {
				t.Fatal("S3's Stop left no pending end")
			}
			block4 := startOrGiveUp(t, f.a, "S4", "startup", sync.OnceFunc(release))
			if strings.Contains(block4, "Extra") || strings.Contains(block4, "Other") || !strings.Contains(block4, "Z") {
				t.Errorf("S4's block = want Z without Extra or Other:\n%s", block4)
			}
			before := toolRead(t, f.a, f.rr)
			if before["Extra"] != "" || before["Z"] != "added" {
				t.Errorf("the tool in S4 = %v; want Z without Extra", before)
			}
			release()
			if err := endPendingRepoEnds(context.Background(), f.a); err != nil {
				t.Fatal(err)
			}
			if rows := sessionRows(t, f.rr, "S3"); len(rows) != 1 || !rows[0].end.Valid {
				t.Fatalf("S3's ended row after recovery = %+v", rows)
			}
			if after := toolRead(t, f.a, f.rr); !mapsSame(after, before) {
				t.Errorf("the tool changed after recovery: before %v, after %v", before, after)
			}
		})
	}
}

func TestScratchOlderConversationStartedAgain(t *testing.T) {
	f := newScratchLate(t)
	start(t, f.a, "S1", "startup")
	editIn(t, f.s, f.a, "S1", f.x, "Extra")
	f.s.stop(f.a, "S1")
	start(t, f.a, "S2", "startup")
	editIn(t, f.s, f.a, "S2", f.y, "Other")
	f.s.stop(f.a, "S2")
	start(t, f.a, "S3", "startup")
	f.s.stop(f.a, "S3")
	f.release()

	block := start(t, f.a, "S2", "compact")
	for _, want := range []string{"Extra", "Other"} {
		if !strings.Contains(block, want) {
			t.Errorf("S2's compact block lacks %s:\n%s", want, block)
		}
	}
	want := []string{"Extra", "Other"}
	if got := sortedNames(toolRead(t, f.a, f.rr)); !slices.Equal(got, want) {
		t.Errorf("the tool after S2's block = %v; want %v", got, want)
	}
	if err := RecordRosterSessions(context.Background(), f.a, "S2"); err != nil {
		t.Fatal(err)
	}
	if got := sortedNames(toolRead(t, f.a, f.rr)); !slices.Equal(got, want) {
		t.Errorf("the tool after warm = %v; want %v", got, want)
	}
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func mapsSame(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestScratchPlainGuardTakesItsFolderEnd(t *testing.T) {
	s := newScratch(t)
	r := s.goRepo("recarto")
	a := s.open(s.folder, r)
	rr := rc(t, a, r)
	index(t, rr)
	start(t, a, "S1", "startup")
	s.stop(a, "S1")
	start(t, a, "S2", "startup")
	editIn(t, s, a, "S2", filepath.Join(r, "recarto", "a.go"), "Other")
	s.stop(a, "S2")

	release := holdWriter(t, rr.Store)
	block := startOrGiveUp(t, a, "S3", "startup", sync.OnceFunc(release))
	if !strings.Contains(block, "Other") {
		t.Fatalf("S3's block lacks Other:\n%s", block)
	}
	release()
	publishDeadEnd(t, s, a, r, "S3")

	// W is journaled after F's time and belongs to no session.
	s.clk.Advance(time.Second)
	appendFunc(t, filepath.Join(r, "recarto", "a.go"), "W")
	if err := rr.Graph.Reconcile(context.Background(), r, graph.FSSource{Root: r}); err != nil {
		t.Fatal(err)
	}
	s.clk.Advance(time.Second)
	mark, err := graph.JournalMark(context.Background(), rr.Store.DB)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := s.clk.Now().UnixMilli()
	pending, err := pendingIn(rr, graph.Session{ID: "S4", StartedAt: now,
		Mark: sql.NullInt64{Int64: mark, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	rng, ok, err := rr.Graph.RecentRange(ctx, r, pending, now, 0)
	if err != nil || !ok {
		t.Fatalf("RecentRange = %v, %v", ok, err)
	}
	var s2 int64
	if err := rr.Store.DB.QueryRow(`SELECT started_at FROM sessions WHERE id='S2'`).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if rng.Since != s2 {
		t.Errorf("baseline = %d; want S2's start %d", rng.Since, s2)
	}
	got := map[string]bool{}
	for _, d := range rng.Deltas {
		got[d.QualifiedName] = true
	}
	if !got["Other"] || !got["W"] {
		t.Errorf("the range = %v; want Other and W", got)
	}
}
