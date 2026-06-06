package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// TestGraphCalleesDedupsIdenticalChains proves that when one ambiguous call site
// resolves to several indistinguishable low-confidence name candidates (here two
// `Foo` definitions reached via the same unknown receiver), the CLI collapses the
// identical rendered chains into a single line with a (×N) count instead of
// emitting N byte-identical lines an LLM must pay tokens to read.
func TestGraphCalleesDedupsIdenticalChains(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "x.cs"),
		[]byte("namespace N;\n"+
			"class A { public void Foo() {} }\n"+
			"class B { public void Foo() {} }\n"+
			"class Svc { void M(object x) { ((dynamic)x).Foo(); } }\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callees", "N.Svc.M"}); err != nil {
		t.Fatal(err)
	}
	// No two emitted lines may be byte-identical: that is pure duplication.
	seen := map[string]bool{}
	for _, ln := range bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n")) {
		if len(ln) == 0 {
			continue
		}
		if seen[string(ln)] {
			t.Errorf("duplicate trace line emitted: %q\nfull output:\n%s", ln, out.String())
		}
		seen[string(ln)] = true
	}
	// The two collapsed Foo candidates must surface as a count, not be silently dropped.
	if !bytes.Contains(out.Bytes(), []byte("×2")) {
		t.Errorf("expected a (×2) candidate count; output = %q", out.String())
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

func TestGraphAnnotated(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "Ext.cs"),
		[]byte("namespace N;\n"+
			"[ExtendObjectType(\"Query\")]\npublic class QueryExtensions { }\n"+
			"public class Plain { }\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"annotated", "ExtendObjectType"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("QueryExtensions")) {
		t.Errorf("graph annotated output = %q, want QueryExtensions", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("Plain")) {
		t.Errorf("graph annotated leaked undecorated class Plain: %q", out.String())
	}
}

// TestGraphArchBudgets proves `graph arch` caps its output on a large module set
// (parity with the MCP get_architecture tool) instead of dumping every dep edge,
// and reports the pre-cap totals so the map is still trustworthy.
func TestGraphArchBudgets(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	// 120 single-symbol modules (> the 80 default cap) via distinct top-level dirs.
	for i := 0; i < 120; i++ {
		dir := filepath.Join(wt, "m"+strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.go"),
			[]byte("package p\nfunc F"+strconv.Itoa(i)+"(){}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"arch"}); err != nil {
		t.Fatal(err)
	}
	moduleLines := bytes.Count(out.Bytes(), []byte(" files  "))
	if moduleLines > 80 {
		t.Errorf("graph arch emitted %d module lines, want <= 80 (budgeted)", moduleLines)
	}
	if !bytes.Contains(out.Bytes(), []byte("/120 modules")) {
		t.Errorf("graph arch should report pre-cap module total; output tail = %q", out.String())
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
