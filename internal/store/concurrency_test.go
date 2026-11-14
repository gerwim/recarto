package store_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/store"
)

// buildBinary compiles the recarto binary into a temp dir and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "recarto")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gerwim/recarto/cmd/recarto")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestConcurrentWritersAcrossProcessesAndGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-process concurrency test in -short mode")
	}
	dbPath := filepath.Join(t.TempDir(), "store.db")

	// Initialize schema once so all writers share a migrated DB.
	st0, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st0.Close(); err != nil {
		t.Fatal(err)
	}

	bin := buildBinary(t)

	const (
		procs      = 4
		goroutines = 4
		iter       = 200
	)
	var wg sync.WaitGroup

	// External processes (simulate hooks from different worktrees).
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(bin, "debug-stress", "--db", dbPath, "--iter", strconv.Itoa(iter))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("subprocess failed: %v\n%s", err, out)
			}
		}()
	}

	// In-process goroutines on a second handle to the same DB.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := ledger.New(st)
	ctx := context.Background()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iter; j++ {
				if err := l.Incr(ctx, "lifetime", "stress", 1); err != nil {
					t.Errorf("incr: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	got, err := l.Get(ctx, "lifetime", "stress")
	if err != nil {
		t.Fatal(err)
	}
	want := int64((procs + goroutines) * iter)
	if got != want {
		t.Fatalf("stress counter = %d, want %d (lost updates under concurrency)", got, want)
	}

	var integrity string
	if err := st.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
}
