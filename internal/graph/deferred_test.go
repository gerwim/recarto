package graph

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/ids"
	"github.com/gerwim/recarto/internal/store"
)

// heldReadSource returns each file's bytes as they are when Read is called, then
// holds the call until the test sends on release: an index that parsed the file
// just before an edit.
type heldReadSource struct {
	inner   Source
	read    chan string   // receives the path of each Read
	release chan struct{} // one send per Read
}

func newHeldReadSource(root string) heldReadSource {
	return heldReadSource{inner: FSSource{Root: root}, read: make(chan string), release: make(chan struct{})}
}

func (h heldReadSource) List() ([]FileEntry, error) { return h.inner.List() }
func (h heldReadSource) Read(p string) ([]byte, error) {
	b, err := h.inner.Read(p)
	h.read <- p
	<-h.release
	return b, err
}

// awaitRead waits for the source's next Read.
func (h heldReadSource) awaitRead(t *testing.T) {
	t.Helper()
	select {
	case <-h.read:
	case <-time.After(hangTimeout):
		t.Fatal("no read came")
	}
}

// mtimes hands out distinct file times, so the index's mtime and size
// pre-check sees every rewrite however fast a test makes them.
var mtimes atomic.Int64

// writeSrc writes src to rel in wt and returns it with its stat, as the
// post-edit hook passes them.
func writeSrc(t *testing.T, wt, rel string, src []byte) ([]byte, int64, int64) {
	t.Helper()
	p := filepath.Join(wt, rel)
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(1_600_000_000+mtimes.Add(1), 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return src, info.ModTime().UnixMilli(), info.Size()
}

// writeGo writes rel in wt defining fns and returns its bytes and stat, as the
// post-edit hook passes them.
func writeGo(t *testing.T, wt, rel string, fns ...string) ([]byte, int64, int64) {
	t.Helper()
	src := "package p\n"
	for _, fn := range fns {
		src += "\nfunc " + fn + "() {}\n"
	}
	return writeSrc(t, wt, rel, []byte(src))
}

// hasSymbol reports whether e's graph of wt holds a symbol named name.
func hasSymbol(t *testing.T, e *Engine, wt, name string) bool {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM nodes n JOIN files f ON f.id = n.file_id
		WHERE f.worktree = ? AND n.name = ?`, wt, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestEditDeferredWhileIndexingIsInWhenTheIndexEnds(t *testing.T) {
	e1, e2 := twoEngines(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeGo(t, wt, "a.go", "Old")
	src := newHeldReadSource(wt)
	done1 := make(chan error, 1)
	go func() { done1 <- e1.ReconcileIfStale(ctx, wt, src, "") }()
	src.awaitRead(t) // e1 holds the index lock and the bytes of Old

	// Hold the store's write lock, as the indexer's resolve does.
	inTx, endTx, txDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		txDone <- e1.st.WriteTx(ctx, func(store.Conn) error {
			close(inTx)
			<-endTx
			return nil
		})
	}()
	select {
	case <-inTx:
	case <-time.After(hangTimeout):
		t.Fatal("write transaction never started")
	}

	b, mtime, size := writeGo(t, wt, "a.go", "NewName")
	edit := make(chan error, 1)
	go func() { edit <- e2.ReindexFileIncremental(ctx, wt, "a.go", b, mtime, size) }()
	awaitErr(t, "post-edit while the indexer holds the write lock", edit)
	close(endTx)
	awaitErr(t, "held write transaction", txDone)
	if hasSymbol(t, e2, wt, "NewName") {
		t.Fatal("the post-edit wrote graph rows beside the running index")
	}

	src.release <- struct{}{}
	src.awaitRead(t) // the drain pass reads the edit
	src.release <- struct{}{}
	awaitErr(t, "index", done1)
	if !hasSymbol(t, e1, wt, "NewName") || hasSymbol(t, e1, wt, "Old") {
		t.Errorf("after the index: NewName=%v Old=%v; want the edit in", hasSymbol(t, e1, wt, "NewName"), hasSymbol(t, e1, wt, "Old"))
	}
}

func TestEditDeferredDuringTheDrainPassIsIn(t *testing.T) {
	e1, e2 := twoEngines(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeGo(t, wt, "a.go", "Old")
	src := newHeldReadSource(wt)
	done1 := make(chan error, 1)
	go func() { done1 <- e1.ReconcileIfStale(ctx, wt, src, "tok") }()
	src.awaitRead(t)

	edit := func(fn string) {
		b, mtime, size := writeGo(t, wt, "a.go", fn)
		if err := e2.ReindexFileIncremental(ctx, wt, "a.go", b, mtime, size); err != nil {
			t.Fatal(err)
		}
	}
	edit("Second")
	src.release <- struct{}{}
	src.awaitRead(t) // the drain pass for Second
	edit("ThirdOne")
	src.release <- struct{}{}
	src.awaitRead(t) // the next drain pass
	src.release <- struct{}{}
	awaitErr(t, "index", done1)
	if !hasSymbol(t, e1, wt, "ThirdOne") {
		t.Error("an edit deferred during the drain pass is not in the graph")
	}
}

func TestPostEditTakesInAMarkNobodyDrained(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeGo(t, wt, "a.go", "A")
	writeGo(t, wt, "b.go", "B")
	if err := e.ReconcileIfStale(ctx, wt, FSSource{Root: wt}, ""); err != nil {
		t.Fatal(err)
	}
	// A post-edit whose try-lock failed just before the holder released and ran
	// its last check.
	writeGo(t, wt, "b.go", "Later")
	if err := e.markDeferred(wt); err != nil {
		t.Fatal(err)
	}

	b, mtime, size := writeGo(t, wt, "a.go", "AA")
	if err := e.ReindexFileIncremental(ctx, wt, "a.go", b, mtime, size); err != nil {
		t.Fatal(err)
	}
	if !hasSymbol(t, e, wt, "Later") {
		t.Error("the marked edit of b.go is not in the graph")
	}
}

// deferFixture is a worktree indexed with a.go (A) and b.go (B), an index
// engine and a post-edit hook engine sharing one fake clock. The clock moves
// only while the index is parked in a read or has returned.
type deferFixture struct {
	t         *testing.T
	ctx       context.Context
	clk       *clock.Fake
	idx, hook *Engine
	wt        string
}

func newDeferFixture(t *testing.T) *deferFixture {
	t.Helper()
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	idx, hook := twoEnginesOn(t, clk)
	f := &deferFixture{t: t, ctx: context.Background(), clk: clk, idx: idx, hook: hook, wt: t.TempDir()}
	writeGo(t, f.wt, "a.go", "A")
	writeGo(t, f.wt, "b.go", "B")
	if err := idx.Reconcile(f.ctx, f.wt, FSSource{Root: f.wt}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	return f
}

// now is the clock's time, Unix ms.
func (f *deferFixture) now() int64 { return f.clk.Now().UnixMilli() }

// tick advances the clock a second and returns the new time.
func (f *deferFixture) tick() int64 {
	f.clk.Advance(time.Second)
	return f.now()
}

// hold starts an index of the worktree and returns once it is parked in a
// read. The caller has changed one file since the last index, so the pass
// reads only that file.
func (f *deferFixture) hold() (heldReadSource, chan error) {
	f.t.Helper()
	src := newHeldReadSource(f.wt)
	done := make(chan error, 1)
	go func() { done <- f.idx.ReconcileIfStale(f.ctx, f.wt, src, "") }()
	src.awaitRead(f.t)
	return src, done
}

// edit writes rel defining fns and runs the post-edit hook on it.
func (f *deferFixture) edit(rel string, fns ...string) {
	f.t.Helper()
	b, mtime, size := writeGo(f.t, f.wt, rel, fns...)
	f.postEdit(rel, b, mtime, size)
}

func (f *deferFixture) postEdit(rel string, b []byte, mtime, size int64) {
	f.t.Helper()
	if err := f.hook.ReindexFileIncremental(f.ctx, f.wt, rel, b, mtime, size); err != nil {
		f.t.Fatal(err)
	}
}

// addSession writes a session of wt; end 0 leaves it open.
func addSession(t *testing.T, e *Engine, wt, id string, start, end int64) {
	t.Helper()
	var ended any
	if end != 0 {
		ended = end
	}
	if _, err := e.st.DB.Exec(`INSERT INTO sessions(id, worktree, started_at, ended_at) VALUES (?,?,?,?)`,
		id, wt, start, ended); err != nil {
		t.Fatal(err)
	}
}

// changesSince returns DeltasSince(since) as qualified name → change.
func changesSince(t *testing.T, e *Engine, wt string, since int64) map[string]string {
	t.Helper()
	ds, _, err := e.DeltasSince(context.Background(), wt, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range ds {
		out[d.QualifiedName] = d.Change
	}
	return out
}

// listed is what the start of session pendingID, started at start and not
// written yet, lists as changed since the last working session.
func listed(t *testing.T, e *Engine, wt, pendingID string, start int64) (baseline int64, ok bool, changes map[string]string) {
	t.Helper()
	baseline, ok, err := e.DeltaBaselineWith(context.Background(), wt, pendingID, start, e.clk.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		changes = changesSince(t, e, wt, baseline)
	}
	return baseline, ok, changes
}

// wantListed fails unless a session starting now lists exactly want, changed
// since the session that started at baseline.
func (f *deferFixture) wantListed(baseline int64, want map[string]string) {
	f.t.Helper()
	got, ok, changes := listed(f.t, f.idx, f.wt, ids.New(), f.tick())
	if !ok || got != baseline || !maps.Equal(changes, want) {
		f.t.Errorf("listed since %d (ok=%v): %v; want since %d: %v", got, ok, changes, baseline, want)
	}
}

// rowsIn counts wt's journal rows dated within [from, to].
func rowsIn(t *testing.T, e *Engine, wt string, from, to int64) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM graph_deltas WHERE worktree=? AND at BETWEEN ? AND ?`,
		wt, from, to).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// release lets the parked read go on, and waits for the next read when next.
func release(t *testing.T, src heldReadSource, next bool) {
	t.Helper()
	src.release <- struct{}{}
	if next {
		src.awaitRead(t)
	}
}

func TestDeferredEditCountsInItsSessionOnceIndexed(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Extra")
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	s2 := f.tick()
	addSession(t, f.idx, f.wt, "s2", s2, f.tick()) // starts and ends while the index runs
	f.tick()
	release(t, src, true) // the drain pass reads a.go
	release(t, src, false)
	awaitErr(t, "index", done)

	s3 := f.tick()
	f.wantListed(s1, map[string]string{"Extra": "added"})
	addSession(t, f.idx, f.wt, "s3", s3, 0)
	if got, ok, err := f.idx.DeltaBaseline(f.ctx, f.wt, f.now()); err != nil || !ok || got != s1 {
		t.Errorf("DeltaBaseline = %d, %v, %v; want S1's start %d", got, ok, err, s1)
	}
}

func TestCoalescedDeferredEditsCountInTheLastEditsSession(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	e1 := f.tick()
	addSession(t, f.idx, f.wt, "s1", s1, e1)
	s2 := f.tick()
	f.edit("a.go", "A", "Bar")
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())
	f.tick()
	release(t, src, true) // one read of a.go, holding both edits
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s2, map[string]string{"Bar": "added"})
	if n := rowsIn(t, f.idx, f.wt, s1, e1); n != 0 {
		t.Errorf("S1's window holds %d journal rows; want none", n)
	}
}

func TestEditAfterTheIndexReadTheFileWaitsForTheNextRead(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, true) // the drain pass reads a.go with Foo
	s2 := f.tick()
	f.edit("a.go", "A", "Foo", "Bar")
	e2 := f.tick()
	addSession(t, f.idx, f.wt, "s2", s2, e2)
	f.tick()
	release(t, src, true) // the next pass reads a.go with Bar

	f.wantListed(s1, map[string]string{"Foo": "added"})
	if n := rowsIn(t, f.idx, f.wt, s2, e2); n != 0 {
		t.Errorf("S2's window holds %d journal rows before Bar is read; want none", n)
	}
	release(t, src, false)
	awaitErr(t, "index", done)
	f.wantListed(s2, map[string]string{"Bar": "added"})
}

func TestEditsRestoringTheReadBytesDoNotTakeTheReadsCredit(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, true) // the drain pass reads a.go with Foo
	s2 := f.tick()
	f.edit("a.go", "A", "Foo", "Bar")
	f.tick()
	f.edit("a.go", "A", "Foo") // the bytes the pass read
	e2 := f.tick()
	addSession(t, f.idx, f.wt, "s2", s2, e2)
	f.tick()
	release(t, src, true) // the next pass reads the restored a.go
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"Foo": "added"})
	if n := rowsIn(t, f.idx, f.wt, s2, e2); n != 0 {
		t.Errorf("S2's window holds %d journal rows; want none", n)
	}
}

func TestEditRecordedAfterTheReadCountsInItsSession(t *testing.T) {
	f := newDeferFixture(t)
	s1 := f.tick()
	b, mtime, size := writeGo(t, f.wt, "a.go", "A", "Foo") // S1's edit, before its hook runs
	src, done := f.hold()                                  // the pass reads it
	f.tick()
	f.postEdit("a.go", b, mtime, size)
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"Foo": "added"})
}

func TestEditFollowedByAnUnhookedChangeCountsInTheEditsSession(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	// A formatter run no hook reports: the same symbols in other bytes.
	writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {\n}\n"))
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	s2 := f.tick()
	release(t, src, true) // the drain pass reads a.go during S2
	release(t, src, false)
	awaitErr(t, "index", done)
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())

	f.wantListed(s1, map[string]string{"Foo": "added"})
}

func TestUnreportedChangeCountsWhenTheIndexWritesIt(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "a.go", "A", "Pulled") // a pull: no hook reports it
	src, done := f.hold()
	s2 := f.tick()
	f.tick()
	release(t, src, false)
	awaitErr(t, "index", done)
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())

	f.wantListed(s2, map[string]string{"Pulled": "added"})
}

// laterChangeCountsInItsSession fails unless an unreported change of a.go in a
// later session counts in that session: no record left over takes it to an
// older one.
func (f *deferFixture) laterChangeCountsInItsSession() {
	f.t.Helper()
	s9 := f.tick()
	writeGo(f.t, f.wt, "a.go", "A", "Foo", "Later")
	f.tick()
	if err := f.idx.ReconcileIfStale(f.ctx, f.wt, FSSource{Root: f.wt}, ""); err != nil {
		f.t.Fatal(err)
	}
	addSession(f.t, f.idx, f.wt, "s9", s9, f.tick())
	if got, ok, changes := listed(f.t, f.idx, f.wt, "s10", f.tick()); !ok || got != s9 || changes["Later"] != "added" {
		f.t.Errorf("listed since %d (ok=%v): %v; want Later added since S9's start %d", got, ok, changes, s9)
	}
}

func TestLateEditRecordCreditsNothingLater(t *testing.T) {
	f := newDeferFixture(t)
	b, mtime, size := writeGo(t, f.wt, "a.go", "A", "Foo") // an edit whose hook has not run yet
	src, done := f.hold()                                  // the pass reads it
	f.tick()
	f.edit("b.go", "B", "B2") // its mark starts a drain pass
	release(t, src, true)     // a.go is written with no record naming it; the drain pass reads b.go
	f.tick()
	f.postEdit("a.go", b, mtime, size) // a.go's hook, late
	release(t, src, false)
	awaitErr(t, "index", done)

	f.laterChangeCountsInItsSession()
}

func TestRevertedEditCreditsNothingLater(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	f.tick()
	f.edit("a.go", "A", "Foo")
	writeGo(t, f.wt, "a.go", "A") // reverted, unreported: the stored bytes again
	f.tick()
	release(t, src, true) // the drain pass reads a.go and finds the stored bytes
	release(t, src, false)
	awaitErr(t, "index", done)

	f.laterChangeCountsInItsSession()
}

func TestFileRemovedAndRecreatedDuringAnIndexReadsAsChanged(t *testing.T) {
	f := newDeferFixture(t)
	if err := os.Remove(filepath.Join(f.wt, "a.go")); err != nil {
		t.Fatal(err)
	}
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold() // listed without a.go
	f.tick()
	f.edit("a.go", "A") // an agent re-creates it
	f.tick()
	release(t, src, true) // the pass deletes a.go's graph after the record; the drain pass reads a.go
	release(t, src, false)
	awaitErr(t, "index", done)

	if got := changesSince(t, f.idx, f.wt, 0); !maps.Equal(got, map[string]string{"A": "changed"}) {
		t.Errorf("changes = %v; want A changed", got)
	}
}

func TestUnreadableEditRecordDoesNotFailTheIndex(t *testing.T) {
	f := newDeferFixture(t)
	b, _, _ := writeGo(t, f.wt, "a.go", "A", "Foo")
	if err := f.idx.recordEdit(f.wt, "a.go", b, f.now()); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(f.idx.editsDir(f.wt), "cut-short.json")
	if err := os.WriteFile(bad, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if err := f.idx.ReconcileIfStale(f.ctx, f.wt, FSSource{Root: f.wt}, ""); err != nil {
		t.Fatalf("index with an unreadable record: %v", err)
	}
	if !hasSymbol(t, f.idx, f.wt, "Foo") {
		t.Error("the edit is not in the graph")
	}
	if _, err := os.Stat(bad); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the unreadable record is still there: %v", err)
	}
}
