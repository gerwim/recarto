//go:build !windows

package graph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// gitShim is a git first on PATH that logs each call as "<marker>|<args>" and
// obeys flag files in its directory: fail-all fails every ls-files, fail-probe
// fails `rev-parse --git-dir`, and hold makes a marked ls-files wait until the
// flag is gone.
type gitShim struct{ dir string }

func newGitShim(t *testing.T) gitShim {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `#!/bin/sh
D='` + dir + `'
printf '%s|%s\n' "$` + AttemptEnv + `" "$*" >> "$D/log"
case " $* " in
*" ls-files "*)
	[ -e "$D/fail-all" ] && exit 1
	if [ -n "$` + AttemptEnv + `" ]; then while [ -e "$D/hold" ]; do sleep 0.01; done; fi ;;
*" rev-parse --git-dir "*)
	[ -e "$D/fail-probe" ] && exit 1 ;;
esac
exec '` + realGit + `' "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(AttemptEnv, "")
	return gitShim{dir}
}

func (g gitShim) set(t *testing.T, flag string, on bool) {
	t.Helper()
	p := filepath.Join(g.dir, flag)
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

// shimCall is one logged git call.
type shimCall struct{ marker, args string }

// calls returns the calls logged since the last reset.
func (g gitShim) calls(t *testing.T) []shimCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g.dir, "log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []shimCall
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if m, a, ok := strings.Cut(l, "|"); ok {
			out = append(out, shimCall{m, a})
		}
	}
	return out
}

func (g gitShim) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(g.dir, "log")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// markers returns the distinct markers of calls, "" for unmarked ones.
func markers(calls []shimCall) []string {
	var out []string
	for _, c := range calls {
		if !slices.Contains(out, c.marker) {
			out = append(out, c.marker)
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

func TestGitSourceFailedListing(t *testing.T) {
	g := newGitShim(t)
	g.set(t, "fail-all", true)
	repo := committedRepo(t, map[string]string{"a.go": "package a"})
	if _, err := NewSource(repo).List(); err == nil {
		t.Error("inside git: a failed listing must fail the list")
	}
	vendored := committedRepo(t, map[string]string{"vendor/a.go": "package a"})
	g.set(t, "fail-probe", true)
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
	g := newGitShim(t)
	repo := committedRepo(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	e := newTestEngine(t)

	if code, err := HasCode(ctx, repo, false); err != nil || !code {
		t.Fatalf("HasCode = %v, %v", code, err)
	}
	if m := markers(g.calls(t)); !slices.Equal(m, []string{""}) {
		t.Errorf("discovery: markers = %q, want only unmarked calls", m)
	}

	token := WorktreeToken(repo, "")
	g.reset(t)
	if err := e.ReconcileIfStale(ctx, repo, NewSource(repo), token); err != nil {
		t.Fatal(err)
	}
	m1 := markers(g.calls(t))
	if len(m1) != 1 || m1[0] == "" {
		t.Fatalf("ReconcileIfStale: markers = %q, want one attempt's marker", m1)
	}

	writeFile(t, repo, "a.go", "package a\nfunc A() {}\nfunc B() {}\n")
	g.reset(t)
	if err := e.ReconcileIfIdle(ctx, repo, NewSource(repo), func(a string) string { return WorktreeToken(repo, a) }); err != nil {
		t.Fatal(err)
	}
	calls := g.calls(t)
	m2 := markers(calls)
	if len(m2) != 1 || m2[0] == "" || m2[0] == m1[0] {
		t.Fatalf("ReconcileIfIdle: markers = %q, want one marker of its own (not %q)", m2, m1[0])
	}
	for _, want := range []string{"rev-parse HEAD", "status --porcelain", "ls-files"} {
		if !slices.ContainsFunc(calls, func(c shimCall) bool { return strings.Contains(c.args, want) }) {
			t.Errorf("ReconcileIfIdle made no marked %q call: %v", want, calls)
		}
	}
}

func TestDiscoveryIsNotHeldByAnAttempt(t *testing.T) {
	ctx := context.Background()
	g := newGitShim(t)
	repo := committedRepo(t, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	e := newTestEngine(t)
	g.set(t, "hold", true)
	done := make(chan error, 1)
	go func() { done <- e.ReconcileIfStale(ctx, repo, NewSource(repo), "tok") }()
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join(g.dir, "hold")); err == nil {
			g.set(t, "hold", false)
			<-done
		}
	})

	held := func(c shimCall) bool { return c.marker != "" && strings.Contains(c.args, "ls-files") }
	deadline := time.Now().Add(time.Minute)
	for !slices.ContainsFunc(g.calls(t), held) {
		if time.Now().After(deadline) {
			t.Fatal("the attempt never reached its listing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := len(g.calls(t))

	if code, err := HasCode(ctx, repo, false); err != nil || !code {
		t.Fatalf("HasCode during the held attempt = %v, %v", code, err)
	}
	if _, err := e.SearchContent(ctx, repo, "package", ContentSearchOpts{}); err != nil {
		t.Fatalf("SearchContent during the held attempt: %v", err)
	}
	for _, c := range g.calls(t)[before:] {
		if c.marker != "" {
			t.Errorf("discovery call carries the attempt's marker: %+v", c)
		}
	}

	g.set(t, "hold", false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
