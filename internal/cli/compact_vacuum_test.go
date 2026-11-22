package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

const reclaimLines = "fts optimized: 4 index(es)\nvacuumed: freed pages returned to the OS\n"

var compactFTSTables = []string{"memories_fts", "nodes_fts", "nodes_fts_stem", "nodes_fts_tri"}

func fragmentFTS(t *testing.T, a *app.App) {
	t.Helper()
	ctx := context.Background()
	for _, table := range compactFTSTables {
		for start := 0; start < 600; start += 10 {
			start := start
			if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
				for k := start; k < start+10; k++ {
					id := fmt.Sprintf("n%d", k)
					token := fmt.Sprintf("Token%d", k)
					var err error
					switch table {
					case "memories_fts":
						_, err = c.ExecContext(ctx, `INSERT INTO memories_fts(memory_id, search_text) VALUES (?, ?)`, id, "alpha "+token)
					case "nodes_fts", "nodes_fts_stem":
						_, err = c.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(node_id, name, qualified_name) VALUES (?, ?, ?)`, table), id, token, "pkg."+token)
					case "nodes_fts_tri":
						_, err = c.ExecContext(ctx, `INSERT INTO nodes_fts_tri(node_id, content) VALUES (?, ?)`, id, "alpha "+token)
					}
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("insert into %s: %v", table, err)
			}
		}
		for start := 0; start < 600; start += 20 {
			start := start
			if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
				for k := start; k < start+10; k++ {
					id := fmt.Sprintf("n%d", k)
					var err error
					if table == "memories_fts" {
						_, err = c.ExecContext(ctx, `DELETE FROM memories_fts WHERE memory_id = ?`, id)
					} else {
						_, err = c.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE node_id = ?`, table), id)
					}
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("delete from %s: %v", table, err)
			}
		}
	}
}

func ftsData(t *testing.T, a *app.App, table string) (size int64, digest string) {
	t.Helper()
	ctx := context.Background()
	if err := a.Store.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COALESCE(SUM(length(block)), 0) FROM %s_data`, table)).Scan(&size); err != nil {
		t.Fatalf("size %s_data: %v", table, err)
	}
	rows, err := a.Store.DB.QueryContext(ctx, fmt.Sprintf(`SELECT id, block FROM %s_data ORDER BY id`, table))
	if err != nil {
		t.Fatalf("query %s_data: %v", table, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close %s_data rows: %v", table, err)
		}
	}()
	h := sha256.New()
	for rows.Next() {
		var id int64
		var block []byte
		if err := rows.Scan(&id, &block); err != nil {
			t.Fatalf("scan %s_data: %v", table, err)
		}
		var prefix [16]byte
		binary.LittleEndian.PutUint64(prefix[:8], uint64(id))
		binary.LittleEndian.PutUint64(prefix[8:], uint64(len(block)))
		if _, err := h.Write(prefix[:]); err != nil {
			t.Fatalf("hash %s_data row prefix: %v", table, err)
		}
		if _, err := h.Write(block); err != nil {
			t.Fatalf("hash %s_data block: %v", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s_data: %v", table, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close %s_data rows: %v", table, err)
	}
	return size, hex.EncodeToString(h.Sum(nil))
}

func ftsMatches(t *testing.T, a *app.App, table, query string) []string {
	t.Helper()
	ctx := context.Background()
	idColumn := "node_id"
	if table == "memories_fts" {
		idColumn = "memory_id"
	}
	rows, err := a.Store.DB.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s MATCH ?`, idColumn, table, table), query)
	if err != nil {
		t.Fatalf("search %s for %q: %v", table, query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close %s search rows: %v", table, err)
		}
	}()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %s search result: %v", table, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s search results: %v", table, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close %s search rows: %v", table, err)
	}
	sort.Strings(ids)
	return ids
}

func freelist(t *testing.T, a *app.App) int {
	t.Helper()
	var n int
	if err := a.Store.DB.QueryRowContext(context.Background(), `PRAGMA freelist_count`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insertMemory(t *testing.T, a *app.App, title, body string) string {
	t.Helper()
	id, _, _, err := a.Engine.Record(context.Background(), memory.NewMemory{
		Kind: memory.KindLearning, Title: title, Body: body, Branch: "main", Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func supersedeMemories(t *testing.T, a *app.App) {
	t.Helper()
	if err := a.Store.WriteTx(context.Background(), func(c store.Conn) error {
		_, err := c.ExecContext(context.Background(), `UPDATE memories SET status = 'superseded'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func checkFTSIntegrity(t *testing.T, a *app.App, table string) {
	t.Helper()
	if _, err := a.Store.DB.ExecContext(context.Background(),
		fmt.Sprintf(`INSERT INTO %s(%s) VALUES ('integrity-check')`, table, table)); err != nil {
		t.Errorf("%s integrity-check: %v", table, err)
	}
}

func TestCompactVacuumOptimizesFTSIndexes(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	fragmentFTS(t, a)
	before := make(map[string]int64, len(compactFTSTables))
	matches := make(map[string]map[string][]string, len(compactFTSTables))
	for _, table := range compactFTSTables {
		before[table], _ = ftsData(t, a, table)
		matches[table] = map[string][]string{
			"token1*": ftsMatches(t, a, table, "token1*"),
			"token15": ftsMatches(t, a, table, "token15"),
		}
	}
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	referencePath := filepath.Join(t.TempDir(), "vacuum-only.db")
	if _, err := a.Store.DB.ExecContext(ctx, `VACUUM INTO ?`, referencePath); err != nil {
		t.Fatalf("VACUUM INTO reference: %v", err)
	}
	referenceInfo, err := os.Stat(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Compact(ctx, a, &out, []string{"--force", "--vacuum"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), reclaimLines) {
		t.Errorf("compact output does not end with reclaim report:\n%s", out.String())
	}
	for _, table := range compactFTSTables {
		after, _ := ftsData(t, a, table)
		if after >= before[table] {
			t.Errorf("%s_data size %d -> %d, want smaller", table, before[table], after)
		}
		for _, query := range []string{"token1*", "token15"} {
			if got := ftsMatches(t, a, table, query); !slices.Equal(got, matches[table][query]) {
				t.Errorf("%s MATCH %q changed: got %v, want %v", table, query, got, matches[table][query])
			}
		}
		checkFTSIntegrity(t, a, table)
	}
	storeInfo, err := os.Stat(a.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if storeInfo.Size() >= referenceInfo.Size() {
		t.Errorf("optimized store size %d, VACUUM-only reference size %d; want smaller", storeInfo.Size(), referenceInfo.Size())
	}
}

func TestCompactVacuumRunsAfterMemoryPass(t *testing.T) {
	a := newTestApp(t, "main")
	for i := range 10 {
		body := strings.Repeat(fmt.Sprintf("memory %d distinct body content ", i), 140)
		insertMemory(t, a, fmt.Sprintf("memory %d", i), body)
	}
	supersedeMemories(t, a)
	var out bytes.Buffer
	if err := Compact(context.Background(), a, &out, []string{"--force", "--vacuum"}); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	reportAt := strings.Index(output, "superseded:   10 gc'd")
	reclaimAt := strings.Index(output, reclaimLines)
	if reportAt < 0 || reclaimAt < 0 || reportAt >= reclaimAt || !strings.HasSuffix(output, reclaimLines) {
		t.Errorf("expected memory report before final reclaim report:\n%s", output)
	}
	if got := freelist(t, a); got != 0 {
		t.Errorf("freelist_count = %d, want 0", got)
	}
}

func TestCompactVacuumAfterSkippedPass(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var first bytes.Buffer
	if err := Compact(ctx, a, &first, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := Compact(ctx, a, &second, []string{"--vacuum"}); err != nil {
		t.Fatal(err)
	}
	want := "compaction skipped (ran recently; use --force to override)\n" + reclaimLines
	if got := second.String(); got != want {
		t.Errorf("second compact output = %q, want %q", got, want)
	}
}

func TestCompactVacuumAfterFailedMemoryPass(t *testing.T) {
	a := newTestApp(t, "main")
	insertMemory(t, a, "blocked memory", "body to retain until compaction")
	supersedeMemories(t, a)
	if _, err := a.Store.DB.ExecContext(context.Background(),
		`CREATE TRIGGER block_memory_pass BEFORE DELETE ON memories BEGIN SELECT RAISE(ABORT, 'memory pass blocked'); END`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Compact(context.Background(), a, &out, []string{"--force", "--vacuum"})
	if err == nil || !strings.Contains(err.Error(), "memory pass blocked") {
		t.Fatalf("Compact error = %v, want memory pass blocked", err)
	}
	if got := out.String(); got != reclaimLines {
		t.Errorf("compact output = %q, want %q", got, reclaimLines)
	}
}

func TestCompactWithoutVacuumKeepsFTSIndexes(t *testing.T) {
	a := newTestApp(t, "main")
	fragmentFTS(t, a)
	before := make(map[string]string, len(compactFTSTables))
	for _, table := range compactFTSTables {
		_, before[table] = ftsData(t, a, table)
	}
	if got := freelist(t, a); got <= 0 {
		t.Fatalf("fixture freelist_count = %d, want positive", got)
	}
	for _, args := range [][]string{{"--force"}, {"--force", "--vacuum", "--dry-run"}} {
		var out bytes.Buffer
		if err := Compact(context.Background(), a, &out, args); err != nil {
			t.Fatalf("Compact(%v): %v", args, err)
		}
		if strings.Contains(out.String(), "fts optimized:") || strings.Contains(out.String(), "vacuumed:") {
			t.Errorf("Compact(%v) output contains reclaim report:\n%s", args, out.String())
		}
		for _, table := range compactFTSTables {
			_, digest := ftsData(t, a, table)
			if digest != before[table] {
				t.Errorf("%s_data changed during Compact(%v)", table, args)
			}
		}
		if got := freelist(t, a); got <= 0 {
			t.Errorf("Compact(%v) left freelist_count %d, want positive", args, got)
		}
	}
}

func TestCompactVacuumOptimizeFailureNamesTable(t *testing.T) {
	a := newTestApp(t, "main")
	fragmentFTS(t, a)
	beforeSize := make(map[string]int64, len(compactFTSTables))
	beforeDigest := make(map[string]string, len(compactFTSTables))
	for _, table := range compactFTSTables {
		beforeSize[table], beforeDigest[table] = ftsData(t, a, table)
	}
	if _, err := a.Store.DB.ExecContext(context.Background(),
		`CREATE TRIGGER block_optimize BEFORE INSERT ON nodes_fts_stem_data BEGIN SELECT RAISE(ABORT, 'optimize blocked'); END`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Compact(context.Background(), a, &out, []string{"--force", "--vacuum"})
	if err == nil || !strings.Contains(err.Error(), "nodes_fts_stem") {
		t.Fatalf("Compact error = %v, want optimize nodes_fts_stem", err)
	}
	for _, table := range []string{"memories_fts", "nodes_fts"} {
		after, _ := ftsData(t, a, table)
		if after >= beforeSize[table] {
			t.Errorf("%s_data size %d -> %d, want earlier optimization retained", table, beforeSize[table], after)
		}
	}
	for _, table := range []string{"nodes_fts_stem", "nodes_fts_tri"} {
		_, digest := ftsData(t, a, table)
		if digest != beforeDigest[table] {
			t.Errorf("%s_data changed after optimize failure", table)
		}
	}
	for _, table := range compactFTSTables {
		checkFTSIntegrity(t, a, table)
	}
	var integrity string
	if err := a.Store.DB.QueryRowContext(context.Background(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Errorf("PRAGMA integrity_check = %q, want ok", integrity)
	}
}
