package agents

import (
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestHandleSessionStartEmptyRepo(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindSessionStart, Agent: "opencode", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Context, "Recarto memory for this repo/branch") {
		t.Errorf("missing session context header:\n%s", res.Context)
	}
	if !strings.Contains(res.Context, "record_memory") {
		t.Errorf("missing record nudge:\n%s", res.Context)
	}
	// The session row must carry the adapter-supplied agent name.
	var agent string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT agent FROM sessions WHERE id='s1'`).Scan(&agent); err != nil {
		t.Fatalf("session row not created: %v", err)
	}
	if agent != "opencode" {
		t.Errorf("sessions.agent = %q, want opencode", agent)
	}
}

func TestHandleStopNudge(t *testing.T) {
	a := newTestApp(t, "main")
	res, err := Handle(context.Background(), a, Event{Kind: KindStop, Agent: "copilot", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "record any new learnings") {
		t.Errorf("missing stop nudge: %q", res.SystemMessage)
	}
}

func TestHandlePostReadWarnsOnRepeat(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	ev := Event{Kind: KindPostRead, Agent: "claude-code", SessionID: "s1", FilePath: "internal/store/store.go", ToolResult: "package store"}

	first, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Context, "already read") {
		t.Errorf("first read should not warn:\n%s", first.Context)
	}
	second, err := Handle(ctx, a, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Context, "already read") {
		t.Errorf("second read should warn:\n%s", second.Context)
	}
}

func TestSessionBudgetEnvOverride(t *testing.T) {
	t.Setenv("RECARTO_SESSION_BUDGET", "50")
	if got := sessionBudget(); got != 50 {
		t.Errorf("sessionBudget() = %d, want 50", got)
	}
	t.Setenv("RECARTO_SESSION_BUDGET", "")
	if got := sessionBudget(); got != sessionStartBudget {
		t.Errorf("sessionBudget() default = %d, want %d", got, sessionStartBudget)
	}
}

func TestFormatSessionContextDegradesToTitles(t *testing.T) {
	results := []memory.RecallResult{
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "AAA", Body: "short body"}},
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "BBB", Body: strings.Repeat("x", 2000)}},
	}
	s := formatSessionContext(results, graph.Architecture{}, nil, 60)
	if !strings.Contains(s, "AAA") || !strings.Contains(s, "short body") {
		t.Errorf("first entry should render with its body:\n%s", s)
	}
	if !strings.Contains(s, "BBB") {
		t.Errorf("second entry title should still appear:\n%s", s)
	}
	if strings.Contains(s, strings.Repeat("x", 100)) {
		t.Errorf("second entry body should be dropped under tight budget:\n%s", s)
	}
}

func TestFormatSessionContextDropsEntryUnderTinyBudget(t *testing.T) {
	results := []memory.RecallResult{
		{Record: &memory.Record{Kind: memory.KindLearning, Title: "WontFitAtAll", Body: "body"}},
	}
	s := formatSessionContext(results, graph.Architecture{}, nil, 2)
	if strings.Contains(s, "WontFitAtAll") {
		t.Errorf("entry should be dropped entirely when even its title exceeds the budget:\n%s", s)
	}
	if strings.Contains(s, "no prior memories yet") {
		t.Errorf("results were non-empty; must not claim none exist:\n%s", s)
	}
}
