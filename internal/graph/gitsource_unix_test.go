//go:build !windows

package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/gitwrap"
)

// markers returns the distinct markers of calls, "" for unmarked ones.
func markers(calls []gitwrap.Call) []string {
	var out []string
	for _, c := range calls {
		if !slices.Contains(out, c.Marker) {
			out = append(out, c.Marker)
		}
	}
	return out
}

func committedRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	for rel, src := range files {
		writeFile(t, dir, rel, src)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir
}

// failFlag creates or removes the flag file p.
func failFlag(t *testing.T, p string, on bool) {
	t.Helper()
	var err error
	if on {
		err = os.WriteFile(p, nil, 0o644)
	} else {
		err = os.Remove(p)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitSourceFailedListing(t *testing.T) {
	flags := t.TempDir()
	t.Setenv("PATH", gitwrap.Script(t, `case " $* " in
*" ls-files "*) [ -e '`+flags+`/fail-all' ] && exit 1 ;;
*" rev-parse --git-dir "*) [ -e '`+flags+`/fail-probe' ] && exit 1 ;;
esac`)+string(os.PathListSeparator)+os.Getenv("PATH"))
	failFlag(t, filepath.Join(flags, "fail-all"), true)
	repo := committedRepo(t, map[string]string{"a.go": "package a"})
	if _, err := NewSource(repo).List(); err == nil {
		t.Error("inside git: a failed listing must fail the list")
	}
	vendored := committedRepo(t, map[string]string{"vendor/a.go": "package a"})
	failFlag(t, filepath.Join(flags, "fail-probe"), true)
	if _, err := NewSource(vendored).List(); err == nil {
		t.Error("inside git with git failing: the walk would skip vendor/ and find nothing; want an error")
	}
	plain := t.TempDir()
	writeFile(t, plain, "a.go", "package a")
	got, err := NewSource(plain).List()
	if err != nil || len(got) != 1 || got[0].Path != "a.go" {
		t.Errorf("outside git: want the walk's a.go, got %v, %v", got, err)
	}
}

func TestIndexAttemptsMarkTheirGitCalls(t *testing.T) {
	ctx := context.Background()
	repo := committedRepo(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	r := gitwrap.NewRecipe(t, repo, AttemptEnv)
	t.Setenv("PATH", r.PATH())
	t.Setenv(AttemptEnv, "")
	e := newTestEngine(t)

	if code, err := HasCode(ctx, repo, false); err != nil || !code {
		t.Fatalf("HasCode = %v, %v", code, err)
	}
	if m := markers(r.Calls(t)); !slices.Equal(m, []string{""}) {
		t.Errorf("discovery: markers = %q, want only unmarked calls", m)
	}

	token := WorktreeToken(repo, "")
	before := len(r.Calls(t))
	if err := e.ReconcileIfStale(ctx, repo, NewSource(repo), token); err != nil {
		t.Fatal(err)
	}
	m1 := markers(r.Calls(t)[before:])
	if len(m1) != 1 || m1[0] == "" {
		t.Fatalf("ReconcileIfStale: markers = %q, want one attempt's marker", m1)
	}

	writeFile(t, repo, "a.go", "package a\nfunc A() {}\nfunc B() {}\n")
	before = len(r.Calls(t))
	if err := e.ReconcileIfIdle(ctx, repo, NewSource(repo), func(a string) string { return WorktreeToken(repo, a) }); err != nil {
		t.Fatal(err)
	}
	calls := r.Calls(t)[before:]
	m2 := markers(calls)
	if len(m2) != 1 || m2[0] == "" || m2[0] == m1[0] {
		t.Fatalf("ReconcileIfIdle: markers = %q, want one marker of its own (not %q)", m2, m1[0])
	}
	for _, want := range []string{"rev-parse HEAD", "status --porcelain", "ls-files"} {
		if !slices.ContainsFunc(calls, func(c gitwrap.Call) bool { return strings.Contains(c.Args, want) }) {
			t.Errorf("ReconcileIfIdle made no marked %q call: %v", want, calls)
		}
	}
}

func TestDiscoveryIsNotHeldByAnAttempt(t *testing.T) {
	ctx := context.Background()
	repo := committedRepo(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	r := gitwrap.NewRecipe(t, repo, AttemptEnv)
	t.Setenv("PATH", r.PATH())
	t.Setenv(AttemptEnv, "")
	e := newTestEngine(t)
	r.Hold(t, true)
	done := make(chan error, 1)
	go func() { done <- e.ReconcileIfStale(ctx, repo, NewSource(repo), "tok") }()
	// The attempt must end before the temp dirs go, also when the test fails early.
	finished := false
	t.Cleanup(func() {
		if !finished {
			r.Hold(t, false)
			<-done
		}
	})

	held := func(c gitwrap.Call) bool { return c.Marker != "" && strings.Contains(c.Args, "ls-files") }
	deadline := time.Now().Add(time.Minute)
	for !slices.ContainsFunc(r.Calls(t), held) {
		if time.Now().After(deadline) {
			t.Fatal("the attempt never reached its listing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := len(r.Calls(t))

	if code, err := HasCode(ctx, repo, false); err != nil || !code {
		t.Fatalf("HasCode during the held attempt = %v, %v", code, err)
	}
	if _, err := e.SearchContent(ctx, repo, "package", ContentSearchOpts{}); err != nil {
		t.Fatalf("SearchContent during the held attempt: %v", err)
	}
	for _, c := range r.Calls(t)[before:] {
		if c.Marker != "" {
			t.Errorf("discovery call carries the attempt's marker: %+v", c)
		}
	}

	r.Hold(t, false)
	err := <-done
	finished = true
	if err != nil {
		t.Fatal(err)
	}
}

func TestHasCodeStopsAtItsDeadline(t *testing.T) {
	repo := committedRepo(t, map[string]string{"a.go": "package a"})
	flags := t.TempDir()
	// Lets the orphaned subshell exit before the temp dirs go.
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(flags, "release"), nil, 0o644) })
	t.Setenv("PATH", gitwrap.Script(t, `case " $* " in *" ls-files "*)
	( while [ ! -e '`+flags+`/release' ]; do sleep 0.01; done ) &
	: > '`+flags+`/started'
	wait ;;
esac`)+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := HasCode(ctx, repo, false)
		done <- err
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(flags, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listing never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled listing returned no error")
		}
	case <-time.After(time.Minute):
		t.Fatal("HasCode waited for the wrapper's child after its deadline")
	}
}
