// Package gitwrap writes git wrappers for tests: scripts placed first on PATH,
// which need /bin/sh.
package gitwrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Script writes a git into a new temp dir and returns the dir, for the front of PATH.
// That git runs hook, a shell snippet that sees the call's arguments as "$@", and then
// the real git with the same arguments.
func Script(t testing.TB, hook string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if strings.Contains(realGit, "'") {
		t.Fatalf("git path %q holds a quote", realGit)
	}
	script := "#!/bin/sh\n" + hook + "\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Recipe is the failing-index recipe: a git that logs every call in one repo,
// and fails or holds the index attempts' listings while its flags say so.
// Calls without an attempt's marker (discovery) always run the real git at once.
type Recipe struct {
	Dir   string // the wrapper's directory, for the front of PATH
	state string // its flag files and log
}

// NewRecipe returns the failing-index recipe for repo. markerEnv names the variable an
// index attempt sets on its git calls (graph.AttemptEnv; passed in because graph's own
// tests use this package).
func NewRecipe(t testing.TB, repo, markerEnv string) *Recipe {
	t.Helper()
	state := t.TempDir()
	r := &Recipe{state: state}
	// Lets a held attempt end before the temp dirs go.
	t.Cleanup(func() { _ = os.Remove(filepath.Join(state, "hold")) })
	roots := []string{repo}
	if real, err := filepath.EvalSymlinks(repo); err == nil && real != repo {
		roots = append(roots, real)
	}
	var scope strings.Builder
	for _, p := range append(roots, state) {
		if strings.Contains(p, "'") {
			t.Fatalf("path %q holds a quote", p)
		}
	}
	for _, root := range roots {
		scope.WriteString(" '" + root + "'|'" + root + "'/*)")
		scope.WriteString(" inscope=1 ;;\n")
	}
	hook := `S='` + state + `'
if [ "$1" = -C ]; then d="$2"; else d="$PWD"; fi
inscope=
case "$d" in
` + scope.String() + `esac
if [ -n "$inscope" ]; then
	m="$` + markerEnv + `"
	printf '%s|%s\n' "$m" "$*" >> "$S/log"
	if [ -n "$m" ]; then
		case " $* " in *" ls-files "*)
			while [ -e "$S/hold" ]; do sleep 0.01; done
			if [ -e "$S/fail" ]; then echo 'fatal: index attempt failed by the test recipe' >&2; exit 1; fi ;;
		esac
	fi
fi`
	r.Dir = Script(t, hook)
	return r
}

// PATH is a PATH with the wrapper first.
func (r *Recipe) PATH() string { return r.Dir + string(os.PathListSeparator) + os.Getenv("PATH") }

// Fail switches the failures of the attempts' listings on or off.
func (r *Recipe) Fail(t testing.TB, on bool) { t.Helper(); r.flag(t, "fail", on) }

// Hold switches the holding of the attempts' listings on or off.
func (r *Recipe) Hold(t testing.TB, on bool) { t.Helper(); r.flag(t, "hold", on) }

func (r *Recipe) flag(t testing.TB, name string, on bool) {
	t.Helper()
	p := filepath.Join(r.state, name)
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

// Call is one logged git call. Marker is "" for discovery.
type Call struct{ Marker, Args string }

// Calls returns every logged call, in order.
func (r *Recipe) Calls(t testing.TB) []Call {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.state, "log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []Call
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if m, a, ok := strings.Cut(l, "|"); ok {
			out = append(out, Call{m, a})
		}
	}
	return out
}

// Attempts returns the distinct markers of the logged calls, in first-seen order.
func (r *Recipe) Attempts(t testing.TB) []string {
	t.Helper()
	var out []string
	for _, c := range r.Calls(t) {
		if c.Marker != "" && !slices.Contains(out, c.Marker) {
			out = append(out, c.Marker)
		}
	}
	return out
}
