package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestForgetRemovesMemory(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	id, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "stray", Body: "recorded in error", Branch: a.Git.Branch, Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Forget(ctx, a, &out, []string{id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "forgotten") || !strings.Contains(out.String(), id) {
		t.Fatalf("forget output unexpected:\n%s", out.String())
	}

	// A second forget of the same id is a no-op and says so.
	var out2 bytes.Buffer
	if err := Forget(ctx, a, &out2, []string{id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "no memory") {
		t.Fatalf("re-forget of deleted id should report no memory:\n%s", out2.String())
	}
}

func TestForgetUnknownIDIsNoOp(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var out bytes.Buffer
	if err := Forget(ctx, a, &out, []string{"019e0000-0000-7000-0000-000000000000"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no memory") {
		t.Fatalf("unknown id should report no memory:\n%s", out.String())
	}
}

func TestForgetRequiresID(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := Forget(ctx, a, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("forget with no id should error")
	}
}
