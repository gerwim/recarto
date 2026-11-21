package agents

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

// codeMapHeader opens the code map in the session-start block.
const codeMapHeader = "Code map (modules)"

// hang only catches a hang; no test waits it out on success.
const hang = time.Minute

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func openAt(t *testing.T, cfg app.Config) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	cfg.Clock = clock.NewFake(time.UnixMilli(1_700_000_000_000))
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	markServing(t, a)
	return a
}

// newCodeTreeApp opens an App whose worktree is a plain directory holding
// files, reported as a git repo by a fake git. Listing it in git fails, so it
// always counts as holding code.
func newCodeTreeApp(t *testing.T, files map[string]string) *app.App {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	a := openTreeApp(t, root)
	markServing(t, a)
	return a
}

// gitIn runs git in dir.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// openRepoApp opens an App in a real git repo holding files, committed.
func openRepoApp(t *testing.T, files map[string]string) *app.App {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	return openAt(t, app.Config{CWD: root})
}

// openNonGitApp opens an App in a directory outside git holding files.
func openNonGitApp(t *testing.T, files map[string]string) *app.App {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	a := openAt(t, app.Config{CWD: root})
	if !a.Git.NonGit {
		t.Fatalf("%s resolved inside git", root)
	}
	return a
}

// fakeStart is a.StartIndex for tests: it counts its calls and runs run, whose
// result is the started process's exit.
type fakeStart struct{ calls atomic.Int32 }

func (f *fakeStart) install(a *app.App, run func() error) {
	a.StartIndex = func(string) (<-chan error, error) {
		f.calls.Add(1)
		done := make(chan error, 1)
		go func() { done <- run() }()
		return done, nil
	}
}

// indexRun is what `recarto warm --only` does to a's worktree, from src. A
// fake git's worktree gets a test token; elsewhere git's own token is used.
func indexRun(a *app.App, src graph.Source) func() error {
	return func() error {
		return a.Graph.ReconcileIfIdle(context.Background(), a.Git.Worktree, src,
			func(attempt string) string {
				if _, fake := a.Runner.(fakeGit); fake {
					return "test-token"
				}
				return graph.WorktreeToken(a.Git.Worktree, attempt)
			})
	}
}

// endWaitWhen ends each first-index wait (and code check) once ch closes.
func endWaitWhen(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	prev := firstIndexDeadline
	firstIndexDeadline = func(ctx context.Context) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-ch:
				cancel()
			case <-c.Done():
			}
		}()
		return c, cancel
	}
	t.Cleanup(func() { firstIndexDeadline = prev })
}

// closed is a channel that is already closed.
func closed() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// gatedSource is a Source whose List (or Read) signals entered, then waits
// for gate to close.
type gatedSource struct {
	graph.Source
	onRead         bool
	entered, gate  chan struct{}
	enteredAlready *atomic.Bool
}

func newGated(src graph.Source, onRead bool) gatedSource {
	return gatedSource{Source: src, onRead: onRead, entered: make(chan struct{}), gate: make(chan struct{}), enteredAlready: &atomic.Bool{}}
}

func (g gatedSource) hold() {
	if g.enteredAlready.CompareAndSwap(false, true) {
		close(g.entered)
	}
	<-g.gate
}

func (g gatedSource) List() ([]graph.FileEntry, error) {
	if !g.onRead {
		g.hold()
	}
	return g.Source.List()
}

func (g gatedSource) Read(p string) ([]byte, error) {
	if g.onRead {
		g.hold()
	}
	return g.Source.Read(p)
}

// await returns what ch delivers, failing on a hang.
func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(hang):
		t.Fatalf("%s: hung", what)
		panic("unreachable")
	}
}

func recordNoSuchHelper(t *testing.T, a *app.App) {
	t.Helper()
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindLearning, Title: "NoSuchHelper wraps the retries", Body: "x",
		Branch: a.Git.Branch, Source: "cli",
		Refs: []memory.Ref{{Type: memory.RefSymbol, Value: "NoSuchHelper"}},
	}); err != nil {
		t.Fatal(err)
	}
}

// tierOf names what the block shows in the code map's place.
func tierOf(block string) string {
	switch {
	case strings.Contains(block, codeMapHeader):
		return "map"
	case strings.Contains(block, indexingLine):
		return "indexing"
	case strings.Contains(block, unbuiltLine):
		return "unbuilt"
	}
	return "none"
}

func wantTier(t *testing.T, block, want string) {
	t.Helper()
	if got := tierOf(block); got != want {
		t.Fatalf("tier = %s, want %s:\n%s", got, want, block)
	}
	if want == "map" && (strings.Contains(block, indexingLine) || strings.Contains(block, unbuiltLine)) {
		t.Fatalf("a line beside the map:\n%s", block)
	}
}

func TestSessionStartNothingToIndex(t *testing.T) {
	for name, a := range map[string]*app.App{
		"git, Java and Markdown": openRepoApp(t, map[string]string{"A.java": "class A {}", "README.md": "# r"}),
		"outside git, empty":     openNonGitApp(t, nil),
	} {
		t.Run(name, func(t *testing.T) {
			var f fakeStart
			f.install(a, func() error { return errors.New("not to be started") })
			wantTier(t, start(t, a, "s1", "startup"), "none")
			wantTier(t, start(t, a, "s2", "startup"), "none")
			if n := f.calls.Load(); n != 0 {
				t.Errorf("started %d indexes for a worktree with nothing to index", n)
			}
		})
	}
}

func TestSessionStartIndexFinishedWithinTheWait(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	recordNoSuchHelper(t, a)
	var f fakeStart
	f.install(a, indexRun(a, graph.FSSource{Root: a.Git.Worktree}))
	block := start(t, a, "s1", "startup")
	wantTier(t, block, "map")
	if !strings.Contains(block, "NoSuchHelper wraps the retries") || !strings.Contains(block, "references code not present in this worktree") {
		t.Errorf("the learning must be ranked against the new index, with its caveat:\n%s", block)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("started %d indexes, want 1", n)
	}
}

func TestSessionStartIndexStillRunning(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	src := newGated(graph.FSSource{Root: a.Git.Worktree}, false)
	endWaitWhen(t, src.entered)
	var f fakeStart
	ran := make(chan error, 1)
	f.install(a, func() error {
		err := indexRun(a, src)()
		ran <- err
		return err
	})

	wantTier(t, start(t, a, "s1", "startup"), "indexing")

	close(src.gate)
	if err := await(t, "the index", ran); err != nil {
		t.Fatal(err)
	}
	wantTier(t, start(t, a, "s2", "startup"), "map")
	if n := f.calls.Load(); n != 1 {
		t.Errorf("started %d indexes, want 1", n)
	}
}

// holdWriter holds st's writer until the returned release runs.
func holdWriter(t *testing.T, st *store.Store) (release func()) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+st.Path())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		_, _ = conn.ExecContext(ctx, "ROLLBACK") // nothing was written
		_ = conn.Close()
		_ = db.Close()
	}
	t.Cleanup(release)
	return release
}

func TestSessionStartIndexEndsWhileTheMarksWait(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	ctx := context.Background()
	recordNoSuchHelper(t, a)
	conv, err := store.Open(filepath.Join(t.TempDir(), "conv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conv.Close() })
	a.Ledger = ledger.New(conv)
	releaseConv := holdWriter(t, conv)

	src := newGated(graph.FSSource{Root: a.Git.Worktree}, false)
	endWaitWhen(t, src.entered)
	var f fakeStart
	ran := make(chan error, 1)
	f.install(a, func() error {
		err := indexRun(a, src)()
		ran <- err
		return err
	})
	life := ledger.New(a.Store)
	recalled := func() int64 {
		n, err := life.Get(ctx, "lifetime", "memories_recalled")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := recalled()

	type result struct {
		res Result
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := Handle(ctx, a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1", Source: "startup"})
		got <- result{res, err}
	}()
	// Recall comes after the tier's decision, which so saw the index running.
	deadline := time.Now().Add(hang)
	for recalled() == before {
		if time.Now().After(deadline) {
			t.Fatal("the hook never recalled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(src.gate)
	if err := await(t, "the index", ran); err != nil {
		t.Fatal(err)
	}
	releaseConv()

	r := await(t, "the hook", got)
	if r.err != nil {
		t.Fatal(r.err)
	}
	wantTier(t, r.res.Context, "map")
	if n, err := ledger.New(conv).Get(ctx, "session:s1", blockMetric); err != nil || n != 1 {
		t.Errorf("block marker = %d, %v; want the mark landed after the wait", n, err)
	}
}

func TestSessionStartFirstIndexNotRunning(t *testing.T) {
	for name, run := range map[string]func(chan struct{}) func() error{
		// The started process has not taken the lock when the wait ends.
		"process alive, no lock": func(stop chan struct{}) func() error {
			return func() error { <-stop; return nil }
		},
		"attempt failed": func(chan struct{}) func() error {
			return func() error { return errors.New("git ls-files: exit status 1") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newCodeTreeApp(t, map[string]string{"a.go": "package a\n"})
			endWaitWhen(t, closed())
			stop := make(chan struct{})
			defer close(stop)
			var f fakeStart
			f.install(a, run(stop))
			wantTier(t, start(t, a, "s1", "startup"), "unbuilt")
			if n := f.calls.Load(); n != 1 {
				t.Errorf("started %d indexes, want 1", n)
			}
		})
	}
}

func TestSessionStartJoinsAnotherProcesssIndex(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"a.go": "package a\n"})
	src := newGated(graph.FSSource{Root: a.Git.Worktree}, false)
	ran := make(chan error, 1)
	go func() { ran <- indexRun(a, src)() }()
	await(t, "the other index", src.entered)
	endWaitWhen(t, closed())

	wantTier(t, start(t, a, "s1", "startup"), "indexing")

	close(src.gate)
	if err := await(t, "the other index", ran); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStartAfterAFinishedIndex(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	ctx := context.Background()
	wt := a.Git.Worktree
	finishIndex(t, a, wt)
	var f fakeStart
	f.install(a, func() error { return errors.New("not to be started") })

	writeTree(t, wt, map[string]string{"lib/b.go": "package lib\n\nfunc B() {}\n"})
	wantTier(t, start(t, a, "s1", "startup"), "map")

	src := newGated(graph.FSSource{Root: wt}, true)
	rctx, cancel := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() { ran <- a.Graph.ReconcileIfStale(rctx, wt, src, "test-token-2") }()
	await(t, "the refresh", src.entered)
	wantTier(t, start(t, a, "s2", "startup"), "indexing")

	cancel()
	close(src.gate)
	if err := await(t, "the refresh", ran); err == nil {
		t.Fatal("the refresh was meant to be cut short")
	}
	wantTier(t, start(t, a, "s3", "startup"), "none")

	if n := f.calls.Load(); n != 0 {
		t.Errorf("started %d indexes for a worktree indexed once", n)
	}
}

// TestSessionStartReadsGitForCode: a finished index keeps its map while a
// stored code file is on disk; git decides once none is; the background index
// drops a file git stopped listing.
func TestSessionStartReadsGitForCode(t *testing.T) {
	finish := func(t *testing.T, a *app.App) {
		wt := a.Git.Worktree
		if err := a.Graph.ReconcileIfStale(context.Background(), wt, graph.NewSource(wt), graph.WorktreeToken(wt, "")); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		change func(t *testing.T, wt string)
		want   string
		next   string
	}{
		"untracked and ignored, still on disk": {func(t *testing.T, wt string) {
			gitIn(t, wt, "rm", "-q", "--cached", "a.go")
			writeTree(t, wt, map[string]string{".gitignore": "a.go\n"})
		}, "map", "none"},
		"deleted": {func(t *testing.T, wt string) {
			if err := os.Remove(filepath.Join(wt, "a.go")); err != nil {
				t.Fatal(err)
			}
		}, "none", "none"},
		"unchanged": {func(*testing.T, string) {}, "map", "map"},
	} {
		t.Run(name, func(t *testing.T) {
			a := openRepoApp(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
			finish(t, a)
			tc.change(t, a.Git.Worktree)
			wantTier(t, start(t, a, "s1", "startup"), tc.want)
			finish(t, a)
			wantTier(t, start(t, a, "s2", "startup"), tc.next)
		})
	}
}

// mapSymbols returns the symbol count of the block's first module.
func mapSymbols(t *testing.T, block string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^- \S+: \d+ files, (\d+) symbols$`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("no module line:\n%s", block)
	}
	n, _ := strconv.Atoi(m[1]) // the pattern only matches digits
	return n
}

func TestSessionStartOutsideGit(t *testing.T) {
	const a1 = "package api\n\nfunc A() {}\n"
	ctx := context.Background()
	t.Run("rows of an index that never finished", func(t *testing.T) {
		a := openNonGitApp(t, map[string]string{"api/a.go": a1})
		wt := a.Git.Worktree
		if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
			t.Fatal(err)
		}
		// A second index cut short leaves the rows unresolved.
		writeTree(t, wt, map[string]string{"api/b.go": "package api\n\nfunc B() {}\n"})
		src := newGated(graph.FSSource{Root: wt}, true)
		rctx, cancel := context.WithCancel(ctx)
		ran := make(chan error, 1)
		go func() { ran <- a.Graph.Reconcile(rctx, wt, src) }()
		await(t, "the cut-short index", src.entered)
		cancel()
		close(src.gate)
		if err := await(t, "the cut-short index", ran); err == nil {
			t.Fatal("the index was meant to be cut short")
		}

		var f fakeStart
		f.install(a, indexRun(a, graph.FSSource{Root: wt}))
		wantTier(t, start(t, a, "s1", "startup"), "map")
		if n := f.calls.Load(); n != 1 {
			t.Errorf("started %d indexes, want 1", n)
		}
	})
	t.Run("finished by an earlier version", func(t *testing.T) {
		a := openNonGitApp(t, map[string]string{"api/a.go": a1})
		if err := a.Graph.Reconcile(ctx, a.Git.Worktree, graph.FSSource{Root: a.Git.Worktree}); err != nil {
			t.Fatal(err)
		}
		var f fakeStart
		f.install(a, func() error { return errors.New("not to be started") })
		wantTier(t, start(t, a, "s1", "startup"), "map")
		if n := f.calls.Load(); n != 0 {
			t.Errorf("started %d indexes, want none", n)
		}
	})
	t.Run("a post-edit shows in the next map", func(t *testing.T) {
		a := openNonGitApp(t, map[string]string{"api/a.go": a1})
		wt := a.Git.Worktree
		if err := a.Graph.ReconcileIfStale(ctx, wt, graph.NewSource(wt), ""); err != nil {
			t.Fatal(err)
		}
		before := mapSymbols(t, start(t, a, "s1", "startup"))
		src := []byte(a1 + "\nfunc C() {}\n")
		writeTree(t, wt, map[string]string{"api/a.go": string(src)})
		if err := a.Graph.ReindexFileIncremental(ctx, wt, "api/a.go", src, 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
		if got := mapSymbols(t, start(t, a, "s2", "startup")); got != before+1 {
			t.Errorf("symbols = %d, want %d", got, before+1)
		}
	})
}

func TestSessionStartOnlyFullBlocksWait(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"a.go": "package a\n"})
	var f fakeStart
	f.install(a, func() error { return errors.New("git ls-files: exit status 1") })
	wantTier(t, start(t, a, "s1", "startup"), "unbuilt")
	wantTier(t, start(t, a, "s1", "resume"), "none")
	if n := f.calls.Load(); n != 1 {
		t.Errorf("after the resume: %d indexes started, want 1", n)
	}
	a.Features = off(features.CodeMap)
	wantTier(t, start(t, a, "s2", "startup"), "none")
	if n := f.calls.Load(); n != 1 {
		t.Errorf("code map off: %d indexes started, want 1", n)
	}
}

func TestSessionStartResumeDuringAWrite(t *testing.T) {
	a := newCodeTreeApp(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
	finishIndex(t, a, a.Git.Worktree)
	record(t, a, memory.KindLearning, "wal mode needs busy timeout", "")
	var f fakeStart
	f.install(a, func() error { return errors.New("not to be started") })
	if block := start(t, a, "s1", "startup"); !strings.Contains(block, "wal mode needs busy timeout") {
		t.Fatalf("the full block must show the learning:\n%s", block)
	}

	release := holdWriter(t, a.Store)
	got := make(chan string, 1)
	go func() {
		res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1", Source: "resume"})
		if err != nil {
			res.Context = "error: " + err.Error()
		}
		got <- res.Context
	}()
	if ctx := await(t, "the resume while another process writes", got); ctx != "" {
		t.Errorf("resume with nothing new: want no context, got %q", ctx)
	}
	release()
	if ctx := start(t, a, "s1", "resume"); ctx != "" {
		t.Errorf("the next resume: want no context, got %q", ctx)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("started %d indexes", n)
	}
}
