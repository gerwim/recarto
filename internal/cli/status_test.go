package cli

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestStatusReportsIdentityCountsAndLedger(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Body: "use wal mode for cross process writers"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Status(ctx, a, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"branch:   main", "learning", "total", "memories_recorded", "repeated_augments_blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	// The recorded memory must be reflected in the printed counter value, not
	// just the label (guards against Record ceasing to bump the ledger).
	if !regexp.MustCompile(`memories_recorded\s+[1-9]`).MatchString(out) {
		t.Errorf("memories_recorded counter not > 0 in:\n%s", out)
	}
}

func TestStatusShowsCompactionState(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Status(ctx, a, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"epoch", "archived", "live tokens", "compactions"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("status missing %q:\n%s", want, buf.String())
		}
	}
}
