package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDoctorPasses(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Doctor(context.Background(), a, &buf); err != nil {
		t.Fatalf("doctor returned error: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"[ok] wal", "[ok] integrity", "[ok] schema", "[ok] git"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorFailsOnUnresolvedGit exercises the FAIL path + non-nil error return:
// an App whose git identity didn't resolve (empty RepoID) must fail the git check.
func TestDoctorFailsOnUnresolvedGit(t *testing.T) {
	a := newTestApp(t, "main")
	a.Git.RepoID = "" // simulate unresolved identity

	var buf bytes.Buffer
	err := Doctor(context.Background(), a, &buf)
	if err == nil {
		t.Fatalf("expected doctor to fail, got nil\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "[FAIL] git") {
		t.Errorf("expected [FAIL] git line:\n%s", buf.String())
	}
}
