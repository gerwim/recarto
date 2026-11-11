package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/update"
)

func testManifest() *update.Manifest {
	return &update.Manifest{Schema: 1, Package: "recarto", Latest: "1.4.0", Versions: []update.Release{
		{Version: "1.4.0", Artifacts: map[string]update.Artifact{"linux-amd64": {Path: "p140"}}},
		{Version: "1.3.0", Artifacts: map[string]update.Artifact{"linux-amd64": {Path: "p130"}}},
	}}
}

func TestSelectTargetExplicitVersion(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "1.3.0", false, strings.NewReader(""), &out)
	if err != nil || !proceed || v != "1.3.0" {
		t.Fatalf("explicit version: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetExplicitUnknownVersion(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := selectTarget(testManifest(), "1.3.0", "9.9.9", false, strings.NewReader(""), &out); err == nil {
		t.Fatal("unknown explicit version must error")
	}
}

func TestSelectTargetNonTTYPicksLatest(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "", false, strings.NewReader(""), &out)
	if err != nil || !proceed || v != "1.4.0" {
		t.Fatalf("non-tty latest: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYEnterPicksLatest(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "", true, strings.NewReader("\n"), &out)
	if err != nil || !proceed || v != "1.4.0" {
		t.Fatalf("tty enter: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYPicksOlder(t *testing.T) {
	var out bytes.Buffer
	// menu is numbered newest-first: 1) 1.4.0  2) 1.3.0
	v, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader("2\n"), &out)
	if err != nil || !proceed || v != "1.3.0" {
		t.Fatalf("tty pick older: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYQuitDoesNotProceed(t *testing.T) {
	var out bytes.Buffer
	_, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader("q\n"), &out)
	if err != nil || proceed {
		t.Fatalf("quit should not proceed: proceed=%v err=%v", proceed, err)
	}
}

func TestSelectTargetTTYOutOfRangeErrors(t *testing.T) {
	var out bytes.Buffer
	for _, in := range []string{"0\n", "99\n", "abc\n"} {
		if _, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader(in), &out); err == nil || proceed {
			t.Fatalf("input %q: expected error and no proceed, got proceed=%v err=%v", in, proceed, err)
		}
	}
}

func TestSelectTargetTTYEOFCancels(t *testing.T) {
	var out bytes.Buffer
	// EOF with no input (Ctrl-D / closed pipe) must cancel, not silently install.
	_, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader(""), &out)
	if err != nil || proceed {
		t.Fatalf("EOF should cancel without error: proceed=%v err=%v", proceed, err)
	}
}
