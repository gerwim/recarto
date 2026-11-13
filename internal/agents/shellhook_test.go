package agents

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/memory"
)

// contextAdapter decodes every payload to ev and writes the result's context.
type contextAdapter struct{ ev Event }

func (c contextAdapter) Decode(kind EventKind, _ io.Reader) (Event, error) {
	ev := c.ev
	ev.Kind = kind
	return ev, nil
}

func (contextAdapter) Encode(_ EventKind, out io.Writer, res Result) error {
	_, err := io.WriteString(out, res.Context)
	return err
}

// newTreeApp serves a real directory (shellTree) as the primary repo, so the
// shell parser finds its files on disk.
func newTreeApp(t *testing.T) (*app.App, string) {
	t.Helper()
	dir := shellTree(t)
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{CWD: dir, Runner: fakeGit{branch: "main", root: dir}, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, dir
}

// shellHook runs a shell tool call through RunHook, as a host hook does, and
// returns the context it injects.
func shellHook(t *testing.T, a *app.App, ev Event) string {
	t.Helper()
	var out bytes.Buffer
	if err := RunHook(context.Background(), Opened(a), contextAdapter{ev}, KindShell, nil, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func recordWidgetBug(t *testing.T, a *app.App) {
	t.Helper()
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow — fix: clamp",
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
}

// Each shell search injects exactly what the Grep/Glob tool gives for its
// pattern — symbols and the matching bug — plus the first-search nudge.
func TestShellSearchGetsTheGrepHint(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	src := []byte("package p\n\nfunc Widget() {}\n")
	if err := a.Graph.IndexFile(ctx, dir, "widget.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	recordWidgetBug(t, a)
	for i, tc := range []struct{ command, pattern string }{
		{`grep -rn "Widget" internal/`, "Widget"},
		{`rg Widget`, "Widget"},
		{`git grep -n Widget`, "Widget"},
		{`find . -name "*Widget*.go"`, "*Widget*.go"},
		{`cd internal && rg -t go Widget | head -20`, "Widget"},
		{`grep --include=*.go -rn Widget .`, "Widget"},
	} {
		session := string(rune('a' + i))
		grep, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: "grep-" + session, Pattern: tc.pattern})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(grep.Context, "Widget overflow") || !strings.Contains(grep.Context, "widget.go:3") {
			t.Fatalf("Grep/Glob hint for %q lacks the symbol or bug:\n%s", tc.pattern, grep.Context)
		}
		got := shellHook(t, a, Event{SessionID: session, Command: tc.command, CWD: dir})
		if want := grep.Context + "\n" + shellSearchNudge; got != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.command, got, want)
		}
	}
}

// Several -e patterns are each searched; flag values are not.
func TestShellSearchEveryPattern(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	for _, title := range []string{"Foo crashes", "Bar leaks", "three misfires"} {
		if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindBug, Title: title, Body: title, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	got := shellHook(t, a, Event{SessionID: "s", Command: `grep -A 3 -e Foo -e Bar .`, CWD: dir})
	for _, want := range []string{"Foo crashes", "Bar leaks"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "three misfires") {
		t.Errorf("the -A value was searched:\n%s", got)
	}
}

// The nudge rides with the first shell search of a session only, even when
// that search finds nothing; Grep/Glob tool hints never carry it.
func TestShellSearchNudgeOncePerSession(t *testing.T) {
	a, dir := newTreeApp(t)
	recordWidgetBug(t, a)
	if got := shellHook(t, a, Event{SessionID: "s", Command: `rg NothingMatches`, CWD: dir}); got != shellSearchNudge {
		t.Errorf("first shell search = %q, want just the nudge", got)
	}
	if got := shellHook(t, a, Event{SessionID: "s", Command: `find . -name '*.go'`, CWD: dir}); got != "" {
		t.Errorf("second shell search = %q, want nothing", got)
	}
	got := shellHook(t, a, Event{SessionID: "s", Command: `rg Widget`, CWD: dir})
	if !strings.Contains(got, "Widget overflow") || strings.Contains(got, "search_content") {
		t.Errorf("later search must give its hint without the nudge:\n%s", got)
	}
	grep, err := Handle(context.Background(), a, Event{Kind: KindAugment, SessionID: "t", Pattern: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(grep.Context, "search_content") {
		t.Errorf("a Grep tool hint carries the nudge:\n%s", grep.Context)
	}
	if got := shellHook(t, a, Event{Command: `rg Nothing`, CWD: dir}); got != "" {
		t.Errorf("a session-less search cannot promise the nudge once: %q", got)
	}
}

// A search or view whose output goes to a file is a write: it injects
// nothing and leaves the session's nudge, bug and view dedup untouched.
func TestShellRedirectedOutputIsSilent(t *testing.T) {
	a, dir := newTreeApp(t)
	recordWidgetBug(t, a)
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindTodo, Title: "split f.go", Body: "split f.go", Source: "cli", Branch: a.Git.Branch,
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "f.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`rg Widget > results.txt`, `find . -name '*Widget*' > results.txt`,
		`{ cat f.go; } > copied.go`, `(head f.go) > copied.go`, `cat f.go | head > copied.go`,
	} {
		if got := shellHook(t, a, Event{SessionID: "s", Command: command, CWD: dir}); got != "" {
			t.Errorf("%s: injected %q", command, got)
		}
	}
	got := shellHook(t, a, Event{SessionID: "s", Command: `rg Widget`, CWD: dir})
	if !strings.Contains(got, "Widget overflow") || !strings.Contains(got, "search_content") {
		t.Errorf("first printed search after redirected ones lacks its bug or the nudge:\n%s", got)
	}
	if got := shellHook(t, a, Event{SessionID: "s", Command: `cat f.go`, CWD: dir}); !strings.Contains(got, "split f.go") {
		t.Errorf("first printed view after redirected ones lacks its todo:\n%s", got)
	}
}

// Grep/Glob and shell searches share the per-session repeat reminder.
func TestShellSearchRepeatSharedWithGrep(t *testing.T) {
	a, dir := newTreeApp(t)
	recordWidgetBug(t, a)
	if _, err := Handle(context.Background(), a, Event{Kind: KindAugment, SessionID: "s", Pattern: "Widget"}); err != nil {
		t.Fatal(err)
	}
	got := shellHook(t, a, Event{SessionID: "s", Command: `rg Widget`, CWD: dir})
	if !strings.Contains(got, `already searched "Widget"`) || strings.Contains(got, "Widget overflow") {
		t.Errorf("shell repeat of a Grep search:\n%s", got)
	}
}

// Every shell file view surfaces the memories anchored to the file once per
// session — without a Read's counters, repeat warning or large-file nudge.
func TestShellViewsSurfaceAnchoredMemories(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	for _, m := range []memory.NewMemory{
		{Kind: memory.KindBug, Title: "f.go panics on nil"},
		{Kind: memory.KindDoNotRepeat, Title: "never inline f.go helpers"},
		{Kind: memory.KindTodo, Title: "split f.go"},
	} {
		m.Body, m.Source, m.Branch = m.Title, "cli", a.Git.Branch
		m.Refs = []memory.Ref{{Type: memory.RefFile, Value: "f.go"}}
		if _, _, _, err := a.Engine.Record(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	big := strings.Repeat("// padding\n", 4000) // a large-file nudge would fire for a Read
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("package x\n\nfunc Foo() {}\n"+big), 0o644); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if err := a.Graph.IndexFile(ctx, dir, "f.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	commands := []string{
		`cat f.go`, `head -n 50 f.go`, `tail f.go`, `sed -n '10,40p' f.go`, `cat f.go | head`, `grep -n Foo f.go`,
		`cd internal && cat ../f.go`, `cat ` + filepath.ToSlash(filepath.Join(dir, "f.go")),
	}
	if vol := filepath.VolumeName(dir); runtime.GOOS == "windows" && len(vol) == 2 {
		// Git Bash spells C:\x\f.go as /c/x/f.go.
		commands = append(commands, `cat /`+strings.ToLower(vol[:1])+filepath.ToSlash(filepath.Join(dir, "f.go")[2:]))
	}
	for _, command := range commands {
		first := shellHook(t, a, Event{SessionID: command, Command: command, CWD: dir})
		for _, want := range []string{"f.go panics on nil", "never inline f.go helpers", "split f.go"} {
			if !strings.Contains(first, want) {
				t.Errorf("%s: missing %q:\n%s", command, want, first)
			}
		}
		for _, banned := range []string{"you already have", "get_file_outline", "search_content"} {
			if strings.Contains(first, banned) {
				t.Errorf("%s: a view must not add %q:\n%s", command, banned, first)
			}
		}
		if again := shellHook(t, a, Event{SessionID: command, Command: command, CWD: dir}); again != "" {
			t.Errorf("%s: repeated in the session: %q", command, again)
		}
	}
	for _, metric := range []string{"reads", "repeated_reads_blocked", "large_reads_nudged"} {
		if n, _ := a.Ledger.Get(ctx, "lifetime", metric); n != 0 {
			t.Errorf("lifetime %s = %d, want 0", metric, n)
		}
	}
}

// A view and a Read dedup against each other, and against session start.
func TestShellViewDedupsWithRead(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "f.go panics on nil", Body: "x", Source: "cli", Branch: a.Git.Branch,
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "f.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	read, err := Handle(ctx, a, Event{Kind: KindPostRead, SessionID: "s", FilePath: filepath.Join(dir, "f.go")})
	if err != nil || !strings.Contains(read.Context, "f.go panics on nil") {
		t.Fatalf("read: %v\n%s", err, read.Context)
	}
	if got := shellHook(t, a, Event{SessionID: "s", Command: `cat f.go`, CWD: dir}); got != "" {
		t.Errorf("view after a Read repeated it: %q", got)
	}
	if got := shellHook(t, a, Event{SessionID: "t", Command: `cat f.go`, CWD: dir}); !strings.Contains(got, "f.go panics on nil") {
		t.Fatalf("view: %q", got)
	}
	read, err = Handle(ctx, a, Event{Kind: KindPostRead, SessionID: "t", FilePath: filepath.Join(dir, "f.go")})
	if err != nil || strings.Contains(read.Context, "f.go panics on nil") {
		t.Errorf("Read after a view repeated it: %v\n%s", err, read.Context)
	}
}

// A view of a file outside every served repo gives nothing.
func TestShellViewOutsideServedRepos(t *testing.T) {
	a, dir := newTreeApp(t)
	other := shellTree(t)
	if _, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindBug, Title: "f.go panics on nil", Body: "x", Source: "cli", Branch: a.Git.Branch,
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "f.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := shellHook(t, a, Event{SessionID: "s", Command: `cat ` + filepath.ToSlash(filepath.Join(other, "f.go")), CWD: dir}); got != "" {
		t.Errorf("view outside the served repos: %q", got)
	}
}

// From a repo-less scratch dir, views of a RECARTO_REPOS repo's file — by
// absolute path, after a cd, or ~-based — surface that repo's memories.
func TestShellViewSurfacesSiblingRepoMemories(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	repo1, scratch := filepath.Join(base, "repo1"), filepath.Join(base, "_scratch")
	for _, d := range []string{repo1, scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initGitRepo(t, repo1)
	ctx := context.Background()
	cdApp, err := app.Open(app.Config{CWD: repo1, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := cdApp.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindDecision, Title: "c.go stays tiny", Body: "keep c.go dependency-free",
		Branch: cdApp.Git.Branch, Worktree: cdApp.Git.Worktree, Source: "mcp",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a/b/c.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = cdApp.Close()

	t.Setenv("RECARTO_REPOS", repo1)
	a, err := app.Open(app.Config{CWD: scratch, Clock: clock.NewFake(time.UnixMilli(1_700_000_100_000))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	for _, command := range []string{
		`cat ` + filepath.ToSlash(filepath.Join(repo1, "a", "b", "c.go")),
		`cd ../repo1 && head -n 5 a/b/c.go`,
		`tail ~/repo1/a/b/c.go`,
	} {
		if got := shellHook(t, a, Event{SessionID: command, Command: command, CWD: scratch}); !strings.Contains(got, "c.go stays tiny") {
			t.Errorf("%s: sibling repo's anchored decision did not fire: %q", command, got)
		}
	}
}
