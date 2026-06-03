package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestBugAddThenSearch(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	var buf bytes.Buffer
	if err := Bug(ctx, a, &buf, []string{"add",
		"--error", "nil pointer in session refresh",
		"--cause", "missing nil guard",
		"--fix", "add a nil check before deref",
		"--file", "internal/auth/session.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "recorded bug ") {
		t.Errorf("unexpected add output: %q", buf.String())
	}

	buf.Reset()
	if err := Bug(ctx, a, &buf, []string{"search", "session", "refresh"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "• [bug] nil pointer in session refresh") {
		t.Errorf("search missed the bug or dropped the kind bracket:\n%s", out)
	}
	if !strings.Contains(out, "add a nil check before deref") {
		t.Errorf("search should surface the fix:\n%s", out)
	}
}

func TestBugRequiresSubcommand(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Bug(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no subcommand given")
	}
}
