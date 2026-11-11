package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/store"
)

// resolveBenchEngine opens the store named by RECARTO_BENCH_STORE (a real,
// already-indexed store such as graphql-platform) and returns an Engine over it.
// Skips when the env var is unset so the normal `go test ./...` never touches it.
func resolveBenchEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	path := os.Getenv("RECARTO_BENCH_STORE")
	if path == "" {
		t.Skip("set RECARTO_BENCH_STORE=/path/to/store.db to run the resolve benchmark")
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e, err := New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(e.Close)
	var wt string
	if err := st.DB.QueryRow(`SELECT worktree FROM files LIMIT 1`).Scan(&wt); err != nil {
		t.Fatalf("worktree: %v", err)
	}
	var nf, nn, ne int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&nf)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nn)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&ne)
	t.Logf("store=%s worktree=%s files=%d nodes=%d edges=%d", path, wt, nf, nn, ne)
	return e, wt
}

// resolvedDigest fingerprints the full resolution result so an optimization can be
// proven byte-identical to the baseline. Node ids are stable within one store, so
// digests computed before and after a code change on the SAME store are comparable.
func resolvedDigest(t *testing.T, e *Engine) (string, int, int, int) {
	t.Helper()
	h := sha256.New()
	rows, err := e.st.DB.Query(`SELECT edge_id, dst_node_id, CAST(ROUND(confidence,4) AS TEXT), method
		FROM edge_candidates ORDER BY edge_id, dst_node_id, method`)
	if err != nil {
		t.Fatal(err)
	}
	cands := 0
	for rows.Next() {
		var eid, dst, conf, method string
		if err := rows.Scan(&eid, &dst, &conf, &method); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(h, "C\x1f%s\x1f%s\x1f%s\x1f%s\x1e", eid, dst, conf, method)
		cands++
	}
	rows.Close()

	callResolved, nonCallResolved := 0, 0
	rows, err = e.st.DB.Query(`SELECT id, kind, COALESCE(dst_node_id,'') FROM edges
		WHERE dst_name <> '' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, kind, dst string
		if err := rows.Scan(&id, &kind, &dst); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(h, "E\x1f%s\x1f%s\x1f%s\x1e", id, kind, dst)
		if dst != "" {
			if kind == "calls" {
				callResolved++
			} else {
				nonCallResolved++
			}
		}
	}
	rows.Close()
	return hex.EncodeToString(h.Sum(nil)), cands, callResolved, nonCallResolved
}

// TestResolveBenchHeadline runs the full Engine.Resolve once and reports wall time
// plus the resolution digest — the headline baseline/after number. The digest must
// stay byte-identical across optimizations (no regressions).
func TestResolveBenchHeadline(t *testing.T) {
	e, wt := resolveBenchEngine(t)
	ctx := context.Background()
	start := time.Now()
	if err := e.Resolve(ctx, wt, noopProgress{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	dur := time.Since(start)
	digest, cands, callRes, nonCallRes := resolvedDigest(t, e)
	t.Logf("RESOLVE total=%s candidates=%d call_resolved=%d noncall_resolved=%d digest=%s",
		dur.Round(time.Millisecond), cands, callRes, nonCallRes, digest)
}

// TestResolveBenchProfile times each sub-step of the CURRENT full Resolve in order,
// inside one WriteTxBulk (the production resolve connection profile), using the real
// package strategy constants and fold helpers so it never drifts from resolve.go. It
// leaves the store resolved and prints the digest.
func TestResolveBenchProfile(t *testing.T) {
	e, wt := resolveBenchEngine(t)
	ctx := context.Background()

	var hasOthers int
	if err := e.st.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM files WHERE worktree <> ?)`, wt).Scan(&hasOthers); err != nil {
		t.Fatal(err)
	}
	single := hasOthers == 0
	t.Logf("single-worktree store = %v", single)

	const nonCall = `UPDATE edges SET dst_node_id = (
  SELECT n.id FROM nodes n
    JOIN files f  ON f.id = n.file_id
    JOIN files ef ON ef.id = edges.file_id
   WHERE f.worktree = ?
     AND (n.name = edges.dst_name OR n.qualified_name = edges.dst_name)
     AND ( ef.language = f.language OR ( ef.language IN ('typescript','tsx','javascript')
                AND f.language IN ('typescript','tsx','javascript') ) )
   ORDER BY CASE WHEN f.id = edges.file_id THEN 0 ELSE 1 END,
     CASE WHEN substr(f.path,1,instr(f.path,'/')) = substr(ef.path,1,instr(ef.path,'/')) THEN 0 ELSE 1 END,
     f.path, n.start_byte, n.id LIMIT 1)
WHERE dst_node_id IS NULL AND dst_name <> '' AND kind <> 'calls'
  AND file_id IN (SELECT id FROM files WHERE worktree = ?)`
	total := time.Now()
	if err := e.st.WriteTxBulk(ctx, func(c store.Conn) error {
		step := func(name string, fn func() (int64, error)) {
			start := time.Now()
			n, err := fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			t.Logf("STEP %-22s %10s rows=%d", name, time.Since(start).Round(time.Millisecond), n)
		}
		exec := func(sql string, args ...any) func() (int64, error) {
			return func() (int64, error) {
				res, err := c.ExecContext(ctx, sql, args...)
				if err != nil {
					return 0, err
				}
				return res.RowsAffected()
			}
		}

		step("drop idx_dst", exec(`DROP INDEX IF EXISTS idx_edge_candidates_dst`))
		if single {
			step("clear candidates", exec(`DELETE FROM edge_candidates`))
			step("clear dst_node_id", exec(`UPDATE edges SET dst_node_id=NULL WHERE kind='calls' AND dst_node_id IS NOT NULL`))
		} else {
			step("clear candidates", exec(`DELETE FROM edge_candidates WHERE edge_id IN (
  SELECT e.id FROM edges e JOIN files f ON f.id=e.file_id WHERE f.worktree=? AND e.kind='calls')`, wt))
			step("clear dst_node_id", exec(`UPDATE edges SET dst_node_id=NULL WHERE kind='calls' AND dst_node_id IS NOT NULL
  AND file_id IN (SELECT id FROM files WHERE worktree=?)`, wt))
		}
		step("non-call edges", exec(nonCall, wt, wt))

		// Call-edge resolution via the exact production helpers: shared maps, the
		// fold-per-strategy passes, S6, then the winner write-back.
		var m *resolveMaps
		step("build maps", func() (int64, error) {
			var err error
			m, err = buildResolveMaps(ctx, c, wt)
			return 0, err
		})
		step("build recv_types", func() (int64, error) { return 0, buildReceiverTypes(ctx, c, wt, m) })
		winners := make(map[string]denormWinner, len(m.edges))
		step("S1 self", func() (int64, error) {
			return 0, foldStrategy(ctx, c, m, winners, stratSelf+returningCandidates, wt, wt)
		})
		step("S2 typed (shared)", func() (int64, error) {
			return 0, foldStrategy(ctx, c, m, winners, stratTypedReceiverShared+returningCandidates, wt)
		})
		step("S3 static", func() (int64, error) {
			return 0, foldStrategy(ctx, c, m, winners, stratStatic+returningCandidates, wt, wt)
		})
		step("S4 iface (shared)", func() (int64, error) {
			return 0, foldStrategy(ctx, c, m, winners, stratInterfaceShared+returningCandidates, wt)
		})
		step("S5 name", func() (int64, error) { return 0, resolveNameFallback(ctx, c, wt, m.files, winners) })
		step("S6 ctor", func() (int64, error) { return 0, stratImplicitCtorFold(ctx, c, wt, m, winners) })
		step("denormalize", func() (int64, error) { return 0, applyWinners(ctx, c, winners) })
		step("rebuild idx_dst", exec(`CREATE INDEX IF NOT EXISTS idx_edge_candidates_dst ON edge_candidates(dst_node_id)`))
		step("analyze", func() (int64, error) {
			if _, err := c.ExecContext(ctx, `PRAGMA analysis_limit=1000`); err != nil {
				return 0, err
			}
			res, err := c.ExecContext(ctx, `ANALYZE`)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		})
		step("drop recv_types", exec(`DROP TABLE IF EXISTS recv_types`))
		return nil
	}); err != nil {
		t.Fatalf("profile tx: %v", err)
	}
	t.Logf("PROFILE total=%s", time.Since(total).Round(time.Millisecond))

	digest, cands, callRes, nonCallRes := resolvedDigest(t, e)
	t.Logf("RESOLVE candidates=%d call_resolved=%d noncall_resolved=%d digest=%s",
		cands, callRes, nonCallRes, digest)
}
