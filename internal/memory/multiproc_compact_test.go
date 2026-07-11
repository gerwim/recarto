package memory_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMultiProcessCompactSharedStore proves that N separate `recarto compact
// --force` PROCESSES against ONE shared store stay integrity-clean and that the
// single-flight lease keeps the epoch consistent (epoch == number of passes that
// actually ran == the "compactions" counter).
func TestMultiProcessCompactSharedStore(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the recarto binary (CGO); skipped under -short")
	}
	bin := filepath.Join(t.TempDir(), "recarto")
	build := exec.Command("go", "build", "-o", bin, "git.gerwim.nl/gerwim/recarto/cmd/recarto")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recarto: %v\n%s", err, out)
	}

	repo := t.TempDir()
	runCompactCmd(t, repo, "git", "init")
	runCompactCmd(t, repo, "git", "config", "user.email", "t@t")
	runCompactCmd(t, repo, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCompactCmd(t, repo, "git", "add", ".")
	runCompactCmd(t, repo, "git", "commit", "-m", "init")

	cache := t.TempDir()
	// The empty trailing overrides neutralize ambient RECARTO_* (workspace/
	// sync/served-repo config on the host running the tests — e.g. a
	// ForgeBoard session) so subprocesses open ONLY this repo's store under
	// the test cache dir.
	env := append(os.Environ(), "RECARTO_CACHE_DIR="+cache,
		"RECARTO_WORKSPACE=", "RECARTO_REPOS=", "RECARTO_SYNC_URL=", "RECARTO_SYNC_KEY=")

	// Seed a couple of memories (one process, sequential).
	seed := exec.Command(bin, "remember", "learning", "--body", "first seeded learning fact")
	seed.Dir, seed.Env = repo, env
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 5; attempt++ {
				cmd := exec.Command(bin, "compact", "--force")
				cmd.Dir, cmd.Env = repo, env
				out, err := cmd.CombinedOutput()
				if err == nil {
					return
				}
				if strings.Contains(string(out), "SQLITE_BUSY") || strings.Contains(string(out), "database is locked") {
					time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
					continue
				}
				t.Errorf("compact: %v\n%s", err, out)
				return
			}
		}()
	}
	wg.Wait()

	matches, _ := filepath.Glob(filepath.Join(cache, "*", "store.db"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 shared store.db, found %v", matches)
	}
	db, err := sql.Open("sqlite", matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}

	var epoch, compactions int64
	db.QueryRow(`SELECT value FROM meta WHERE key='compact_activity_epoch'`).Scan(&epoch)
	db.QueryRow(`SELECT value FROM counters WHERE scope='lifetime' AND metric='compactions'`).Scan(&compactions)
	// Each pass that ran advanced the epoch once and bumped the counter once.
	// Epoch starts at 1; after `compactions` passes it is 1 + compactions.
	if epoch != 1+compactions {
		t.Errorf("epoch=%d, compactions=%d; want epoch == 1+compactions", epoch, compactions)
	}
	if compactions < 1 {
		t.Error("at least one compaction pass should have run")
	}
}

func runCompactCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
	}
}
