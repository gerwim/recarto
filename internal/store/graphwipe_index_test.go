package store_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

const (
	wtA = "/wtA"
	wtB = "/wtB"
)

type wipeSource struct {
	contents map[string][]byte
	mtimes   map[string]int64
}

func (s *wipeSource) set(path, content string) {
	if s.contents == nil {
		s.contents = make(map[string][]byte)
		s.mtimes = make(map[string]int64)
	}
	s.contents[path] = []byte(content)
	s.mtimes[path]++
}

func (s *wipeSource) List() ([]graph.FileEntry, error) {
	paths := make([]string, 0, len(s.contents))
	for path := range s.contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]graph.FileEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, graph.FileEntry{Path: path, Mtime: s.mtimes[path], Size: int64(len(s.contents[path]))})
	}
	return entries, nil
}

func (s *wipeSource) Read(path string) ([]byte, error) {
	content, ok := s.contents[path]
	if !ok {
		return nil, fmt.Errorf("source file %q not found", path)
	}
	return content, nil
}

func goFile(pkg, gen int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package p%d\n\ntype Widget struct{}\nfunc (w *Widget) Spin() {}\nfunc Handle() { var w Widget; w.Spin(); Helper() }\nfunc Helper() {}\n", pkg)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "func Task%dGen%d() {}\n", i, gen)
	}
	return b.String()
}

type graphTestStore struct {
	st *store.Store
	e  *graph.Engine
}

type agedStore struct {
	graphTestStore
	sources map[string]*wipeSource
	tokens  map[string]string
}

func openGraphTestStore(t *testing.T, path string) graphTestStore {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	e, err := graph.New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return graphTestStore{st: st, e: e}
}

func reconcile(t *testing.T, e *graph.Engine, worktree string, src *wipeSource, token string) {
	t.Helper()
	if err := e.ReconcileIfStale(context.Background(), worktree, src, token); err != nil {
		t.Fatalf("reconcile %s: %v", worktree, err)
	}
}

func newAgedStore(t *testing.T) *agedStore {
	t.Helper()
	gs := openGraphTestStore(t, filepath.Join(t.TempDir(), "store.db"))
	sources := map[string]*wipeSource{wtA: {}, wtB: {}}
	for i := 0; i < 12; i++ {
		for _, wt := range []string{wtA, wtB} {
			sources[wt].set(fmt.Sprintf("p%d/f.go", i), goFile(i, 0))
		}
	}
	sources[wtA].set("solo/solo.go", "package solo\nfunc Zanzibar() {}\nfunc Keep() {}\n")
	tokens := map[string]string{wtA: "a0", wtB: ""}
	for _, wt := range []string{wtA, wtB} {
		reconcile(t, gs.e, wt, sources[wt], tokens[wt])
	}
	for round := 1; round <= 3; round++ {
		for i := 0; i < 12; i += 4 {
			for _, wt := range []string{wtA, wtB} {
				sources[wt].set(fmt.Sprintf("p%d/f.go", i), goFile(i, round))
			}
		}
		tokens[wtA] = fmt.Sprintf("a%d", round)
		for _, wt := range []string{wtA, wtB} {
			reconcile(t, gs.e, wt, sources[wt], tokens[wt])
		}
	}
	if _, err := gs.e.Architecture(context.Background(), wtA, ""); err != nil {
		t.Fatal(err)
	}
	return &agedStore{graphTestStore: gs, sources: sources, tokens: map[string]string{wtA: "a3", wtB: ""}}
}

func indexFresh(t *testing.T, sources map[string]*wipeSource, tokens map[string]string) graphTestStore {
	t.Helper()
	gs := openGraphTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	for _, wt := range []string{wtA, wtB} {
		reconcile(t, gs.e, wt, sources[wt], tokens[wt])
	}
	return gs
}

func ftsData(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow("SELECT coalesce(sum(length(block)), 0) FROM " + table + "_data").Scan(&n); err != nil {
		t.Fatalf("read %s index data: %v", table, err)
	}
	return n
}

func rowsOf(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close query rows: %v", err)
		}
	}()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([]string, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(values))
		for i, value := range values {
			switch v := value.(type) {
			case nil:
				parts[i] = "null"
			case int64:
				parts[i] = fmt.Sprintf("i:%d", v)
			case float64:
				parts[i] = fmt.Sprintf("f:%g", v)
			case string:
				parts[i] = fmt.Sprintf("s:%q", v)
			case []byte:
				parts[i] = "b:" + hex.EncodeToString(v)
			default:
				parts[i] = fmt.Sprintf("%T:%v", value, value)
			}
		}
		line, err := json.Marshal(parts)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, string(line))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func metaWithoutGraphKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT key, value FROM meta")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close meta rows: %v", err)
		}
	}()
	var result []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(key, "graph_token:") || strings.HasPrefix(key, "arch_cache:") || strings.HasPrefix(key, "graph_built:") {
			continue
		}
		result = append(result, fmt.Sprintf("%q=%q", key, value))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func snapshotsOfPreciousState(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"memories":           "SELECT * FROM memories",
		"memory_refs":        "SELECT * FROM memory_refs",
		"memory_minhash":     "SELECT * FROM memory_minhash",
		"memories_fts":       "SELECT rowid, * FROM memories_fts",
		"memories_fts_data":  "SELECT * FROM memories_fts_data",
		"memories_fts_vocab": "SELECT * FROM memories_fts_vocab",
		"memory_tombstones":  "SELECT * FROM memory_tombstones",
		"memory_sync_outbox": "SELECT * FROM memory_sync_outbox",
		"memory_sync_merges": "SELECT * FROM memory_sync_merges",
		"sessions":           "SELECT * FROM sessions",
		"graph_deltas":       "SELECT * FROM graph_deltas",
		"counters":           "SELECT * FROM counters",
	}
	result := make(map[string][]string, len(queries)+1)
	for table, query := range queries {
		result[table] = rowsOf(t, db, query)
	}
	result["meta_without_graph"] = metaWithoutGraphKeys(t, db)
	return result
}

func assertNonempty(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if rows := rowsOf(t, db, "SELECT * FROM "+table); len(rows) == 0 {
			t.Fatalf("test premise failed: %s is empty", table)
		}
	}
}

func graphStateSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var result []string
	for _, query := range []string{
		"SELECT id, worktree, path, last_indexed_at FROM files ORDER BY worktree, path",
		"SELECT rowid, node_id, name, qualified_name FROM nodes_fts ORDER BY rowid",
		"SELECT rowid, node_id, name, qualified_name FROM nodes_fts_stem ORDER BY rowid",
		"SELECT rowid, node_id, content FROM nodes_fts_tri ORDER BY rowid",
		"SELECT key, value FROM meta WHERE key IN ('graph_token:/wtA', 'arch_cache:/wtA', 'graph_built:/wtB') ORDER BY key",
	} {
		result = append(result, rowsOf(t, db, query)...)
	}
	return result
}

func searchKeys(t *testing.T, e *graph.Engine, worktree, query string) []string {
	t.Helper()
	results, err := e.Search(context.Background(), worktree, query, "", "", 50)
	if err != nil {
		t.Fatalf("search %s in %s: %v", query, worktree, err)
	}
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%d", result.Path, result.QualifiedName, result.Kind, result.Line))
	}
	sort.Strings(keys)
	return keys
}

type memoryTestRecord struct {
	kind memory.Kind
	word string
}

func assertOrdinaryOpenDoesNotReindex(t *testing.T, st *store.Store) {
	t.Helper()
	before := graphStateSnapshot(t, st.DB)
	reopened, err := store.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := reopened.Close(); err != nil {
				t.Errorf("close reopened store: %v", err)
			}
		}
	}()
	var version int
	if err := reopened.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 27 {
		t.Fatalf("reopened user_version = %d, want 27", version)
	}
	if after := graphStateSnapshot(t, reopened.DB); !slices.Equal(after, before) {
		t.Fatalf("ordinary open changed indexed graph state:\n got: %v\nwant: %v", after, before)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func seedMemoryRecords(t *testing.T, st *store.Store) (*memory.Engine, []memoryTestRecord, map[string]string) {
	t.Helper()
	t.Setenv("RECARTO_SEMANTIC", "0")
	ctx := context.Background()
	mem := memory.New(st, ledger.New(st), clock.NewFake(time.UnixMilli(1_700_000_000_000)))
	records := []memoryTestRecord{
		{kind: memory.KindLearning, word: "quasarine"},
		{kind: memory.KindBug, word: "nebulite"},
		{kind: memory.KindTodo, word: "pulsarix"},
	}
	ids := make(map[string]string, len(records))
	for _, record := range records {
		id, _, _, err := mem.Record(ctx, memory.NewMemory{Kind: record.kind, Title: record.word + " preserved entry", Body: "Keep this " + record.word + " record."})
		if err != nil {
			t.Fatalf("record %s: %v", record.word, err)
		}
		ids[record.word] = id
	}
	for _, query := range []string{
		`INSERT INTO sessions(id, started_at) VALUES('session-seed', 1)`,
		`INSERT INTO memory_tombstones(id, deleted_at) VALUES('tombstone-seed', 1)`,
		`INSERT INTO memory_sync_merges(id, canonical_id, updated_at) VALUES('merge-seed', 'canonical-seed', 1)`,
		`INSERT INTO meta(key, value) VALUES
		 ('graphXtoken:/keep', 'x-token'), ('archXcache:/keep', 'x-cache'),
		 ('graphXbuilt:/keep', 'x-built'), ('GRAPH_TOKEN:/keep', 'upper-token')`,
	} {
		if _, err := st.DB.ExecContext(ctx, query); err != nil {
			t.Fatalf("seed preserved state: %v", err)
		}
	}
	return mem, records, ids
}

func assertWipePreservedState(t *testing.T, st *store.Store) map[string][]string {
	t.Helper()
	assertNonempty(t, st.DB,
		"files", "nodes", "edges", "edge_candidates", "type_bindings", "node_fts_rows",
		"nodes_fts", "nodes_fts_stem", "nodes_fts_tri",
		"memories", "memories_fts", "memories_fts_vocab", "memory_tombstones",
		"memory_sync_outbox", "memory_sync_merges", "sessions", "graph_deltas")
	for _, key := range []string{"graph_token:/wtA", "arch_cache:/wtA", "graph_built:/wtB"} {
		if got := rowsOf(t, st.DB, "SELECT key FROM meta WHERE key = '"+key+"'"); len(got) != 1 {
			t.Fatalf("test premise missing meta key %q", key)
		}
	}
	before := snapshotsOfPreciousState(t, st.DB)
	for _, table := range []string{"memories", "memories_fts", "memories_fts_vocab", "memory_sync_outbox", "memory_sync_merges", "memory_tombstones", "sessions", "graph_deltas"} {
		if len(before[table]) == 0 {
			t.Fatalf("test premise snapshot for %s is empty", table)
		}
	}
	return before
}

func assertWipeEmptiedCodeGraph(t *testing.T, st, baseline *store.Store, preciousBefore map[string][]string) {
	t.Helper()
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		if got := rowsOf(t, st.DB, "SELECT rowid FROM "+table); len(got) != 0 {
			t.Errorf("%s rows after wipe = %d, want 0", table, len(got))
		}
		if got, fresh := ftsData(t, st.DB, table), ftsData(t, baseline.DB, table); got > fresh {
			t.Errorf("%s index data after wipe = %d bytes, fresh store = %d", table, got, fresh)
		}
	}
	if got, fresh := store.SchemaSnapshot(t, st.DB), store.SchemaSnapshot(t, baseline.DB); !slices.Equal(got, fresh) {
		gotSet, freshSet := make(map[string]bool, len(got)), make(map[string]bool, len(fresh))
		for _, row := range got {
			gotSet[row] = true
		}
		for _, row := range fresh {
			freshSet[row] = true
		}
		for row := range gotSet {
			if !freshSet[row] {
				t.Errorf("schema object not in fresh store: %s", row)
			}
		}
		for row := range freshSet {
			if !gotSet[row] {
				t.Errorf("fresh schema object missing after wipe: %s", row)
			}
		}
	}
	for _, table := range []string{"files", "nodes", "edges", "edge_candidates", "type_bindings", "node_fts_rows"} {
		if count := len(rowsOf(t, st.DB, "SELECT * FROM "+table)); count != 0 {
			t.Errorf("%s rows after wipe = %d, want 0", table, count)
		}
	}
	if got := rowsOf(t, st.DB, "SELECT key FROM meta WHERE key GLOB 'graph_token:*' OR key GLOB 'arch_cache:*' OR key GLOB 'graph_built:*'"); len(got) != 0 {
		t.Errorf("graph freshness/cache keys remain: %v", got)
	}
	if after := snapshotsOfPreciousState(t, st.DB); !equalSnapshots(after, preciousBefore) {
		t.Errorf("precious state changed during graph wipe")
	}
}

func assertMemoryRecordsRecall(t *testing.T, mem *memory.Engine, records []memoryTestRecord, ids map[string]string) {
	t.Helper()
	for _, record := range records {
		got, err := mem.Recall(context.Background(), memory.RecallQuery{TaskHint: record.word})
		if err != nil {
			t.Fatalf("recall %s: %v", record.word, err)
		}
		found := false
		for _, result := range got.Results {
			if result.Record.ID == ids[record.word] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("recall %s did not return memory %s", record.word, ids[record.word])
		}
	}
}

func TestGraphWipeMigrationEmptiesTheCodeGraph(t *testing.T) {
	aged := newAgedStore(t)
	assertOrdinaryOpenDoesNotReindex(t, aged.st)
	mem, records, ids := seedMemoryRecords(t, aged.st)
	before := assertWipePreservedState(t, aged.st)
	if err := store.MigrateWithGraphWipe(aged.st.DB); err != nil {
		t.Fatal(err)
	}
	baseline := openGraphTestStore(t, filepath.Join(t.TempDir(), "baseline.db"))
	if _, err := baseline.st.DB.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
	assertWipeEmptiedCodeGraph(t, aged.st, baseline.st, before)
	assertMemoryRecordsRecall(t, mem, records, ids)
}

func equalSnapshots(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, values := range a {
		if !slices.Equal(values, b[key]) {
			return false
		}
	}
	return true
}

func TestGraphWipeMigrationReindexMatchesAFreshIndex(t *testing.T) {
	aged := newAgedStore(t)
	if err := store.MigrateWithGraphWipe(aged.st.DB); err != nil {
		t.Fatal(err)
	}
	for _, wt := range []string{wtA, wtB} {
		reconcile(t, aged.e, wt, aged.sources[wt], aged.tokens[wt])
		entries, err := aged.sources[wt].List()
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := aged.st.DB.QueryRow("SELECT count(*) FROM files WHERE worktree = ?", wt).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != len(entries) {
			t.Errorf("%s indexed %d files, want %d", wt, count, len(entries))
		}
	}
	fresh := indexFresh(t, aged.sources, aged.tokens)
	checks := []struct{ worktree, query string }{
		{wtA, "Handle"}, {wtA, "spins"}, {wtA, "idget"}, {wtA, "Zanzibar"},
		{wtB, "Handle"}, {wtB, "spins"}, {wtB, "idget"},
	}
	for _, check := range checks {
		got, want := searchKeys(t, aged.e, check.worktree, check.query), searchKeys(t, fresh.e, check.worktree, check.query)
		if len(got) == 0 || len(got) >= 50 {
			t.Errorf("search %q in %s returned %d rows; want 1..49", check.query, check.worktree, len(got))
		}
		if !slices.Equal(got, want) {
			t.Errorf("search %q in %s differs from fresh index:\n got: %v\nwant: %v", check.query, check.worktree, got, want)
		}
	}
	wipedSize := totalFTSData(t, aged.st.DB)
	freshSize := totalFTSData(t, fresh.st.DB)
	if wipedSize*10 > freshSize*11 {
		t.Errorf("re-indexed FTS data = %d bytes, fresh = %d bytes; exceeds 10%% allowance", wipedSize, freshSize)
	}
	assertFTSMatch(t, aged.st.DB, `"zanzibar"`, 1)
	aged.sources[wtA].set("solo/solo.go", "package solo\nfunc Keep() {}\n")
	reconcile(t, aged.e, wtA, aged.sources[wtA], "a4")
	assertFTSMatch(t, aged.st.DB, `"zanzibar"`, 0)
	if got := searchKeys(t, aged.e, wtA, "Zanzibar"); len(got) != 0 {
		t.Errorf("removed Zanzibar symbol remains in graph search: %v", got)
	}
}

func totalFTSData(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var total int64
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		total += ftsData(t, db, table)
	}
	return total
}

func assertFTSMatch(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var count int
		if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s MATCH ?`, table, table), query).Scan(&count); err != nil {
			t.Fatalf("MATCH in %s: %v", table, err)
		}
		if count != want {
			t.Errorf("%s MATCH %q = %d rows, want %d", table, query, count, want)
		}
	}
}
