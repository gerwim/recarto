package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUnknownCommandErrors(t *testing.T) {
	// Unknown commands must error without ever opening a store.
	if err := Run([]string{"frobnicate"}, io.Discard); err == nil {
		t.Error("expected error for unknown command")
	}
}

func TestRunNoArgsShowsUsage(t *testing.T) {
	var buf bytes.Buffer
	if err := Run(nil, &buf); err != nil {
		t.Errorf("no-args Run should print usage and return nil, got %v", err)
	}
	if !strings.Contains(buf.String(), "recarto <command>") {
		t.Errorf("expected usage banner, got: %q", buf.String())
	}
}

func TestRunHelpListsNewCommands(t *testing.T) {
	var buf bytes.Buffer
	if err := Run([]string{"help"}, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"init", "serve", "hook", "licenses"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRunLicensesWorksOutsideARepo(t *testing.T) {
	t.Chdir(t.TempDir())
	var buf bytes.Buffer
	if err := Run([]string{"licenses"}, &buf); err != nil {
		t.Fatalf("licenses failed outside a repo: %v", err)
	}
	if !strings.Contains(buf.String(), "Third-party notices") {
		t.Error("licenses output has no third-party notices")
	}
}

func TestRunInitNeedsNoStore(t *testing.T) {
	// init must work in a non-git temp dir and must not require a resolvable repo.
	dir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Chdir(dir) // Go 1.24+: scopes os.Getwd to dir for this test
	if err := Run([]string{"init"}, io.Discard); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf("init did not write .mcp.json: %v", err)
	}
}
