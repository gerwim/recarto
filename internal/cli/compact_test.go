package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCompactForceReports(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Compact(ctx, a, &buf, []string{"--force"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"epoch", "archived:", "deduped:", "live tokens:"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output missing %q:\n%s", want, out)
		}
	}
}

func TestCompactDryRunPrefixed(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Compact(ctx, a, &buf, []string{"--force", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[dry-run]") {
		t.Errorf("dry-run output should be prefixed:\n%s", buf.String())
	}
}
