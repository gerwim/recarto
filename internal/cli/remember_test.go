package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRememberRecordsThenReinforces(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	var buf bytes.Buffer
	if err := Remember(ctx, a, &buf, []string{"learning",
		"--body", "use wal mode for cross process writers",
		"--files", "internal/store/store.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "recorded learning ") {
		t.Errorf("unexpected output: %q", buf.String())
	}

	// A near-duplicate of the same kind reinforces instead of inserting.
	buf.Reset()
	if err := Remember(ctx, a, &buf, []string{"learning",
		"--body", "use wal mode for cross process writer"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "reinforced learning ") {
		t.Errorf("expected reinforce, got: %q", buf.String())
	}
}

func TestRememberRejectsBadKindAndMissingBody(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := Remember(ctx, a, &bytes.Buffer{}, []string{"bogus", "--body", "x"}); err == nil {
		t.Error("expected error for invalid kind")
	}
	if err := Remember(ctx, a, &bytes.Buffer{}, []string{"learning"}); err == nil {
		t.Error("expected error for missing --body")
	}
}

// TestRememberHelpIsCleanExit ensures --help returns nil (not flag.ErrHelp) so
// the caller exits 0 rather than reporting a failure.
func TestRememberHelpIsCleanExit(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Remember(context.Background(), a, &buf, []string{"learning", "--help"}); err != nil {
		t.Errorf("--help should exit cleanly, got %v", err)
	}
}
