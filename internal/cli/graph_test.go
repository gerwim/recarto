package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGraphSearch(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"search", "Authenticate"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Authenticate")) {
		t.Errorf("graph search output = %q", out.String())
	}
}

func TestGraphRequiresSubcommand(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Graph(context.Background(), a, &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no subcommand given")
	}
}

// TestGraphSearchAutoReconciles proves the spec §11 query-time freshness: a graph
// query reflects the current checkout WITHOUT a prior explicit `recarto index`.
func TestGraphSearchAutoReconciles(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "z.go"),
		[]byte("package p\nfunc Zeta(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// No explicit Index call — Graph must lazily reconcile first.
	if err := Graph(context.Background(), a, &out, []string{"search", "Zeta"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Zeta")) {
		t.Errorf("graph search did not auto-reconcile; output = %q", out.String())
	}
}

func TestGraphSearchSymbolFlag(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.cs"),
		[]byte("namespace App;\nclass PdokAddressLookupService {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Concept words + a --symbol candidate that bridges to the target name.
	if err := Graph(context.Background(), a, &out,
		[]string{"search", "--symbol=AddressLookup", "convert", "street"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("PdokAddressLookupService")) {
		t.Errorf("graph search --symbol output = %q", out.String())
	}
}
