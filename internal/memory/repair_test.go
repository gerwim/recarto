package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// TestEngineWorksOnARepairedStore is the engine-level contract of store.Repair:
// the two structures the memory engine derives its behaviour from — the FTS
// index (recall) and the MinHash bands (near-duplicate reinforcement) — must
// still work after the store has been rebuilt, since only one of them is
// actually rebuilt (FTS) and the other is salvaged (MinHash).
func TestEngineWorksOnARepairedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	ctx := context.Background()

	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(st, ledger.New(st), clk)

	original := NewMemory{
		Kind:   KindLearning,
		Title:  "WAL needs busy_timeout",
		Body:   "set busy_timeout so concurrent writers wait instead of erroring SQLITE_BUSY",
		Refs:   []Ref{{Type: RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
		Source: "cli",
	}
	origID, _, _, err := eng.Record(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := eng.Record(ctx, NewMemory{
		Kind:   KindDecision,
		Title:  "Cache lives outside the worktree",
		Body:   "the sqlite cache is stored under the user cache dir, shared by all worktrees",
		Branch: "main",
		Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Repair has no corruption precondition — it is also the "rebuild my store,
	// keep my memories" tool, which is exactly what this exercises.
	stats, err := store.Repair(path)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if stats.Tables["memories"].Salvaged != 2 {
		t.Fatalf("salvaged %d memories, want 2", stats.Tables["memories"].Salvaged)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng = New(st, ledger.New(st), clk)

	// Recall depends on the rebuilt FTS index.
	got, err := eng.Recall(ctx, RecallQuery{TaskHint: "busy_timeout writers", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got.Results {
		if r.Record != nil && r.Record.ID == origID {
			found = true
		}
	}
	if !found {
		t.Fatalf("recall did not find the salvaged memory (%d results)", len(got.Results))
	}
	var vectors int
	if err := st.DB.QueryRow(`SELECT count(*) FROM memory_vectors`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 2 {
		t.Fatalf("re-embedded memory_vectors = %d, want 2", vectors)
	}

	// Reinforcement depends on the salvaged MinHash bands: re-recording the same
	// learning must reinforce the existing row, not create a second one.
	id2, reinforced, _, err := eng.Record(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	if !reinforced || id2 != origID {
		t.Fatalf("re-recording gave id=%s reinforced=%v, want the salvaged id %s reinforced",
			id2, reinforced, origID)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("memories = %d after reinforcement, want 2", n)
	}
}
