package remotesync

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/ledger"
	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/snapshot"
	"github.com/gerwim/recarto/internal/store"
	"github.com/gerwim/recarto/internal/syncserver"
)

// unixHTTPClient starts handler on a unix-domain-socket listener. This
// sandbox's loopback TCP is unreachable (a freshly net.Listen'd 127.0.0.1
// port refuses connections even from the same process), so httptest.Server
// cannot be used here; a unix socket exercises the identical net/http server
// and client machinery unaffected by that restriction.
func unixHTTPClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// machine bundles one "machine's" local store/engine/Syncer against a shared
// in-process sync server.
type machine struct {
	eng *memory.Engine
	st  *store.Store
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng := memory.New(st, ledger.New(st), clock.Real{})
	return &machine{eng: eng, st: st}
}

func (m *machine) syncer(t *testing.T, httpClient *http.Client, repoKey string) *Syncer {
	t.Helper()
	return &Syncer{
		repoKey: repoKey,
		client:  &Client{BaseURL: "http://unix", Key: "s3cret", HTTP: httpClient},
		eng:     m.eng,
		st:      m.st,
	}
}

func newSharedServer(t *testing.T) *http.Client {
	t.Helper()
	srv := syncserver.New("s3cret", filepath.Join(t.TempDir(), "serverdata"))
	t.Cleanup(func() { _ = srv.Close() })
	return unixHTTPClient(t, srv.Handler())
}

func TestTwoMachinesConvergeOnRecordedMemory(t *testing.T) {
	httpClient := newSharedServer(t)
	repoKey := "repo-abc"

	a := newMachine(t)
	b := newMachine(t)
	syncA := a.syncer(t, httpClient, repoKey)
	syncB := b.syncer(t, httpClient, repoKey)
	ctx := context.Background()

	id, _, _, err := a.eng.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "t", Body: "always vendor modules for reproducible builds",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := b.eng.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("machine B did not receive A's recorded memory")
	}
	if got.Body != "always vendor modules for reproducible builds" {
		t.Fatalf("body mismatch after sync: %+v", got)
	}
}

func TestIndependentAuthoringConverges(t *testing.T) {
	httpClient := newSharedServer(t)
	repoKey := "repo-conv"

	a := newMachine(t)
	b := newMachine(t)
	syncA := a.syncer(t, httpClient, repoKey)
	syncB := b.syncer(t, httpClient, repoKey)
	ctx := context.Background()

	if _, _, _, err := a.eng.Record(ctx, memory.NewMemory{
		Kind: memory.KindPreference, Title: "t", Body: "prefer tabs over spaces in this repo",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.eng.Record(ctx, memory.NewMemory{
		Kind: memory.KindPreference, Title: "t", Body: "prefer tabs over spaces in this repo!",
	}); err != nil {
		t.Fatal(err)
	}

	// A pushes, B pulls+pushes (order chosen so B's independent authoring meets
	// A's on the server and reinforces rather than duplicating).
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncA.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	countActive := func(m *machine) int {
		recs, err := m.eng.List(ctx, memory.ListFilter{Kind: memory.KindPreference})
		if err != nil {
			t.Fatal(err)
		}
		return len(recs)
	}
	if n := countActive(a); n != 1 {
		t.Errorf("machine A should have converged to 1 preference memory, got %d", n)
	}
	if n := countActive(b); n != 1 {
		t.Errorf("machine B should have converged to 1 preference memory, got %d", n)
	}
}

func TestForgetPropagatesAndDoesNotResurrect(t *testing.T) {
	httpClient := newSharedServer(t)
	repoKey := "repo-forget"

	a := newMachine(t)
	b := newMachine(t)
	syncA := a.syncer(t, httpClient, repoKey)
	syncB := b.syncer(t, httpClient, repoKey)
	ctx := context.Background()

	id, _, _, err := a.eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "t", Body: "ephemeral shared note"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.eng.GetByID(ctx, id); got == nil {
		t.Fatal("setup: B should have the memory before forgetting it")
	}

	if ok, err := a.eng.Forget(ctx, id, "no longer relevant"); err != nil || !ok {
		t.Fatalf("Forget: ok=%v err=%v", ok, err)
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.eng.GetByID(ctx, id); got != nil {
		t.Fatalf("B should no longer have the forgotten memory: %+v", got)
	}

	// B's own subsequent sync cycle must not resurrect it (no leftover local
	// authoring queues it back up).
	if err := syncB.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.eng.GetByID(ctx, id); got != nil {
		t.Fatalf("a later sync cycle resurrected the forgotten memory: %+v", got)
	}
}

// TestForgetThenStaleSnapshotDoesNotResurrect reproduces the round-7 gap at
// the full two-machine integration layer: the exact session-start sequence
// (pull tombstone → import a stale committed snapshot) must not resurrect a
// memory the fleet already forgot, and must not re-queue it for push.
func TestForgetThenStaleSnapshotDoesNotResurrect(t *testing.T) {
	httpClient := newSharedServer(t)
	repoKey := "repo-forget-snapshot"

	a := newMachine(t)
	b := newMachine(t)
	syncA := a.syncer(t, httpClient, repoKey)
	syncB := b.syncer(t, httpClient, repoKey)
	ctx := context.Background()

	id, _, _, err := a.eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "t", Body: "ephemeral shared note for snapshot test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := b.eng.GetByID(ctx, id)
	if err != nil || before == nil {
		t.Fatalf("setup: B should have the memory before forgetting it: %v %v", before, err)
	}

	// B has a stale committed .recarto/memory.jsonl that still lists this id —
	// e.g. checked into the repo before the memory was forgotten anywhere.
	repoDir := t.TempDir()
	snapPath := filepath.Join(repoDir, ".recarto", "memory.jsonl")
	if err := os.MkdirAll(filepath.Dir(snapPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapPath, []byte(
		`{"id":"`+id+`","kind":"learning","title":"t","body":"ephemeral shared note for snapshot test","confidence":1,"created_at":`+
			strconv.FormatInt(before.CreatedAt, 10)+`}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	// A forgets the memory and pushes the tombstone.
	if ok, err := a.eng.Forget(ctx, id, "no longer relevant"); err != nil || !ok {
		t.Fatalf("Forget: ok=%v err=%v", ok, err)
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}

	// B's session-start sequence: pull (learns the tombstone, deletes the row),
	// then import the stale snapshot (must NOT resurrect it).
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.eng.GetByID(ctx, id); got != nil {
		t.Fatalf("setup: B should have deleted the memory after pulling the tombstone: %+v", got)
	}
	ins, err := snapshot.SyncOnSessionStart(ctx, b.eng, b.st, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if ins != 0 {
		t.Fatalf("importing the stale snapshot must not insert the forgotten memory, got %d", ins)
	}
	if got, _ := b.eng.GetByID(ctx, id); got != nil {
		t.Fatalf("stale snapshot import resurrected a forgotten memory: %+v", got)
	}

	// B's subsequent push must not re-send it (the import must not have queued
	// it in the outbox).
	if err := syncB.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syncA.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.eng.GetByID(ctx, id); got != nil {
		t.Fatalf("A must not see the forgotten memory reappear via B's push: %+v", got)
	}
}

func TestOfflineServerIsNonBlockingAndCatchesUpLater(t *testing.T) {
	ctx := context.Background()
	repoKey := "repo-offline"

	// A syncer pointed at an address nothing is listening on.
	deadClient := &http.Client{Timeout: 200 * time.Millisecond}
	m := newMachine(t)
	syncer := &Syncer{
		repoKey: repoKey,
		client:  &Client{BaseURL: "http://127.0.0.1:1", Key: "s3cret", HTTP: deadClient},
		eng:     m.eng,
		st:      m.st,
	}

	id, _, _, err := m.eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "t", Body: "recorded while offline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.Once(ctx); err == nil {
		t.Fatal("expected an error syncing against an unreachable server")
	}
	// Local operation must be entirely unaffected by the sync failure.
	if got, _ := m.eng.GetByID(ctx, id); got == nil {
		t.Fatal("local record must survive an offline sync attempt")
	}
	pending, err := m.eng.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("the unpushed record must remain queued for the next attempt, got %+v", pending)
	}

	// Now bring the server up and catch up.
	httpClient := newSharedServer(t)
	syncer.client = &Client{BaseURL: "http://unix", Key: "s3cret", HTTP: httpClient}
	if err := syncer.Once(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = m.eng.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("catching up should clear the outbox, got %+v", pending)
	}
}

func TestBootstrapDoesNotCreateGraphTables(t *testing.T) {
	httpClient := newSharedServer(t)
	repoKey := "repo-bootstrap"

	a := newMachine(t)
	b := newMachine(t)
	syncA := a.syncer(t, httpClient, repoKey)
	syncB := b.syncer(t, httpClient, repoKey)
	ctx := context.Background()

	topics := []string{
		"caching layers should invalidate on write not on read",
		"goroutine leaks happen when a context is never cancelled",
		"prefer table-driven tests for parser edge cases",
		"database migrations must be idempotent under retry",
		"http clients need an explicit timeout or they hang forever",
		"json field names should stay stable across releases",
		"retry loops need jittered backoff to avoid thundering herds",
		"logging secrets is a recurring incident source",
		"feature flags should default to the safe behavior",
		"index rebuilds are expensive so gate them behind a budget",
	}
	for _, body := range topics {
		if _, _, _, err := a.eng.Record(ctx, memory.NewMemory{
			Kind: memory.KindLearning, Title: "t", Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := syncA.Push(ctx); err != nil {
		t.Fatal(err)
	}
	// Fresh clone / fresh ephemeral VM bootstrap: cursor 0.
	if err := syncB.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	recs, err := b.eng.List(ctx, memory.ListFilter{Kind: memory.KindLearning})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 10 {
		t.Fatalf("bootstrap should pull all 10 memories, got %d", len(recs))
	}

	var graphRows int
	if err := b.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&graphRows); err != nil {
		t.Fatal(err)
	}
	if graphRows != 0 {
		t.Fatalf("sync must never populate the code graph, found %d files rows", graphRows)
	}
}

func TestRepoKeyRootCommitStableAcrossDifferentLocalRepoIDs(t *testing.T) {
	gcA := gitctx.Context{RepoID: "aaaaaaaaaaaaaaaa", RootCommit: "deadbeefcafef00d"}
	gcB := gitctx.Context{RepoID: "bbbbbbbbbbbbbbbb", RootCommit: "deadbeefcafef00d"}
	keyA, okA := RepoKey(gcA)
	keyB, okB := RepoKey(gcB)
	if !okA || !okB {
		t.Fatalf("RepoKey should resolve for both: okA=%v okB=%v", okA, okB)
	}
	if keyA != keyB {
		t.Fatalf("same RootCommit must yield the same sync key regardless of local RepoID: %q != %q", keyA, keyB)
	}
}

func TestRepoKeyDisabledWithoutRootCommitOrOverride(t *testing.T) {
	_, ok := RepoKey(gitctx.Context{RepoID: "x"})
	if ok {
		t.Fatal("RepoKey must report disabled when there is no RootCommit and no override")
	}
}
