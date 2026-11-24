package graph

import (
	"context"
	"errors"
	"fmt"
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
// holds the call until the test sends on release: an index that got the file's
// bytes just before an edit, and whose Read has not returned yet.
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

// heldWrite holds an index after each file it read, before it writes it, until
// the test sends on release: an index whose read of the file returned just
// before an edit. The drain passes after the held pass report to no Progress.
type heldWrite struct {
	noopProgress
	read    chan struct{} // receives once per file read
	release chan struct{} // one send per file read
}

func (h heldWrite) Inc() {
	h.read <- struct{}{}
	<-h.release
}

// holdWrite starts an index of the worktree and returns once its read of the
// one file the caller changed since the last index has returned, before the
// pass writes it.
func (f *deferFixture) holdWrite() (heldWrite, chan error) {
	f.t.Helper()
	h := heldWrite{read: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- f.idx.ReconcileIfStale(f.ctx, f.wt, FSSource{Root: f.wt}, "", WithProgress(h)) }()
	select {
	case <-h.read:
	case <-time.After(hangTimeout):
		f.t.Fatal("no read came")
	}
	return h, done
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
	held, done := f.holdWrite()                            // the pass has read it
	f.tick()
	f.postEdit("a.go", b, mtime, size)
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	held.release <- struct{}{}
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
	release(t, src, true)              // its record makes the next pass read a.go again
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

func TestFileRecreatedDuringAnIndexCountsInTheRecreatingSession(t *testing.T) {
	f := newDeferFixture(t)
	if err := os.Remove(filepath.Join(f.wt, "a.go")); err != nil {
		t.Fatal(err)
	}
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold() // listed without a.go
	s1 := f.tick()
	f.edit("a.go", "A", "Extra") // an agent re-creates it
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	s2 := f.tick()
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())
	f.tick()
	release(t, src, true) // the pass deletes a.go's graph after S1; the drain pass reads a.go
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"A": "added", "Extra": "added"})
}

func TestSameStatEditAfterTheReadIsReadByTheNextPass(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	b, mtime, size := writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {}\n\n// padding...\n"))
	f.postEdit("a.go", b, mtime, size)
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, true) // the drain pass reads the padded a.go
	s2 := f.tick()
	// Same length, and the mtime set back: the stat the pass stores.
	b = []byte("package p\n\nfunc A() {}\n\nfunc Foo() {}\n\nfunc Bar() {}\n")
	if int64(len(b)) != size {
		t.Fatalf("S2's bytes are %d long; want %d", len(b), size)
	}
	p := filepath.Join(f.wt, "a.go")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.UnixMilli(mtime), time.UnixMilli(mtime)); err != nil {
		t.Fatal(err)
	}
	f.postEdit("a.go", b, mtime, size)
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())
	f.tick()
	release(t, src, true) // the next pass reads a.go although its stat is the stored one

	f.wantListed(s1, map[string]string{"Foo": "added"})
	release(t, src, false)
	awaitErr(t, "index", done)
	f.wantListed(s2, map[string]string{"Bar": "added"})
}

func TestRestoredBytesThenAnUnhookedChangeCountInTheRestoringSession(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, true) // the drain pass reads S1's a.go
	s2 := f.tick()
	f.edit("a.go", "A", "Foo", "Bar")
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())
	s3 := f.tick()
	f.edit("a.go", "A", "Foo") // the bytes the pass read
	f.tick()
	// A formatter run no hook reports: the same symbols in other bytes.
	writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {\n}\n"))
	addSession(t, f.idx, f.wt, "s3", s3, f.tick())
	f.tick()
	release(t, src, true) // the next pass reads the formatted a.go

	f.wantListed(s1, map[string]string{"Foo": "added"})
	release(t, src, false)
	awaitErr(t, "index", done)
	f.wantListed(s3, map[string]string{"Foo": "changed"})
}

// unreportedThenEdit runs a pull that rewrites a.go to pulled, read by a held
// index; S1, starting gap after that read, edits a.go to edited. The index
// writes both after S1 ended, while S2 is open. It returns S1's and S2's starts.
func (f *deferFixture) unreportedThenEdit(pulled, edited string, gap time.Duration) (s1, s2 int64) {
	f.t.Helper()
	writeSrc(f.t, f.wt, "a.go", []byte(pulled)) // a pull: no hook reports it
	src, done := f.hold()
	f.clk.Advance(gap)
	s1 = f.now()
	f.tick()
	b, mtime, size := writeSrc(f.t, f.wt, "a.go", []byte(edited))
	f.postEdit("a.go", b, mtime, size)
	addSession(f.t, f.idx, f.wt, "s1", s1, f.tick())
	s2 = f.tick()
	addSession(f.t, f.idx, f.wt, "s2", s2, 0)
	f.tick()
	release(f.t, src, true) // the pass writes the pulled a.go; the drain pass reads S1's
	release(f.t, src, false)
	awaitErr(f.t, "index", done)
	return s1, s2
}

func TestEditOfAnUnreportedChangeListsInItsSession(t *testing.T) {
	const a = "package p\n\nfunc A() {}\n"
	const pulled = a + "\nfunc Pulled() {}\n"
	for _, tc := range []struct {
		name           string
		pulled, edited string
		gap            time.Duration
		inS2           bool // the listing starts at S2, not S1
		want           map[string]string
	}{
		{name: "removes it", pulled: pulled, edited: a, gap: time.Second,
			want: map[string]string{"Pulled": "removed"}},
		{name: "reshapes it", pulled: pulled, edited: a + "\nfunc Pulled() {\n}\n", gap: time.Second,
			want: map[string]string{"Pulled": "changed"}},
		// Unrelated comes first so its span does not move: S1 leaves it as pulled.
		{name: "removes one of two", pulled: a + "\nfunc Unrelated() {}\n" + "\nfunc Pulled() {}\n",
			edited: a + "\nfunc Unrelated() {}\n", gap: time.Second,
			want: map[string]string{"Pulled": "removed"}},
		{name: "starts in the read's millisecond", pulled: pulled, edited: a,
			want: map[string]string{"Pulled": "removed"}},
		{name: "changes no symbol", pulled: pulled, edited: pulled + "\n// trailing note\n", gap: time.Second,
			inS2: true, want: map[string]string{"Pulled": "added"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferFixture(t)
			s1, s2 := f.unreportedThenEdit(tc.pulled, tc.edited, tc.gap)
			since := s1
			if tc.inS2 {
				since = s2
			}
			f.wantListed(since, tc.want)
			addSession(t, f.idx, f.wt, "s3", f.tick(), 0)
			if got, ok, err := f.idx.DeltaBaseline(f.ctx, f.wt, f.now()); err != nil || !ok || got != since {
				t.Errorf("DeltaBaseline = %d, %v, %v; want %d", got, ok, err, since)
			}
		})
	}
}

func TestEditsRestoringAnUnreportedChangeTheIndexReadTakeNoCredit(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "a.go", "A", "Pulled") // a pull: no hook reports it
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Pulled", "Temp")
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	s2 := f.tick()
	f.edit("a.go", "A", "Pulled") // the bytes the pass read
	e2 := f.tick()
	addSession(t, f.idx, f.wt, "s2", s2, e2)
	s3 := f.tick()
	addSession(t, f.idx, f.wt, "s3", s3, 0)
	f.tick()
	release(t, src, true) // the pass writes the pull; the drain pass reads the restored a.go
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s3, map[string]string{"Pulled": "added"})
	if n := rowsIn(t, f.idx, f.wt, s1, e2); n != 0 {
		t.Errorf("S1's and S2's windows hold %d journal rows; want none", n)
	}
}

func TestEditOfAFileGoneBeforeTheReadCreditsNothingLater(t *testing.T) {
	f := newDeferFixture(t)
	if err := os.Remove(filepath.Join(f.wt, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := f.idx.ReconcileIfStale(f.ctx, f.wt, FSSource{Root: f.wt}, ""); err != nil {
		t.Fatal(err)
	}
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	if err := os.Remove(filepath.Join(f.wt, "a.go")); err != nil { // gone again, unreported
		t.Fatal(err)
	}
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, false) // the drain pass lists no a.go and reads nothing
	awaitErr(t, "index", done)

	f.laterChangeCountsInItsSession()
}

// listingHeld starts an index of f's worktree whose listing waits until the
// returned gate closes, and returns once the listing has begun.
func (f *deferFixture) listingHeld() (gate chan struct{}, done chan error) {
	f.t.Helper()
	src := &gatedSource{inner: FSSource{Root: f.wt}, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	done = make(chan error, 1)
	go func() { done <- f.idx.ReconcileIfStale(f.ctx, f.wt, src, "") }()
	select {
	case <-src.entered:
	case <-time.After(hangTimeout):
		f.t.Fatal("the index did not begin listing")
	}
	return src.gate, done
}

func TestDeletionSeenDuringAListingCountsInTheEditingSession(t *testing.T) {
	f := newDeferFixture(t)
	gate, done := f.listingHeld()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	f.tick()
	if err := os.Remove(filepath.Join(f.wt, "a.go")); err != nil { // unreported
		t.Fatal(err)
	}
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	close(gate) // the listing finds a.go gone, after S1's hook and the removal
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"A": "removed"})
}

func TestHookAndReadInOneMillisecondKeepTheirOrder(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.tick()
	f.edit("a.go", "A", "Foo")
	f.clk.Advance(200 * time.Microsecond)
	// A formatter run no hook reports: the same symbols in other bytes.
	writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {\n}\n"))
	f.clk.Advance(200 * time.Microsecond)
	addSession(t, f.idx, f.wt, "s1", s1, f.now())
	f.clk.Advance(200 * time.Microsecond)
	release(t, src, true) // the drain reads a.go in the hook's millisecond, after it
	f.tick()
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"Foo": "added"})
}

func TestFileWhoseDirectoryBecameAFileDuringAListingCountsInTheEditingSession(t *testing.T) {
	f := newDeferFixture(t)
	pkg := filepath.Join(f.wt, "pkg")
	if err := os.Mkdir(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGo(t, f.wt, "pkg/c.go", "C")
	if err := f.idx.ReconcileIfStale(f.ctx, f.wt, FSSource{Root: f.wt}, ""); err != nil {
		t.Fatal(err)
	}
	f.tick()
	gate, done := f.listingHeld()
	s1 := f.tick()
	f.edit("pkg/c.go", "C", "Foo")
	f.tick()
	// A branch switch, unreported: the directory is now a file.
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	close(gate)
	awaitErr(t, "index", done)

	f.wantListed(s1, map[string]string{"C": "removed"})
}

// heldStart holds an index between its listing and its reads until the test
// closes release. The drain passes after the held pass report to no Progress.
type heldStart struct {
	noopProgress
	started chan struct{} // closed once the listing is done
	release chan struct{}
}

func (h heldStart) Start(int) {
	close(h.started)
	<-h.release
}

// editsDuringTheRead starts an index of f's worktree, which then reads only
// a.go. It runs between, when given, once the listing is done; then it holds
// the read of a.go before the read gets the bytes, while each of edits, in a
// session of its own, rewrites a.go to define those functions. It returns the
// sessions' starts once the index has ended.
func (f *deferFixture) editsDuringTheRead(between func(), edits ...[]string) []int64 {
	f.t.Helper()
	src := &gateFirstRead{inner: FSSource{Root: f.wt}, started: make(chan struct{}), release: make(chan struct{})}
	listed := heldStart{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- f.idx.ReconcileIfStale(f.ctx, f.wt, src, "", WithProgress(listed)) }()
	for _, c := range []chan struct{}{listed.started, src.started} {
		select {
		case <-c:
		case <-time.After(hangTimeout):
			f.t.Fatal("the index did not reach its read of a.go")
		}
		if c == listed.started {
			f.tick()
			if between != nil {
				between()
			}
			close(listed.release)
		}
	}
	var starts []int64
	for i, fns := range edits {
		s := f.tick()
		f.edit("a.go", fns...)
		addSession(f.t, f.idx, f.wt, fmt.Sprintf("s%d", i+1), s, f.tick())
		starts = append(starts, s)
	}
	f.tick()
	close(src.release)
	awaitErr(f.t, "index", done)
	return starts
}

func TestEditsMadeWhileAReadWaitsForTheBytesCountInTheLastEditsSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  func(f *deferFixture) // before the index starts
		between func(f *deferFixture) // between its listing and its read
		edits   [][]string
		want    map[string]string // listed since the last edit's session
	}{{
		// The listed version is the stored one, so the bytes read are not it.
		name:   "two edits after a touch",
		before: func(f *deferFixture) { writeGo(f.t, f.wt, "a.go", "A") },
		edits:  [][]string{{"A", "Foo"}, {"A", "Bar"}},
		want:   map[string]string{"Bar": "added"},
	}, {
		// The first edit after the read began names the bytes read, though
		// they have the size of the unreported version the listing saw.
		name:   "one edit after an unreported change of that size",
		before: func(f *deferFixture) { writeGo(f.t, f.wt, "a.go", "A", "Pulled") },
		edits:  [][]string{{"A", "Barbar"}},
		want:   map[string]string{"Barbar": "added"},
	}, {
		// The listed version is the earlier edit's, which its record names.
		name: "two edits after an earlier deferred edit of that size",
		before: func(f *deferFixture) {
			unlock, ok, err := store.TryLockFile(f.idx.indexLockPath(f.wt)) // another index holds the worktree
			if err != nil || !ok {
				f.t.Fatalf("index lock: %v, %v", ok, err)
			}
			s0 := f.tick()
			f.edit("a.go", "A", "Foo")
			addSession(f.t, f.idx, f.wt, "s0", s0, f.tick())
			unlock()
		},
		edits: [][]string{{"A", "Baz"}, {"A", "Bar"}},
		want:  map[string]string{"Bar": "added"},
	}, {
		// An edit recorded after the listing, before the read: the read did not
		// get the listed version.
		name:   "two edits after one made between the listing and the read",
		before: func(f *deferFixture) { writeGo(f.t, f.wt, "a.go", "A", "Pulled") },
		between: func(f *deferFixture) {
			s0 := f.tick()
			f.edit("a.go", "A", "Foo")
			addSession(f.t, f.idx, f.wt, "s0", s0, f.tick())
		},
		edits: [][]string{{"A", "Bazbaz"}, {"A", "Barbar"}},
		want:  map[string]string{"Barbar": "added"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferFixture(t)
			tc.before(f)
			var between func()
			if tc.between != nil {
				between = func() { tc.between(f) }
			}
			starts := f.editsDuringTheRead(between, tc.edits...)
			f.wantListed(starts[len(starts)-1], tc.want)
		})
	}
}

func TestEditDuringTheReadOfAnUnhookedChangeWaitsForTheNextRead(t *testing.T) {
	f := newDeferFixture(t)
	writeGo(t, f.wt, "b.go", "B")
	src, done := f.hold()
	s1 := f.tick()
	f.edit("a.go", "A", "Foo")
	// A formatter run no hook reports: the same symbols in other bytes.
	writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {\n}\n"))
	addSession(t, f.idx, f.wt, "s1", s1, f.tick())
	f.tick()
	release(t, src, true) // the drain pass has the formatted bytes
	s2 := f.tick()
	b, mtime, size := writeSrc(t, f.wt, "a.go", []byte("package p\n\nfunc A() {}\n\nfunc Foo() {\n}\n\nfunc Bar() {}\n"))
	f.postEdit("a.go", b, mtime, size)
	addSession(t, f.idx, f.wt, "s2", s2, f.tick())
	f.tick()
	release(t, src, true) // S2's record makes the next pass read a.go
	f.wantListed(s1, map[string]string{"Foo": "added"})
	release(t, src, false)
	awaitErr(t, "index", done)

	f.wantListed(s2, map[string]string{"Bar": "added"})
}

func TestUnreadableEditRecordDoesNotFailTheIndex(t *testing.T) {
	f := newDeferFixture(t)
	b, mtime, size := writeGo(t, f.wt, "a.go", "A", "Foo")
	if err := f.idx.recordEdit(f.wt, "a.go", b, mtime, size, f.clk.Now().UnixNano()); err != nil {
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
