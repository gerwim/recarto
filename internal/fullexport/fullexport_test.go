package fullexport

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/repokey"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// seedStore opens (creating) a store at <cacheDir>/<dirName>/store.db and
// writes repo_name/root_commit meta rows exactly like app.writeMeta does, so
// ExportAll's meta-reading logic exercises the real shape those rows have in
// production. The caller must Close the returned store.
func seedStore(t *testing.T, cacheDir, dirName, repoName, rootCommit string) (*store.Store, *memory.Engine) {
	t.Helper()
	st, err := store.Open(filepath.Join(cacheDir, dirName, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = st.WriteTx(ctx, func(c store.Conn) error {
		for _, kv := range [][2]string{{"repo_name", repoName}, {"root_commit", rootCommit}} {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, memory.New(st, ledger.New(st), clock.Real{})
}

func readAllEntries(t *testing.T, data []byte) []syncwire.ExportEntry {
	t.Helper()
	var entries []syncwire.ExportEntry
	if _, err := Read(bytes.NewReader(data), func(e syncwire.ExportEntry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestExportAllGroupsByServerKeyAndCoversAllStatuses(t *testing.T) {
	cacheDir := t.TempDir()
	ctx := context.Background()

	stA, engA := seedStore(t, cacheDir, "dirA", "repoA", "rootcommitA")
	if _, _, _, err := engA.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "a", Body: "a learning about repo A retries",
	}); err != nil {
		t.Fatal(err)
	}
	doneID, _, _, err := engA.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "d", Body: "a todo in repo A that gets done", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engA.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}
	forgottenID, _, _, err := engA.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "f", Body: "a learning that will be forgotten in repo A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engA.Forget(ctx, forgottenID, "test"); err != nil {
		t.Fatal(err)
	}
	stA.Close()

	stB, engB := seedStore(t, cacheDir, "dirB", "repoB", "rootcommitB")
	if _, _, _, err := engB.Record(ctx, memory.NewMemory{
		Kind: memory.KindDecision, Title: "b", Body: "a decision recorded in repo B",
	}); err != nil {
		t.Fatal(err)
	}
	stB.Close()

	stFB, engFB := seedStore(t, cacheDir, feedback.StoreID, "", "")
	if _, _, _, err := engFB.Record(ctx, memory.NewMemory{
		Kind: memory.KindFeedback, Title: "fb", Body: "a recarto issue report",
	}); err != nil {
		t.Fatal(err)
	}
	stFB.Close()

	// A shallow-clone-like store with no root commit must be skipped, not fail
	// the whole export.
	stShallow, _ := seedStore(t, cacheDir, "dirShallow", "shallowRepo", "")
	stShallow.Close()

	var buf bytes.Buffer
	stats, err := ExportAll(ctx, cacheDir, clock.NewFake(time.UnixMilli(1_700_000_000_000)), &buf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Repos != 3 {
		t.Fatalf("stats.Repos = %d, want 3 (repoA, repoB, feedback)", stats.Repos)
	}
	if stats.Records != 4 {
		t.Fatalf("stats.Records = %d, want 4 (A-active, A-done, B-decision, feedback)", stats.Records)
	}
	if stats.Tombstones != 1 {
		t.Fatalf("stats.Tombstones = %d, want 1 (the forgotten learning)", stats.Tombstones)
	}
	if stats.Skipped != 1 || len(stats.SkippedRepos) != 1 || stats.SkippedRepos[0] != "shallowRepo" {
		t.Fatalf("stats.Skipped = %+v, want exactly shallowRepo skipped", stats)
	}

	keyA := repokey.Hash("rootcommitA")
	keyB := repokey.Hash("rootcommitB")
	if keyA == keyB {
		t.Fatal("distinct root commits must hash to distinct keys")
	}

	entries := readAllEntries(t, buf.Bytes())
	if len(entries) != 5 { // 4 records + 1 tombstone
		t.Fatalf("entries = %d, want 5", len(entries))
	}

	byKey := map[string][]syncwire.ExportEntry{}
	for _, e := range entries {
		byKey[e.RepoKey] = append(byKey[e.RepoKey], e)
	}
	if len(byKey[keyA]) != 3 { // active + done + tombstone
		t.Fatalf("entries under repoA's key = %d, want 3", len(byKey[keyA]))
	}
	if len(byKey[keyB]) != 1 {
		t.Fatalf("entries under repoB's key = %d, want 1", len(byKey[keyB]))
	}
	if len(byKey[feedback.StoreID]) != 1 {
		t.Fatalf("entries under the reserved feedback key = %d, want 1", len(byKey[feedback.StoreID]))
	}

	var sawDone, sawTombstone bool
	for _, e := range byKey[keyA] {
		if e.Record.Status == "done" {
			sawDone = true
		}
		if e.Record.Deleted {
			sawTombstone = true
		}
	}
	if !sawDone {
		t.Fatal("expected the done todo to round-trip with status=done")
	}
	if !sawTombstone {
		t.Fatal("expected the forgotten memory's tombstone to round-trip")
	}

	// The header (first line) must be self-identifying and skip-able, and
	// must carry each project's root_commit (not just its key/name) — that
	// is the metadata that lets a reader/operator confirm which live repo an
	// opaque key corresponds to.
	firstLine := bytes.SplitN(buf.Bytes(), []byte("\n"), 2)[0]
	if !bytes.Contains(firstLine, []byte(syncwire.ExportFormat)) {
		t.Fatalf("first line missing format marker: %s", firstLine)
	}
	var header syncwire.ExportHeader
	if err := json.Unmarshal(firstLine, &header); err != nil {
		t.Fatal(err)
	}
	rootCommitsByKey := map[string]string{}
	for _, r := range header.Repos {
		rootCommitsByKey[r.Key] = r.RootCommit
	}
	if rootCommitsByKey[keyA] != "rootcommitA" {
		t.Fatalf("header root_commit for repoA = %q, want rootcommitA", rootCommitsByKey[keyA])
	}
	if rootCommitsByKey[keyB] != "rootcommitB" {
		t.Fatalf("header root_commit for repoB = %q, want rootcommitB", rootCommitsByKey[keyB])
	}
}

func TestExportAllOnNonexistentCacheDirIsEmptyNotError(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "never-created")
	var buf bytes.Buffer
	stats, err := ExportAll(context.Background(), cacheDir, clock.Real{}, &buf, Options{})
	if err != nil {
		t.Fatalf("a machine with no cache dir yet must export empty, not error: %v", err)
	}
	if stats.Repos != 0 || stats.Records != 0 {
		t.Fatalf("stats = %+v, want all zero", stats)
	}
}

func TestExportAllOnlyRestrictsToOneRepo(t *testing.T) {
	cacheDir := t.TempDir()
	ctx := context.Background()

	stA, engA := seedStore(t, cacheDir, "dirA", "repoA", "rootcommitA")
	if _, _, _, err := engA.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "a", Body: "repo A only content"}); err != nil {
		t.Fatal(err)
	}
	stA.Close()

	stB, engB := seedStore(t, cacheDir, "dirB", "repoB", "rootcommitB")
	if _, _, _, err := engB.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "b", Body: "repo B only content"}); err != nil {
		t.Fatal(err)
	}
	stB.Close()

	var buf bytes.Buffer
	stats, err := ExportAll(ctx, cacheDir, clock.Real{}, &buf, Options{Only: "dirA"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Repos != 1 || stats.Records != 1 {
		t.Fatalf("stats = %+v, want exactly repoA's 1 record", stats)
	}
	entries := readAllEntries(t, buf.Bytes())
	if len(entries) != 1 || entries[0].Record.Body != "repo A only content" {
		t.Fatalf("entries = %+v, want only repo A's record", entries)
	}
}

// TestExportAllOnlyKeyOverridesShallowCloneSkip pins that scoping to a single
// project with an explicit OnlyKey (as `recarto export --repo` does when
// RECARTO_SYNC_REPO is set, resolved live via remotesync.RepoKey) exports a
// store that an unscoped, whole-cache-dir sweep would otherwise have to skip
// for lacking a resolvable root commit.
func TestExportAllOnlyKeyOverridesShallowCloneSkip(t *testing.T) {
	cacheDir := t.TempDir()
	ctx := context.Background()

	st, eng := seedStore(t, cacheDir, "dirShallow", "shallowRepo", "")
	if _, _, _, err := eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "s", Body: "shallow clone content"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var buf bytes.Buffer
	stats, err := ExportAll(ctx, cacheDir, clock.Real{}, &buf, Options{Only: "dirShallow", OnlyKey: "manual-override-key"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 0 || stats.Repos != 1 || stats.Records != 1 {
		t.Fatalf("stats = %+v, want the shallow clone exported via the override, not skipped", stats)
	}
	entries := readAllEntries(t, buf.Bytes())
	if len(entries) != 1 || entries[0].RepoKey != "manual-override-key" {
		t.Fatalf("entries = %+v, want 1 entry tagged with the override key", entries)
	}
}

// TestExportShipsNoGraph mirrors internal/remotesync's
// TestBootstrapDoesNotCreateGraphTables: a store with graph rows must export
// only the portable memory/tombstone tables, never the code graph.
func TestExportShipsNoGraph(t *testing.T) {
	cacheDir := t.TempDir()
	ctx := context.Background()

	st, eng := seedStore(t, cacheDir, "dirA", "repoA", "rootcommitA")
	if _, _, _, err := eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "a", Body: "a portable learning"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO files(id, worktree, path, language, content_hash, mtime, size, last_indexed_at)
		 VALUES ('f1', '/repo', 'main.go', 'go', 'hash1', 0, 100, 0)`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var buf bytes.Buffer
	stats, err := ExportAll(ctx, cacheDir, clock.Real{}, &buf, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 {
		t.Fatalf("stats.Records = %d, want 1 (only the memory, never the graph row)", stats.Records)
	}
	if bytes.Contains(buf.Bytes(), []byte("main.go")) {
		t.Fatalf("export must never contain graph data:\n%s", buf.String())
	}
}
