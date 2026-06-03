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
