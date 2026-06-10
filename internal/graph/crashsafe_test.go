package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// cancelOnResolvePhase cancels the reconcile's context the moment the resolve
// phase starts — reproducing a client-side MCP timeout that aborts a reconcile
// after every file write has landed but before resolution completes (the
// production failure mode: opencode cancels the tool call mid-reconcile).
type cancelOnResolvePhase struct{ cancel context.CancelFunc }

func (p cancelOnResolvePhase) Start(int) {}
func (p cancelOnResolvePhase) Inc()      {}
func (p cancelOnResolvePhase) Done()     {}
func (p cancelOnResolvePhase) Phase(label string) {
	if label == "resolving references" {
		p.cancel()
	}
}

// phaseRecorder records every Phase label a reconcile announces, so a test can
// assert whether the resolve stage ran at all.
type phaseRecorder struct{ phases []string }

func (p *phaseRecorder) Start(int)          {}
func (p *phaseRecorder) Inc()               {}
func (p *phaseRecorder) Done()              {}
func (p *phaseRecorder) Phase(label string) { p.phases = append(p.phases, label) }

func (p *phaseRecorder) saw(label string) bool { return slices.Contains(p.phases, label) }

// resolvedCallEdges counts call edges in wt whose destination has been resolved
// to a concrete node — the observable output of the Resolve stage.
func resolvedCallEdges(t *testing.T, e *Engine, wt string) int {
	t.Helper()
	var n int
	err := e.st.DB.QueryRow(`
		SELECT COUNT(*) FROM edges ed
		JOIN files f ON f.id = ed.file_id
		WHERE f.worktree = ? AND ed.kind = 'calls' AND ed.dst_node_id IS NOT NULL`, wt).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// crossFileCallSource is a two-file worktree with a cross-file call, the
// smallest graph whose resolution is observable (A() calls B() in another file).
func crossFileCallSource() *mutMemSource {
	return &mutMemSource{files: map[string]string{
		"a.go": "package p\n\nfunc A() {\n\tB()\n}\n",
		"b.go": "package p\n\nfunc B() {}\n",
	}}
}

// TestAbortedResolveIsHealedByNextReconcile is the production bug: a reconcile
// cancelled during Resolve (after all file writes committed) must NOT let the
// next reconcile — whose worklist is empty because every file hash matches —
// conclude there is nothing to do, stamp the token fresh, and leave the store
// with unresolved edges forever.
func TestAbortedResolveIsHealedByNextReconcile(t *testing.T) {
	e := newTestEngine(t)
	src := crossFileCallSource()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1",
		WithProgress(cancelOnResolvePhase{cancel}))
	if err == nil {
		t.Fatal("reconcile aborted during resolve: want error, got nil")
	}
	if got := resolvedCallEdges(t, e, "/wt"); got != 0 {
		t.Fatalf("setup: resolve was expected to be aborted, but %d edges resolved", got)
	}

	// Recovery: same token, no file changed. The interrupted resolve must be
	// detected and finished, not skipped.
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if got := resolvedCallEdges(t, e, "/wt"); got == 0 {
		t.Fatal("call edge A->B still unresolved after recovery reconcile: aborted resolve was stamped fresh")
	}
}

// TestPendingResolveClearedAfterRecovery proves the pending state does not leak:
// once a recovery reconcile has resolved, a further reconcile with no changes
// must be a true no-op (no resolve phase) — otherwise every future reconcile
// would pay a full resolve.
func TestPendingResolveClearedAfterRecovery(t *testing.T) {
	e := newTestEngine(t)
	src := crossFileCallSource()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1",
		WithProgress(cancelOnResolvePhase{cancel})); err == nil {
		t.Fatal("setup: want aborted reconcile")
	}
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}

	rec := &phaseRecorder{}
	if err := e.Reconcile(context.Background(), "/wt", src, WithProgress(rec)); err != nil {
		t.Fatal(err)
	}
	if rec.saw("resolving references") {
		t.Fatal("no-change reconcile after recovery still resolves: pending marker not cleared")
	}
}

// TestReconcileIfStaleHealsMissingIndexes covers the wedged production store: the
// freshness token is stamped, but the graph secondary indexes are gone (dropped
// by an aborted bulk load under an older binary). A token-fresh short-circuit
// must not keep a broken store broken.
func TestReconcileIfStaleHealsMissingIndexes(t *testing.T) {
	e := newTestEngine(t)
	src := crossFileCallSource()
	ctx := context.Background()
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.st.DB.Exec(`DROP INDEX idx_nodes_name`); err != nil {
		t.Fatal(err)
	}
	// Wipe the resolution too, as an aborted resolve under the old binary did.
	if _, err := e.st.DB.Exec(`UPDATE edges SET dst_node_id = NULL WHERE kind='calls'`); err != nil {
		t.Fatal(err)
	}

	// Same token: previously a pure no-op. It must now detect the missing index,
	// rebuild it, and re-resolve.
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.st.DB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_nodes_name'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("idx_nodes_name still missing after token-fresh reconcile: store cannot self-heal")
	}
	if got := resolvedCallEdges(t, e, "/wt"); got == 0 {
		t.Fatal("edges still unresolved after index self-heal")
	}
}

// countingSource counts List calls so a test can tell whether ReconcileIfStale
// short-circuited or actually reconciled.
type countingSource struct {
	inner Source
	lists int
}

func (c *countingSource) List() ([]FileEntry, error) {
	c.lists++
	return c.inner.List()
}
func (c *countingSource) Read(p string) ([]byte, error) { return c.inner.Read(p) }

// TestReconcileIfStaleForce: WithForce bypasses the token short-circuit and
// re-runs a full resolve — the explicit heal lever for a wedged store.
func TestReconcileIfStaleForce(t *testing.T) {
	e := newTestEngine(t)
	src := &countingSource{inner: crossFileCallSource()}
	ctx := context.Background()
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if src.lists != 1 {
		t.Fatalf("List calls = %d, want 1 (second call should short-circuit)", src.lists)
	}

	// Simulate silent damage the token cannot see.
	if _, err := e.st.DB.Exec(`UPDATE edges SET dst_node_id = NULL WHERE kind='calls'`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileIfStale(ctx, "/wt", src, "tok-1", WithForce()); err != nil {
		t.Fatal(err)
	}
	if src.lists != 2 {
		t.Fatalf("List calls = %d, want 2 (force must reconcile)", src.lists)
	}
	if got := resolvedCallEdges(t, e, "/wt"); got == 0 {
		t.Fatal("force reconcile did not re-resolve edges")
	}
}

// TestReconcileTruncatesWALAfterMutation: a mutating reconcile must leave the
// WAL truncated. The resolve phase writes the most bytes of the whole run
// (edge updates, candidates, index rebuilds) and lands AFTER the last in-loop
// checkpoint, so without a final checkpoint every completed index leaves all
// of that sitting in the WAL (821MB observed in production).
func TestReconcileTruncatesWALAfterMutation(t *testing.T) {
	e := newTestEngine(t)
	src := crossFileCallSource()
	if err := e.ReconcileIfStale(context.Background(), "/wt", src, "tok-1"); err != nil {
		t.Fatal(err)
	}
	wal := e.st.Path() + "-wal"
	if info, err := os.Stat(wal); err == nil && info.Size() != 0 {
		t.Fatalf("WAL is %d bytes after reconcile, want 0 (truncated)", info.Size())
	}
}

// TestMtimeOnlySweepDoesNotResolve is the performance guard for the pending
// marker: a sweep that touches every mtime but changes no content must remain a
// cheap no-op — it must NOT set the pending marker in a way that triggers a full
// resolve (e.g. after a `touch -r`/checkout that preserves content).
func TestMtimeOnlySweepDoesNotResolve(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\n\nfunc A() {\n\tB()\n}\n")
	writeFile(t, wt, "b.go", "package p\n\nfunc B() {}\n")
	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src); err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(time.Hour)
	for _, f := range []string{"a.go", "b.go"} {
		if err := os.Chtimes(filepath.Join(wt, f), future, future); err != nil {
			t.Fatal(err)
		}
	}
	rec := &phaseRecorder{}
	if err := e.Reconcile(ctx, wt, src, WithProgress(rec)); err != nil {
		t.Fatal(err)
	}
	if rec.saw("resolving references") {
		t.Fatal("mtime-only sweep triggered a full resolve: pending marker too eager")
	}
}
