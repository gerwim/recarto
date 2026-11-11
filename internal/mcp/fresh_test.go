package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/graph"
)

func TestFreshnessHintMapping(t *testing.T) {
	if got := freshnessHint(graph.Freshness{State: graph.Fresh}); got != "" {
		t.Errorf("Fresh hint = %q, want empty", got)
	}
	got := freshnessHint(graph.Freshness{State: graph.FirstIndex, Detail: "12/80 files indexed"})
	if !strings.Contains(got, "12/80 files indexed") || !strings.Contains(got, "background") {
		t.Errorf("FirstIndex hint %q should carry progress and say the index runs in the background", got)
	}
	got = freshnessHint(graph.Freshness{State: graph.Refreshing, Detail: "resolving references"})
	if !strings.Contains(got, "resolving references") || !strings.Contains(got, "stale") {
		t.Errorf("Refreshing hint %q should carry the detail and warn about staleness", got)
	}
	// A resolve sub-phase detail (prefix-compatible with the base) must flow through
	// unchanged, so a polling client sees which stage is running.
	got = freshnessHint(graph.Freshness{State: graph.Refreshing, Detail: "resolving references (4/8: name fallback)"})
	if !strings.Contains(got, "resolving references (4/8: name fallback)") || !strings.Contains(got, "stale") {
		t.Errorf("Refreshing hint %q should carry the resolve sub-phase detail", got)
	}
}

func TestJoinHints(t *testing.T) {
	if got := joinHints("", ""); got != "" {
		t.Errorf("joinHints empty = %q", got)
	}
	if got := joinHints("a.", ""); got != "a." {
		t.Errorf("joinHints(a, '') = %q", got)
	}
	if got := joinHints("", "b."); got != "b." {
		t.Errorf("joinHints('', b) = %q", got)
	}
	if got := joinHints("a.", "b."); got != "a. b." {
		t.Errorf("joinHints(a, b) = %q", got)
	}
}

// gatedListSource blocks List until released, holding the worktree's
// singleflight refresh slot so a handler call observes an in-flight refresh.
type gatedListSource struct {
	root string
	gate chan struct{}
}

func (g gatedListSource) List() ([]graph.FileEntry, error) {
	<-g.gate
	return graph.FSSource{Root: g.root}.List()
}
func (g gatedListSource) Read(p string) ([]byte, error) {
	return graph.FSSource{Root: g.root}.Read(p)
}

// TestHandlersCarryFreshnessHint: while a refresh is in flight, query handlers
// must answer immediately (bounded wait, stale data) and tell the agent so via
// the hint — across handlers that already had hints (search_graph) and ones
// that did not (search_content, get_architecture).
func TestHandlersCarryFreshnessHint(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Authenticate(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	prevWait := freshWait
	freshWait = 20 * time.Millisecond
	defer func() { freshWait = prevWait }()

	// Occupy the worktree's refresh slot with a gated reconcile.
	gate := make(chan struct{})
	occupied := a.Graph.EnsureFresh(ctx, wt, gatedListSource{root: wt, gate: gate}, "tok-gated", 0)
	if occupied.State == graph.Fresh {
		t.Fatal("setup: gated refresh reported Fresh")
	}

	sg, err := searchGraph(ctx, a, SearchGraphInput{Query: "Authenticate"})
	if err != nil {
		t.Fatal(err)
	}
	if sg.Count != 1 {
		t.Fatalf("stale data must still be served during a refresh; got count=%d", sg.Count)
	}
	if !strings.Contains(sg.Hint, "background") {
		t.Errorf("search_graph hint %q should mention the background refresh", sg.Hint)
	}
	sc, err := searchContent(ctx, a, SearchContentInput{Pattern: "Authenticate"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sc.Hint, "background") {
		t.Errorf("search_content hint %q should mention the background refresh", sc.Hint)
	}
	ga, err := getArchitecture(ctx, a, GetArchitectureInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ga.Hint, "background") {
		t.Errorf("get_architecture hint %q should mention the background refresh", ga.Hint)
	}

	// Release the refresh; once it lands, hints disappear.
	close(gate)
	done := a.Graph.EnsureFresh(ctx, wt, gatedListSource{root: wt, gate: gate}, "tok-gated", 10*time.Second)
	if done.State != graph.Fresh {
		t.Fatalf("joined refresh state = %v (detail %q), want Fresh", done.State, done.Detail)
	}
	freshWait = 10 * time.Second
	sg2, err := searchGraph(ctx, a, SearchGraphInput{Query: "Authenticate"})
	if err != nil {
		t.Fatal(err)
	}
	if sg2.Hint != "" {
		t.Errorf("hint should be empty once the graph is fresh, got %q", sg2.Hint)
	}
}
