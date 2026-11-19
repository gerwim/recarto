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
	defer rows.Close()
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

// editAdd appends a function to file in r's worktree and reports the edit.
func (s *scratch) editAdd(a *app.App, file, fn string) {
	s.t.Helper()
	s.clk.Advance(time.Second)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, "\nfunc %s() {}\n", fn); err != nil {
		s.t.Fatal(err)
	}
	_ = f.Close()
	handle(s.t, a, Event{Kind: KindPostEdit, SessionID: "s", FilePath: file})
}

// stop ends a turn of session id.
func (s *scratch) stop(a *app.App, id string) {
	s.t.Helper()
	s.clk.Advance(time.Second)
	handle(s.t, a, Event{Kind: KindStop, SessionID: id, StopActive: true})
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
	defer rr.Close()
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
			second, big, md := s.goRepo("second"), s.goRepo("big"), s.repo("docs", map[string]string{"README.md": "# d"})
			repos := []string{second, big, md}
			if order == "big first" {
				repos = []string{big, second, md}
			}
			a := s.open(s.folder, repos...)
			remember(t, rc(t, a, second), memory.KindLearning, "NoSuchHelper wraps the retries", "x", memory.Ref{Type: memory.RefSymbol, Value: "NoSuchHelper"})
			src := newGated(graph.NewSource(big), false)
			secondDone := make(chan struct{})
			var secondOnce sync.Once
			ch := make(chan struct{})
			go func() {
				<-secondDone
				<-src.entered
				close(ch)
			}()
			endWaitWhen(t, ch)
			bigRan := make(chan error, 1)
			var mu sync.Mutex
			starts := map[string]int{}
			a.StartIndex = func(wt string) (<-chan error, error) {
				mu.Lock()
				starts[wt]++
				mu.Unlock()
				done := make(chan error, 1)
				x := rc(t, a, wt)
				go func() {
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
				}()
				return done, nil
			}
			b := start(t, a, "s", "startup")
			for _, w := range []string{"- second: 1 files", "\nbig: " + indexingLine, "\ndocs: 0 files, 0 symbols\n", "references code not present in this worktree"} {
				if !strings.Contains(b, w) {
					t.Errorf("block lacks %q", w)
				}
			}
			if strings.Contains(b, "- big:") || starts[md] != 0 || starts[big] != 1 || starts[second] != 1 {
				t.Errorf("starts %v", starts)
			}
			if t.Failed() {
				t.Fatalf("block:\n%s", b)
			}
			close(src.gate)
			if err := await(t, "big's index", bigRan); err != nil {
				t.Fatal(err)
			}
			// Nothing waits now; an open deadline keeps the folder's code check from running out.
			endWaitWhen(t, make(chan struct{}))
			if b := start(t, a, "s2", "startup"); !strings.Contains(b, "- big: 1 files") || strings.Contains(b, indexingLine) {
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
	got := make(chan string, 1)
	go func() {
		res, _ := Handle(context.Background(), a, Event{Kind: KindSessionStart, SessionID: "s", Source: "startup"})
		got <- res.Context
	}()
	if b := await(t, "the start", got); !strings.Contains(b, alsoServing([]string{"recarto"})) {
		t.Fatalf("block:\n%s", b)
	}
	if rows := sessionRows(t, rr, "s"); len(rows) != 0 {
		t.Fatalf("a row written past the held writer: %+v", rows)
	}
	res := make(chan string, 1)
	go func() {
		r, _ := Handle(context.Background(), a, Event{Kind: KindSessionStart, SessionID: "s", Source: "resume"})
		res <- r.Context
	}()
	await(t, "a resume while the writer is held", res)

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
	yDone := make(chan struct{})
	endWaitWhen(t, yDone)
	a.StartIndex = func(wt string) (<-chan error, error) {
		done := make(chan error, 1)
		if wt == y {
			go func() {
				index(t, ry)
				close(yDone)
				done <- nil
			}()
		}
		return done, nil
	}
	got := make(chan string, 1)
	go func() {
		res, _ := Handle(context.Background(), a, Event{Kind: KindSessionStart, SessionID: "s", Source: "startup"})
		got <- res.Context
	}()
	b := await(t, "the start while x's writer is held", got)
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
