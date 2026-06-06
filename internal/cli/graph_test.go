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

func TestGraphCalleesAnnotated(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "Svc.cs"),
		[]byte("namespace N;\nclass Dep { public void Run() {} }\n"+
			"class Svc { private readonly Dep _dep; void M() { _dep.Run(); } }"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callees", "N.Svc.M"}); err != nil {
		t.Fatal(err)
	}
	// The Run callee should render with its resolution + confidence annotation.
	if !bytes.Contains(out.Bytes(), []byte("Run")) ||
		!bytes.Contains(out.Bytes(), []byte("typed")) ||
		!bytes.Contains(out.Bytes(), []byte("high")) {
		t.Errorf("graph callees output = %q, want Run [typed·high]", out.String())
	}
}

func TestGraphImplementations(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "Greet.cs"),
		[]byte("namespace N;\ninterface IGreeter { string Greet(); }\n"+
			"class EnglishGreeter : IGreeter { public string Greet() => \"hi\"; }\n"+
			"class FrenchGreeter : IGreeter { public string Greet() => \"salut\"; }\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"impls", "IGreeter"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("EnglishGreeter")) ||
		!bytes.Contains(out.Bytes(), []byte("FrenchGreeter")) {
		t.Errorf("graph impls output = %q, want both implementers", out.String())
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
