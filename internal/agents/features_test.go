package agents

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/features"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// off returns a Set with fs switched off.
func off(fs ...*features.Feature) features.Set {
	s := features.Set{}
	for _, f := range fs {
		s[f] = false
	}
	return s
}

// record stores a memory of kind anchored to file rel ("" for none).
func record(t *testing.T, a *app.App, kind memory.Kind, title, rel string) {
	t.Helper()
	nm := memory.NewMemory{Kind: kind, Title: title, Body: title + " body", Branch: a.Git.Branch, Source: "cli"}
	if rel != "" {
		nm.Refs = []memory.Ref{{Type: memory.RefFile, Value: rel}}
	}
	if _, _, _, err := a.Engine.Record(context.Background(), nm); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStartTiersFollowFeatures(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	record(t, a, memory.KindLearning, "wal mode needs busy timeout", "")
	record(t, a, memory.KindTodo, "split the handler file", "")
	record(t, a, memory.KindBug, "reindex drops edges", "")
	src := []byte("package p\n\nfunc Alpha() {}\n")
	if err := a.Graph.IndexFile(ctx, a.Git.Worktree, "pkg/a.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	start := func(id string, f features.Set) string {
		t.Helper()
		a.Features = f
		res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		return res.Context
	}
	const (
		header  = "Recarto memory for this repo/branch"
		learned = "wal mode needs busy timeout"
		todo    = "split the handler file"
		codeMap = "Code map (modules)"
		query   = "Query the code graph"
		bugs    = "Recent bugs"
		nudge   = "Record new learnings"
	)
	all := start("all", nil)
	for _, want := range []string{header, learned, todo, codeMap, query, bugs, nudge, "search_bugs"} {
		if !strings.Contains(all, want) {
			t.Fatalf("all features on: missing %q:\n%s", want, all)
		}
	}
	for _, tc := range []struct {
		f       features.Set
		gone    []string
		staying []string
	}{
		{off(features.SessionMemories), []string{header, learned, todo, nudge, "no prior memories"}, []string{codeMap, bugs}},
		{off(features.CodeMap), []string{codeMap, query}, []string{learned, bugs}},
		{off(features.RecentBugs), []string{bugs}, []string{learned, codeMap}},
		{off(features.Todos), []string{todo}, []string{learned}},
		{off(features.GraphTools), []string{query}, []string{codeMap}},
		{off(features.MemoryTools), []string{nudge, "search_bugs"}, []string{learned, "Recent bugs:\n- reindex drops edges"}},
	} {
		got := start(fmt.Sprint(tc.f), tc.f)
		for _, s := range tc.gone {
			if strings.Contains(got, s) {
				t.Errorf("%v: %q still shown:\n%s", tc.f, s, got)
			}
		}
		for _, s := range tc.staying {
			if !strings.Contains(got, s) {
				t.Errorf("%v: %q missing:\n%s", tc.f, s, got)
			}
		}
	}
	if got := start("none", off(features.SessionMemories, features.CodeMap, features.RecentBugs, features.RecentChanges)); got != "" {
		t.Errorf("every session-start tier off: got %q, want no context", got)
	}
}

func TestRecentChangesFollowsFeature(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := t.TempDir()
	a.Git.Worktree = wt
	clk := a.Clock.(*clock.Fake)
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"package p\nfunc Work() {}\n", "package p\nfunc Work() { _ = 1 }\nfunc Extra() {}\n"} {
		if err := os.WriteFile(filepath.Join(wt, "w.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	const tier = "Changed since your last working session"
	a.Features = off(features.RecentChanges)
	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Context, tier) {
		t.Errorf("recent-changes off, tier shown:\n%s", res.Context)
	}
	// Switched back on, the tier reports the same changes: the off session
	// did not use up or prune them.
	clk.Advance(time.Second)
	a.Features = nil
	if res, err = Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s3"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, tier) {
		t.Errorf("recent-changes on, tier missing:\n%s", res.Context)
	}
}

func TestSessionStartUpkeepFollowsFeatures(t *testing.T) {
	ctx := context.Background()
	meta := func(a *app.App, key string) bool {
		var v string
		return a.Store.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v) == nil
	}
	for _, on := range []bool{false, true} {
		a := newTestApp(t, "main")
		a.Git.Worktree = t.TempDir()
		if err := os.MkdirAll(filepath.Join(a.Git.Worktree, ".recarto"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.Git.Worktree, ".recarto", "memory.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if !on {
			a.Features = off(features.TeamSnapshot, features.AutoCompact)
		}
		if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s"}); err != nil {
			t.Fatal(err)
		}
		if meta(a, "snapshot_hash") != on || meta(a, "compact_last_run_at") != on {
			t.Errorf("features on=%v: snapshot imported=%v, compacted=%v", on, meta(a, "snapshot_hash"), meta(a, "compact_last_run_at"))
		}
	}
}

func TestReadAndEditHintsFollowFeatures(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	wt := a.Git.Worktree
	rel := "big.go"
	src := []byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	if err := a.Graph.IndexFile(ctx, wt, rel, src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	record(t, a, memory.KindBug, "alpha overflows", rel)
	record(t, a, memory.KindDoNotRepeat, "never inline beta", rel)
	record(t, a, memory.KindTodo, "rename alpha", rel)
	const (
		bug      = "prior bug in big.go"
		guidance = "do-not-repeat for big.go"
		todo     = "open todo for big.go"
		large    = "get_file_outline"
		repeat   = "already read"
	)
	read := func(session string, f features.Set) string {
		t.Helper()
		a.Features = f
		var out []string
		for range 2 {
			res, err := Handle(ctx, a, Event{Kind: KindPostRead, SessionID: session, FilePath: wt + "/" + rel, ToolResult: largeReadResult()})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, res.Context)
		}
		return strings.Join(out, "\n")
	}
	if got := read("all", nil); !strings.Contains(got, bug) || !strings.Contains(got, guidance) ||
		!strings.Contains(got, todo) || !strings.Contains(got, large) || !strings.Contains(got, repeat) {
		t.Fatalf("all features on, missing a hint:\n%s", got)
	}
	for name, tc := range map[string]struct {
		f    features.Set
		gone string
	}{
		"bug":      {off(features.BugWarnings), bug},
		"guidance": {off(features.Guidance), guidance},
		"todo":     {off(features.Todos), todo},
		"large":    {off(features.LargeReads), large},
		"tools":    {off(features.GraphTools), large},
		"repeat":   {off(features.RepeatedReads), repeat},
	} {
		got := read(name, tc.f)
		if strings.Contains(got, tc.gone) {
			t.Errorf("%v: %q still shown:\n%s", tc.f, tc.gone, got)
		}
		if !strings.Contains(got, "rename alpha") && tc.gone != todo {
			t.Errorf("%v: switched off more than it names:\n%s", tc.f, got)
		}
	}

	a.Features = off(features.BugWarnings, features.Guidance, features.Todos)
	res, err := Handle(ctx, a, Event{Kind: KindPostEdit, SessionID: "edit", FilePath: wt + "/" + rel})
	if err != nil {
		t.Fatal(err)
	}
	if res.Context != "" {
		t.Errorf("edit with bug-warnings, guidance and todos off: %q", res.Context)
	}
}

func TestSearchHintsFollowFeatures(t *testing.T) {
	a, dir := newTreeApp(t)
	ctx := context.Background()
	src := []byte("package p\n\nfunc Widget() {}\n")
	if err := a.Graph.IndexFile(ctx, dir, "widget.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	recordWidgetBug(t, a)
	grep := func(session string, f features.Set) string {
		t.Helper()
		a.Features = f
		res, err := Handle(ctx, a, Event{Kind: KindAugment, SessionID: session, Pattern: "Widget"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Context
	}
	if got := grep("on", nil); !strings.Contains(got, "widget.go:3") || !strings.Contains(got, "get_code_snippet") ||
		!strings.Contains(got, "Related bugs (search_bugs for detail):") {
		t.Fatalf("all on:\n%s", got)
	}
	if got := grep("off", off(features.SearchHints)); got != "" {
		t.Errorf("search-hints off: %q", got)
	}
	if got := grep("tools", off(features.GraphTools)); !strings.Contains(got, "widget.go:3") || strings.Contains(got, "get_code_snippet") {
		t.Errorf("graph-tools off must keep the symbols, drop the tool pointer:\n%s", got)
	}
	// memory-tools off unregisters search_bugs, but the bug titles stay.
	if got := grep("memory", off(features.MemoryTools)); !strings.Contains(got, "Related bugs:\n") ||
		!strings.Contains(got, "Widget overflow") || strings.Contains(got, "search_bugs") {
		t.Errorf("memory-tools off must keep the bugs, drop the search_bugs pointer:\n%s", got)
	}

	a.Features = off(features.ShellHints)
	if got := shellHook(t, a, Event{SessionID: "sh1", Command: "rg Widget", CWD: dir}); got != "" {
		t.Errorf("shell-hints off: %q", got)
	}
	a.Features = off(features.GraphTools)
	if got := shellHook(t, a, Event{SessionID: "sh2", Command: "rg Widget", CWD: dir}); !strings.Contains(got, "Widget overflow") || strings.Contains(got, "search_content") {
		t.Errorf("graph-tools off must drop the shell nudge only:\n%s", got)
	}
}

func TestStopFollowsFeatures(t *testing.T) {
	ctx := context.Background()
	stop := func(f features.Set, edits int64) Result {
		t.Helper()
		a := newTestApp(t, "main")
		a.Features = f
		if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, SessionID: "s"}); err != nil {
			t.Fatal(err)
		}
		_ = a.Ledger.Incr(ctx, "s", "edits", edits)
		res, err := Handle(ctx, a, Event{Kind: KindStop, SessionID: "s"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := stop(off(features.StopReminder), 0); res != (Result{}) {
		t.Errorf("stop-reminder off: %+v", res)
	}
	if res := stop(off(features.StopNudge), 3); res.Block || !strings.Contains(res.SystemMessage, "record any new learnings") {
		t.Errorf("stop-nudge off: %+v", res)
	}
	if res := stop(off(features.StopNudge, features.StopReminder), 3); res != (Result{}) {
		t.Errorf("both off: %+v", res)
	}
	if res := stop(off(features.IssueReports), 0); res.SystemMessage == "" || strings.Contains(res.SystemMessage, "report_recarto_issue") {
		t.Errorf("issue-reports off, reminder: %q", res.SystemMessage)
	}
	if res := stop(off(features.IssueReports), 3); !res.Block || strings.Contains(res.Reason, "report_recarto_issue") ||
		!strings.HasSuffix(res.Reason, "(record_memory / record_bug). Or finish if there's genuinely nothing to keep.") {
		t.Errorf("issue-reports off, nudge: %q", res.Reason)
	}
	if res := stop(off(features.MemoryTools), 3); res != (Result{}) {
		t.Errorf("memory-tools off: %+v", res)
	}
}

// TestRunHookSkipsSwitchedOffHooks: a hook whose features are all off in the
// config file answers without opening a store.
func TestRunHookSkipsSwitchedOffHooks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := features.Save(p, off(features.SearchHints, features.ShellHints, features.RepeatedReads,
		features.LargeReads, features.BugWarnings, features.Guidance, features.Todos)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECARTO_CONFIG", p)
	refuse := func(Event) (*app.App, error) {
		t.Fatal("a switched-off hook opened the App")
		return nil, nil
	}
	for _, ev := range []Event{
		{Kind: KindAugment, SessionID: "s", Pattern: "Widget"},
		{Kind: KindShell, SessionID: "s", Command: "rg Widget", CWD: t.TempDir()},
		{Kind: KindPostRead, SessionID: "s", FilePath: "/repo/a.go"},
	} {
		var out bytes.Buffer
		if err := RunHook(context.Background(), refuse, fixedAdapter{ev}, ev.Kind, nil, &out); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("%+v", Result{}); out.String() != want {
			t.Errorf("%+v: %q", ev, out.String())
		}
	}
}
