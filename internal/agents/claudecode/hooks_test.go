package claudecode

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// fakeGit + newTestApp duplicated per test package (no shared testutil yet).
type fakeGit struct{ branch string }

func (f fakeGit) Run(args ...string) (string, error) {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir"):
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return "/repo", nil
	case args[0] == "symbolic-ref":
		return f.branch, nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: branch},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestSessionStartInjectsRecallAndOpensSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:  memory.KindDoNotRepeat,
		Title: "no redundant toolchain line",
		Body:  "go.mod must not carry a toolchain directive equal to the go line",
	}); err != nil {
		t.Fatal(err)
	}

	in := strings.NewReader(`{"session_id":"sess-1","source":"startup","hook_event_name":"SessionStart"}`)
	var out bytes.Buffer
	if err := SessionStart(ctx, a, in, &out); err != nil {
		t.Fatal(err)
	}

	s := out.String()
	if !strings.Contains(s, "additionalContext") {
		t.Errorf("session-start output is not a hook payload:\n%s", s)
	}
	if !strings.Contains(s, "no redundant toolchain line") {
		t.Errorf("session-start did not inject the pinned memory:\n%s", s)
	}
	if !strings.Contains(s, "record_memory") {
		t.Errorf("session-start missing the record nudge:\n%s", s)
	}

	var started int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT started_at FROM sessions WHERE id='sess-1'`).Scan(&started); err != nil {
		t.Fatalf("session row not created: %v", err)
	}
	// The Claude Code adapter must stamp the agent name.
	var agent string
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT agent FROM sessions WHERE id='sess-1'`).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if agent != "claude-code" {
		t.Errorf("sessions.agent = %q, want claude-code", agent)
	}
}

func TestPostReadWarnsOnRepeat(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	payload := `{"session_id":"sess-1","tool_name":"Read","tool_input":{"file_path":"internal/store/store.go"},"tool_result":"package store // ..."}`

	var first bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.String(), "already read") {
		t.Errorf("first read should not warn:\n%s", first.String())
	}

	var second bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.String(), "already read") {
		t.Errorf("second read should warn:\n%s", second.String())
	}

	n, _ := a.Ledger.Get(ctx, "lifetime", "repeated_reads_blocked")
	if n != 1 {
		t.Errorf("repeated_reads_blocked = %d, want 1", n)
	}
}

func TestSessionStartRunsCompaction(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var out bytes.Buffer
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"s1","source":"startup"}`), &out); err != nil {
		t.Fatal(err)
	}
	epoch, err := a.Engine.Epoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 2 { // fresh epoch 1 + one gated pass on session start
		t.Errorf("epoch after SessionStart = %d, want 2 (compaction did not run)", epoch)
	}
}

func TestStopFinalizesSession(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"sess-1","source":"startup"}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"sess-1","stop_hook_active":false}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "record_memory") {
		t.Errorf("stop reminder missing:\n%s", out.String())
	}

	var ended *int64
	if err := a.Store.DB.QueryRowContext(ctx,
		`SELECT ended_at FROM sessions WHERE id='sess-1'`).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if ended == nil {
		t.Error("ended_at not set on stop")
	}
}

func TestSessionStartInjectsCerebrumAndRecentBugs(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindDecision, Title: "Use SQLite", Body: "chose sqlite with wal", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "Store outside tree", Body: "db lives outside the worktree", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{Kind: memory.KindBug, Title: "panic in handler", Body: "panic in handler — fix: guard", Source: "cli", Extra: map[string]any{"fix": "guard"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := SessionStart(ctx, a, strings.NewReader(`{"session_id":"s1","source":"startup"}`), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"Use SQLite", "Store outside tree", "panic in handler", "record_memory"} {
		if !strings.Contains(s, want) {
			t.Errorf("session-start output missing %q:\n%s", want, s)
		}
	}
}

func TestPostReadSurfacesBugForFile(t *testing.T) {
	a := newTestApp(t, "main") // worktree "/repo"
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "nil deref here", Body: "nil deref — fix: guard",
		Refs:   []memory.Ref{{Type: memory.RefFile, Value: "internal/store/store.go"}},
		Source: "cli", Extra: map[string]any{"fix": "guard"},
	}); err != nil {
		t.Fatal(err)
	}
	// Claude reports an ABSOLUTE path; normalization must still match the rel ref.
	payload := `{"session_id":"s1","tool_input":{"file_path":"/repo/internal/store/store.go"},"tool_result":"package store"}`
	var out bytes.Buffer
	if err := PostRead(ctx, a, strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "prior bug") || !strings.Contains(s, "nil deref here") {
		t.Errorf("post-read should surface the prior bug:\n%s", s)
	}
}

func TestAdapterEncodeSessionStart(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindSessionStart, &sb, agents.Result{Context: "hello"}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"hookSpecificOutput"`) ||
		!strings.Contains(out, `"additionalContext"`) ||
		!strings.Contains(out, "hello") {
		t.Errorf("unexpected CC envelope:\n%s", out)
	}
}

func TestAdapterDecodePostEditFilePath(t *testing.T) {
	in := strings.NewReader(`{"session_id":"s","tool_input":{"file_path":"/x/y.go"}}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/x/y.go" || ev.Agent != "claude-code" {
		t.Errorf("decoded %+v", ev)
	}
}

func TestStopEncodesBlockOnce(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, "main")
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"e1","hook_event_name":"SessionStart","source":"startup"}`)
	if err := SessionStart(ctx, a, in, &out); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = a.Ledger.Incr(ctx, "e1", "edits", 1)
	}
	out.Reset()
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"e1"}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"decision":"block"`) || !strings.Contains(out.String(), "recorded nothing") {
		t.Fatalf("first gated stop must emit decision:block, got %q", out.String())
	}
	out.Reset()
	if err := Stop(ctx, a, strings.NewReader(`{"session_id":"e1","stop_hook_active":true}`), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "block") {
		t.Fatalf("stop_hook_active follow-up must not block, got %q", out.String())
	}
}
