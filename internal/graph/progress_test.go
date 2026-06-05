package graph

import (
	"context"
	"testing"
)

// recProgress is a test Progress that records the calls Reconcile makes.
type recProgress struct {
	startCalls int
	total      int
	incs       int
	doneCalls  int
}

func (p *recProgress) Start(total int) { p.startCalls++; p.total = total }
func (p *recProgress) Inc()            { p.incs++ }
func (p *recProgress) Phase(string)    {}
func (p *recProgress) Done()           { p.doneCalls++ }

func TestReconcileReportsProgress(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	writeFile(t, wt, "a.go", "package p\nfunc A(){}")
	writeFile(t, wt, "b.go", "package p\nfunc B(){}")
	writeFile(t, wt, "notes.md", "ignore me") // unsupported ext: never listed

	p := &recProgress{}
	src := FSSource{Root: wt}
	if err := e.Reconcile(ctx, wt, src, WithProgress(p)); err != nil {
		t.Fatal(err)
	}
	if p.startCalls != 1 {
		t.Errorf("Start called %d times, want 1", p.startCalls)
	}
	if p.total != 2 {
		t.Errorf("Start total = %d, want 2 (a.go, b.go)", p.total)
	}
	if p.incs != 2 {
		t.Errorf("Inc called %d times, want 2", p.incs)
	}
	if p.doneCalls != 1 {
		t.Errorf("Done called %d times, want 1", p.doneCalls)
	}

	// A second reconcile with nothing changed must report an empty worklist: the
	// total reflects files that actually need (re)indexing, not all source files.
	p2 := &recProgress{}
	if err := e.Reconcile(ctx, wt, src, WithProgress(p2)); err != nil {
		t.Fatal(err)
	}
	if p2.total != 0 || p2.incs != 0 {
		t.Errorf("unchanged reconcile reported total=%d incs=%d, want 0/0", p2.total, p2.incs)
	}
	if p2.startCalls != 1 || p2.doneCalls != 1 {
		t.Errorf("unchanged reconcile Start=%d Done=%d, want 1/1", p2.startCalls, p2.doneCalls)
	}
}
