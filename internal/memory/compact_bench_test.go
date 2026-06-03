package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// BenchmarkCompact measures a full DRY-RUN pass over a 50k-record store. Dry-run
// is idempotent (no writes), so it is repeatable across b.N and exercises every
// read/compute path (live-token scan, eviction ranking, dedup candidate scan,
// GC counts) — the latency that determines whether the synchronous hook path is
// fast enough or a detached process is warranted (spec D8 / §11). Because this
// file is package memory it may call the unexported e.insert / shingleSet / etc.
func BenchmarkCompact(b *testing.B) {
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	st := mustBenchStore(b)
	defer st.Close()
	e := New(st, ledger.New(st), clk)
	ctx := context.Background()

	kinds := []Kind{KindAction, KindPreference, KindLearning, KindDecision, KindBug}
	// Seed 50k rows in ONE transaction (fast). last_used_epoch stays 0 — the
	// dry-run scan/sort cost we are measuring is independent of its exact value.
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		for i := 0; i < 50_000; i++ {
			id := fmt.Sprintf("bench-%d", i)
			body := fmt.Sprintf("synthetic memory number %d about subsystem %d and its behavior", i, i%97)
			nm := NewMemory{Kind: kinds[i%len(kinds)], Body: body}
			set := shingleSet(body)
			if err := e.insert(ctx, c, id, nm, BuildSearchText(nm.Title, nm.Body, nm.Refs),
				simHash64(set), bandHashes(minHashSignature(set)), 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Compact(ctx, CompactOptions{Force: true, DryRun: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func mustBenchStore(b *testing.B) *store.Store {
	b.Helper()
	st, err := store.Open(filepath.Join(b.TempDir(), "store.db"))
	if err != nil {
		b.Fatal(err)
	}
	return st
}
