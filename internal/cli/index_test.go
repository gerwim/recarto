package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexReportsCounts(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc A(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Index(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("indexed")) {
		t.Errorf("output = %q, want an 'indexed' summary", out.String())
	}
}
