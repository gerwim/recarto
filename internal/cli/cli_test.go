package cli

import (
	"bytes"
	"io"
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
