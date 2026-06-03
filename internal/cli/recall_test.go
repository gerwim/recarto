package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestRecallByFileRef(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindLearning,
		Title:  "WAL setup",
		Body:   "use wal mode and busy_timeout for cross process writers",
		Refs:   []memory.Ref{{Type: memory.RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Recall(ctx, a, &buf, []string{"--files", "internal/store/store.go"}); err != nil {
		t.Fatal(err)
	}
	// Assert the full result line, pinning the bullet + kind-bracket format.
	if !strings.Contains(buf.String(), "• [learning] WAL setup") {
		t.Errorf("recall missing/!matching structural result line:\n%s", buf.String())
	}
}

func TestRecallEmpty(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Recall(context.Background(), a, &buf, []string{"--files", "nope/missing.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no memories recalled") {
		t.Errorf("expected empty-recall notice, got: %q", buf.String())
	}
}
