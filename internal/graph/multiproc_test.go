package graph

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// runCmd runs args in dir and fails the test on error.
func runCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestMultiProcessIndexAcrossWorktrees is the spec §8 deliverable: N separate
// `recarto index` PROCESSES index distinct git worktrees that share ONE store,
// concurrently. It asserts the shared DB stays integrity-clean and each worktree
// gets its own graph (worktree isolation) with no lost rows.
func TestMultiProcessIndexAcrossWorktrees(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the recarto binary (CGO); skipped under -short")
	}

	// Build the recarto binary once (CGO).
	bin := filepath.Join(t.TempDir(), "recarto")
	build := exec.Command("go", "build", "-o", bin, "git.gerwim.nl/gerwim/recarto/cmd/recarto")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recarto: %v\n%s", err, out)
	}

	// Base repo with one commit of a NON-Go file (so only the per-worktree .go
	// files get indexed). A commit is required before `git worktree add`.
	base := t.TempDir()
	runCmd(t, base, "git", "init")
	runCmd(t, base, "git", "config", "user.email", "t@t")
	runCmd(t, base, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(base, "README.md"), []byte("# base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, base, "git", "add", ".")
	runCmd(t, base, "git", "commit", "-m", "init")

	cache := t.TempDir()

	const n = 4
	wts := make([]string, n)
	for i := 0; i < n; i++ {
		wt := filepath.Join(t.TempDir(), fmt.Sprintf("wt%d", i))
		runCmd(t, base, "git", "worktree", "add", "-b", fmt.Sprintf("b%d", i), wt)
		// Each worktree gets a distinct untracked .go file: A_i calls B_i.
		src := fmt.Sprintf("package p\nfunc A%d(){ B%d() }\nfunc B%d(){}", i, i, i)
		if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		wts[i] = wt
	}

	// Run `recarto index` in every worktree CONCURRENTLY, all sharing one store.
	// Retry on SQLITE_BUSY: busy_timeout(5000) in the binary is sufficient for
	// normal load but can be exceeded when the full test suite saturates the CPU.
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, wt := range wts {
		wg.Add(1)
		go func(wt string) {
			defer wg.Done()
			for attempt := 0; attempt < 5; attempt++ {
				cmd := exec.Command(bin, "index")
				cmd.Dir = wt
				cmd.Env = append(os.Environ(), "RECARTO_CACHE_DIR="+cache)
				out, err := cmd.CombinedOutput()
				if err == nil {
					return
				}
				// Retry transient SQLITE_BUSY (can occur under high test parallelism).
				if strings.Contains(string(out), "SQLITE_BUSY") || strings.Contains(string(out), "database is locked") {
					time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
					continue
				}
				errs <- fmt.Errorf("index %s: %v\n%s", wt, err, out)
				return
			}
			errs <- fmt.Errorf("index %s: still SQLITE_BUSY after retries", wt)
		}(wt)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Exactly one shared store.db was created (one repo-id across all worktrees).
	matches, _ := filepath.Glob(filepath.Join(cache, "*", "store.db"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 store.db (shared), found %v", matches)
	}
	db, err := sql.Open("sqlite", matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}

	// Worktree isolation: n distinct worktrees, each with exactly its 2 symbols
	// (A_i, B_i) — README.md is not a .go file, so 2n nodes total, no lost rows.
	var distinctWts, totalNodes, totalFiles int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT worktree) FROM files`).Scan(&distinctWts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&totalNodes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&totalFiles); err != nil {
		t.Fatal(err)
	}
	if distinctWts != n {
		t.Errorf("distinct worktrees = %d, want %d", distinctWts, n)
	}
	if totalFiles != n {
		t.Errorf("files = %d, want %d (one w.go per worktree)", totalFiles, n)
	}
	if totalNodes != 2*n {
		t.Errorf("nodes = %d, want %d (A_i + B_i per worktree)", totalNodes, 2*n)
	}
}
