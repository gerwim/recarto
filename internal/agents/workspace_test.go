package agents

import (
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// newWorkspaceTestApp is newTestApp with the cross-repo workspace store enabled.
func newWorkspaceTestApp(t *testing.T, branch string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_WORKSPACE", "acme")
	a := newTestApp(t, branch)
	if a.Workspace == nil {
		t.Fatal("workspace not opened despite RECARTO_WORKSPACE")
	}
	return a
}

// Session start renders a workspace tier when the workspace store holds
// memories, naming the workspace so the agent knows the entries span repos.
func TestSessionStartInjectsWorkspaceTier(t *testing.T) {
	a := newWorkspaceTestApp(t, "main")
	ctx := context.Background()
	if _, _, _, err := a.Workspace.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindPreference, Title: "CliWrap everywhere",
		Body: "always use CliWrap for process execution", Source: "mcp",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Handle(ctx, a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Workspace memory (acme") {
		t.Fatalf("session-start block missing workspace tier:\n%s", res.Context)
	}
	if !strings.Contains(res.Context, "CliWrap everywhere") {
		t.Fatalf("workspace memory not rendered:\n%s", res.Context)
	}
}

// Without a workspace the block must not mention one (and session start still works).
func TestSessionStartOmitsWorkspaceTierWhenUnconfigured(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Context, "Workspace memory") {
		t.Fatalf("unexpected workspace tier:\n%s", res.Context)
	}
}

// A session that recorded only workspace-scoped memories recorded something:
// the stop hook's capture nudge must not fire on it.
func TestHandleStopCountsWorkspaceRecords(t *testing.T) {
	a := newWorkspaceTestApp(t, "main")
	ctx := context.Background()
	if _, err := Handle(ctx, a, Event{Kind: KindSessionStart, Agent: "claude-code", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	for range stopNudgeEditThreshold {
		if _, err := Handle(ctx, a, Event{Kind: KindPostEdit, SessionID: "s1", FilePath: "/repo/main.go"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := a.Workspace.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "learned", Body: "a workspace-scoped learning", Source: "mcp",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Handle(ctx, a, Event{Kind: KindStop, Agent: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Block {
		t.Fatalf("stop nudge fired despite a workspace record: %+v", res)
	}
}
