package agents

import (
	"context"
	"os"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// TestStopCheckpointsWAL: the Stop hook is the natural turn boundary to flush
// the WAL — the agent is idle, so the TRUNCATE checkpoint can actually win.
// Without it the WAL only ever shrinks mid-index, which silently fails under
// concurrent readers.
func TestStopCheckpointsWAL(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, Agent: "opencode", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	wal := a.Store.Path() + "-wal"
	if info, err := os.Stat(wal); err == nil && info.Size() != 0 {
		t.Fatalf("WAL is %d bytes after Stop hook, want 0 (truncated)", info.Size())
	}
}

// blockingSource never returns from List, simulating a first index of a huge
// repo inside the session-start hook.
type blockingSource struct{ release chan struct{} }

func (b blockingSource) List() ([]graph.FileEntry, error) {
	<-b.release
	return nil, nil
}
func (b blockingSource) Read(string) ([]byte, error) { return nil, nil }

// TestSessionStartIndexBudget: the session-start graph warm-up must be bounded.
// Unbounded, a first index of a large repo blocks the host's hook (and with it
// the user's first message) for minutes; aborting at the budget is safe — the
// crash-safe marker lets a later reconcile finish the work.
func TestSessionStartIndexBudget(t *testing.T) {
	a := newTestApp(t, "main")

	src := blockingSource{release: make(chan struct{})}
	defer close(src.release)
	prevSource, prevBudget := newGraphSource, sessionStartIndexBudget
	newGraphSource = func(string) graph.Source { return src }
	sessionStartIndexBudget = 50 * time.Millisecond
	defer func() { newGraphSource, sessionStartIndexBudget = prevSource, prevBudget }()

	done := make(chan error, 1)
	go func() {
		_, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, SessionID: "s1"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session-start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session-start still blocked after 5s: the graph warm-up must be budget-bounded")
	}
}
