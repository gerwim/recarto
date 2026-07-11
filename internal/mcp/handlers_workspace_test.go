package mcp

import (
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// newWorkspaceApp is newTestApp with the cross-repo workspace store enabled.
func newWorkspaceApp(t *testing.T, branch string) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	t.Setenv("RECARTO_WORKSPACE", "acme")
	a := openTestApp(t, branch)
	if a.Workspace == nil {
		t.Fatal("workspace not opened despite RECARTO_WORKSPACE")
	}
	return a
}

func TestRecordMemoryWorkspaceScopeRequiresWorkspace(t *testing.T) {
	a := newTestApp(t, "main") // no RECARTO_WORKSPACE
	_, err := recordMemory(context.Background(), a, RecordMemoryInput{
		Kind: "preference", Body: "x", Scope: scopeWorkspace,
	})
	if err == nil || !strings.Contains(err.Error(), "RECARTO_WORKSPACE") {
		t.Fatalf("err = %v, want a workspace-not-configured error", err)
	}
}

func TestRecordMemoryRejectsUnknownScope(t *testing.T) {
	a := newTestApp(t, "main")
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{
		Kind: "preference", Body: "x", Scope: "galaxy",
	}); err == nil {
		t.Fatal("expected error for unknown scope")
	}
}

func TestWorkspaceScopeRoundTrip(t *testing.T) {
	a := newWorkspaceApp(t, "main")
	ctx := context.Background()

	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "preference", Title: "CliWrap", Body: "always use CliWrap for process execution",
		Scope: scopeWorkspace,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stored in the workspace store, not the repo store.
	if rec, _ := a.Engine.GetByID(ctx, out.ID); rec != nil {
		t.Error("workspace-scoped memory must not land in the repo store")
	}
	if rec, _ := a.Workspace.Engine.GetByID(ctx, out.ID); rec == nil {
		t.Fatal("workspace-scoped memory missing from the workspace store")
	}
	// Workspace rows are branch-less by construction.
	if rec, _ := a.Workspace.Engine.GetByID(ctx, out.ID); rec.Branch != "" {
		t.Errorf("workspace memory branch = %q, want branch-less", rec.Branch)
	}

	// get_memory falls back across stores by id and labels the scope.
	got, err := getMemory(ctx, a, GetMemoryInput{ID: out.ID})
	if err != nil || !got.Found {
		t.Fatalf("getMemory = (%+v, %v), want found", got, err)
	}
	if got.Memory.Scope != scopeWorkspace {
		t.Errorf("Scope = %q, want %q", got.Memory.Scope, scopeWorkspace)
	}

	// recall merges both stores and labels workspace hits.
	rec, err := recallMemory(ctx, a, RecallMemoryInput{TaskHint: "CliWrap process execution"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range rec.Memories {
		if m.ID == out.ID {
			found = true
			if m.Scope != scopeWorkspace {
				t.Errorf("recalled Scope = %q, want %q", m.Scope, scopeWorkspace)
			}
		}
	}
	if !found {
		t.Fatalf("workspace memory absent from merged recall: %+v", rec.Memories)
	}

	// list merges both stores too.
	lst, err := listMemory(ctx, a, ListMemoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, m := range lst.Memories {
		if m.ID == out.ID && m.Scope == scopeWorkspace {
			found = true
		}
	}
	if !found {
		t.Fatalf("workspace memory absent from merged list: %+v", lst.Memories)
	}

	// forget falls back across stores by id.
	fout, err := forgetMemory(ctx, a, ForgetMemoryInput{ID: out.ID})
	if err != nil || !fout.Forgotten {
		t.Fatalf("forgetMemory = (%+v, %v), want forgotten", fout, err)
	}
	if rec, _ := a.Workspace.Engine.GetByID(ctx, out.ID); rec != nil {
		t.Error("memory still present after workspace-fallback forget")
	}
}

func TestCompleteTodoFallsBackToWorkspace(t *testing.T) {
	a := newWorkspaceApp(t, "main")
	ctx := context.Background()
	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "todo", Title: "wire the dashboards", Body: "wire the dashboards across repos",
		Scope: scopeWorkspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := completeTodo(ctx, a, CompleteTodoInput{ID: out.ID})
	if err != nil || !done.Completed {
		t.Fatalf("completeTodo = (%+v, %v), want completed via workspace fallback", done, err)
	}
}

func TestRepoScopeUntouchedByWorkspace(t *testing.T) {
	a := newWorkspaceApp(t, "main")
	ctx := context.Background()
	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning", Body: "repo-scoped stays in the repo store",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := a.Engine.GetByID(ctx, out.ID); rec == nil {
		t.Error("default-scope memory missing from the repo store")
	}
	if rec, _ := a.Workspace.Engine.GetByID(ctx, out.ID); rec != nil {
		t.Error("default-scope memory must not land in the workspace store")
	}
}
