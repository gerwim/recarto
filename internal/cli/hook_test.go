package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHookSessionStartDispatch(t *testing.T) {
	a := newTestApp(t, "main")
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"s1","source":"startup"}`)
	if err := Hook(context.Background(), a, in, &out, []string{"session-start"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("hook session-start produced no context:\n%s", out.String())
	}
}

func TestHookUnknownEventErrors(t *testing.T) {
	a := newTestApp(t, "main")
	err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, []string{"bogus"})
	if err == nil {
		t.Error("expected error for unknown hook event")
	}
}

func TestHookRequiresEvent(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no event is given")
	}
}

func TestHookAgentSelectsCopilotPayload(t *testing.T) {
	a := newTestApp(t, "main")
	// Copilot camelCase payload: a bad (claude) parse would not stamp the session.
	in := strings.NewReader(`{"sessionId":"cop-1","source":"new"}`)
	var out bytes.Buffer
	if err := Hook(context.Background(), a, in, &out, []string{"session-start", "--agent", "copilot"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("copilot session-start should emit additionalContext:\n%s", out.String())
	}
	var agent string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT agent FROM sessions WHERE id='cop-1'`).Scan(&agent); err != nil {
		t.Fatalf("copilot session row not created (payload not parsed as camelCase): %v", err)
	}
	if agent != "copilot" {
		t.Errorf("sessions.agent = %q, want copilot", agent)
	}
}

func TestHookUnknownAgentErrors(t *testing.T) {
	a := newTestApp(t, "main")
	err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, []string{"session-start", "--agent", "vim"})
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}
