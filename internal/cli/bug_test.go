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

func TestBugAddAcceptsSymbols(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := Bug(ctx, a, &bytes.Buffer{}, []string{"add",
		"--error", "panic in Tokenize",
		"--fix", "guard the slice",
		"--symbols", "Tokenize,Lexer",
	}); err != nil {
		t.Fatal(err)
	}
	// The bug is now retrievable by one of its symbol refs.
	bugs, err := a.Engine.BugsForCode(ctx, "main", nil, []string{"Tokenize"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bugs) != 1 || bugs[0].Title != "panic in Tokenize" {
		t.Fatalf("want 1 bug by symbol Tokenize, got %d: %+v", len(bugs), bugs)
	}
}
