package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/gitwrap"
	"github.com/gerwim/recarto/internal/graph"
)

// warmRunning reports whether a process runs bin with the argument warm.
func warmRunning(t *testing.T, bin string) bool {
	t.Helper()
	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cmdlines {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // the process ended since the glob
		}
		args := strings.Split(string(b), "\x00")
		if len(args) > 1 && args[0] == bin && args[1] == "warm" {
			return true
		}
	}
	return false
}

// awaitNoWarm polls until no worker of bin runs.
func awaitNoWarm(t *testing.T, bin string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for warmRunning(t, bin) {
		if time.Now().After(deadline) {
			t.Fatal("the background index did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// buildRecarto builds the recarto binary into a temp dir.
func buildRecarto(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the recarto binary (CGO); skipped under -short")
	}
	bin := filepath.Join(t.TempDir(), "recarto")
	build := exec.Command("go", "build", "-o", bin, "github.com/gerwim/recarto/cmd/recarto")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recarto: %v\n%s", err, out)
	}
	return bin
}

// recartoIn returns a runner of bin in dir with a fresh cache and config,
// no served repos and no sync, and path as PATH.
func recartoIn(t *testing.T, bin, dir, path string) func(stdin string, args ...string) string {
	t.Helper()
	tmp := t.TempDir()
	// Runs before the temp dirs go, so a worker never writes into a removed dir.
	t.Cleanup(func() { awaitNoWarm(t, bin) })
	env := append(os.Environ(),
		"RECARTO_CACHE_DIR="+filepath.Join(tmp, "cache"),
		"RECARTO_CONFIG="+filepath.Join(tmp, "config.toml"),
		"RECARTO_WORKSPACE=", "RECARTO_REPOS=", "RECARTO_SYNC_URL=", "RECARTO_SYNC_KEY=",
		"PATH="+path)
	return func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("recarto %v: %v\n%s", args, err, stderr.String())
		}
		return string(out)
	}
}

func TestSessionStartIndexesInTheBackground(t *testing.T) {
	bin := buildRecarto(t)
	repo := t.TempDir()
	initGitRepo(t, repo, map[string]string{
		"api/a.go": "package api\n\nimport \"example.com/m/lib\"\n\nfunc A() { lib.B() }\n",
		"lib/b.go": "package lib\n\nfunc B() {}\n",
	})
	run := recartoIn(t, bin, repo, os.Getenv("PATH"))

	const (
		codeMap      = "Code map (modules)"
		indexingLine = "The code graph is still being indexed; graph results may be incomplete."
	)
	// What the first block shows depends on how fast its index runs; the
	// agents tests cover each outcome.
	run(`{"session_id":"s1","source":"startup"}`, "hook", "session-start")
	awaitNoWarm(t, bin)

	got := run(`{"session_id":"s2","source":"startup"}`, "hook", "session-start")
	if !strings.Contains(got, codeMap) || strings.Contains(got, indexingLine) {
		t.Fatalf("after the background index: want the map and no indexing line:\n%s", got)
	}
	awaitNoWarm(t, bin)

	status := run("", "status")
	m := regexp.MustCompile(`graph_index_runs\s+(\d+)`).FindStringSubmatch(status)
	if m == nil || m[1] != "1" {
		t.Errorf("graph_index_runs = %v, want 1:\n%s", m, status)
	}
}

// TestSessionStartMakesOneIndexAttempt: a session start makes one index
// attempt, and the next one tries again. Which line a failed or slow attempt
// shows is covered by TestSessionStartFirstIndexNotRunning and
// TestSessionStartIndexStillRunning in internal/agents.
func TestSessionStartMakesOneIndexAttempt(t *testing.T) {
	bin := buildRecarto(t)
	repo := t.TempDir()
	initGitRepo(t, repo, map[string]string{"api/a.go": "package api\n\nfunc A() {}\n"})
	rec := gitwrap.NewRecipe(t, repo, graph.AttemptEnv)
	run := recartoIn(t, bin, repo, rec.PATH())
	const (
		codeMap      = "Code map (modules)"
		indexingLine = "The code graph is still being indexed; graph results may be incomplete."
		unbuiltLine  = "The code graph is not built yet; graph results may be empty."
	)

	rec.Fail(t, true)
	got := run(`{"session_id":"s1","source":"startup"}`, "hook", "session-start")
	if strings.Contains(got, codeMap) {
		t.Fatalf("failed first index: a code map:\n%s", got)
	}
	awaitNoWarm(t, bin)
	if n := len(rec.Attempts(t)); n != 1 {
		t.Fatalf("index attempts = %d, want 1", n)
	}

	rec.Fail(t, false)
	// A slow attempt may not hold the lock yet when the wait ends, so the
	// block's line is not checked here.
	run(`{"session_id":"s2","source":"startup"}`, "hook", "session-start")
	awaitNoWarm(t, bin)
	if n := len(rec.Attempts(t)); n != 2 {
		t.Fatalf("index attempts = %d, want 2", n)
	}
	got = run(`{"session_id":"s3","source":"startup"}`, "hook", "session-start")
	if !strings.Contains(got, codeMap) || strings.Contains(got, indexingLine) || strings.Contains(got, unbuiltLine) {
		t.Fatalf("after the index: want the map alone:\n%s", got)
	}
}
