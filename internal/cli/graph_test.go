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

// TestGraphQueryBareNameMatchesName proves a bare positional to `graph query` is
// treated as the symbol name (substring on name/qualified-name) — what the help
// documents. Previously the first arg was read as a KIND filter, so querying a
// real symbol name returned nothing and bounced the agent to shell grep.
func TestGraphQueryBareNameMatchesName(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"query", "Authenticate"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Authenticate")) {
		t.Errorf("graph query <name> should find the symbol, got %q", out.String())
	}
	// The --kind flag still constrains: a non-matching kind yields nothing.
	out.Reset()
	if err := Graph(context.Background(), a, &out, []string{"query", "Authenticate", "--kind=class"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("Authenticate")) {
		t.Errorf("graph query --kind=class should exclude the function, got %q", out.String())
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

// TestGraphOutline proves `graph outline <file>` lists a file's symbols (kind,
// name, line) in source order so an agent can grasp structure without reading the
// whole file, and that it scopes strictly to the requested file.
func TestGraphOutline(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "Svc.cs"),
		[]byte("namespace N;\n"+
			"public class Svc {\n"+
			"  public void Alpha() {}\n"+
			"  public int Beta(string s) => 0;\n"+
			"}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "Other.cs"),
		[]byte("namespace N;\npublic class Other { public void Gamma() {} }\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"outline", "Svc.cs"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Alpha")) || !bytes.Contains(out.Bytes(), []byte("Beta")) {
		t.Errorf("graph outline output = %q, want Alpha and Beta", out.String())
	}
	// Must not leak symbols from a different file.
	if bytes.Contains(out.Bytes(), []byte("Gamma")) {
		t.Errorf("graph outline leaked Other.cs symbol Gamma: %q", out.String())
	}
	// Each line should carry a line number (path-less, since the file is fixed).
	if !bytes.Contains(out.Bytes(), []byte(":3")) {
		t.Errorf("graph outline should show line numbers; output = %q", out.String())
	}
}

// TestGraphSnippetBySymbol proves `graph snippet <symbol>` resolves the file
// itself (the common case right after a search), so the agent need not already
// know the path.
func TestGraphSnippetBySymbol(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\n\nfunc Uniq() int {\n\treturn 7\n}\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"snippet", "Uniq"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("func Uniq() int")) {
		t.Errorf("graph snippet by symbol = %q, want the Uniq body", out.String())
	}
}

// TestGraphSnippetBySymbolAmbiguous proves a bare name with several definitions
// lists the candidates (with file:line and qualified name) instead of silently
// returning one or nothing — keeping the agent inside recarto to disambiguate.
func TestGraphSnippetBySymbolAmbiguous(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package a\n\nfunc Dup() {}\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "b.go"), []byte("package b\n\nfunc Dup() {}\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"snippet", "Dup"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("a.go")) || !bytes.Contains(out.Bytes(), []byte("b.go")) {
		t.Errorf("ambiguous snippet did not list both candidate files; output = %q", out.String())
	}
}

// TestGraphHelpListsGrepFlags proves the subcommand flags are discoverable from
// the CLI itself — an agent cannot read the source, so `graph help` must document
// the grep search modifiers (regex, ignore-case, glob) it would otherwise miss.
func TestGraphHelpListsGrepFlags(t *testing.T) {
	a := newTestApp(t, "main")
	for _, args := range [][]string{{"help"}, {"grep", "--help"}} {
		var out bytes.Buffer
		if err := Graph(context.Background(), a, &out, args); err != nil {
			t.Fatalf("graph %v: %v", args, err)
		}
		s := out.String()
		for _, want := range []string{"--regex", "--ignore-case", "--glob"} {
			if !bytes.Contains(out.Bytes(), []byte(want)) {
				t.Errorf("graph %v help missing %q; output = %q", args, want, s)
			}
		}
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

// TestGraphGrepReportsTruncation proves `graph grep` tells the agent when its
// page is partial (the grep-replacement must never present a capped result as the
// whole answer) and that --limit raises the cap.
func TestGraphGrepReportsTruncation(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\n// hit\n// hit\n// hit\n// hit\n// hit\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"grep", "--limit=2", "hit"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("of 5")) {
		t.Errorf("grep did not report truncation (5 matches, 2 shown); output = %q", out.String())
	}

	// Raising the limit shows every match and drops the notice.
	out.Reset()
	if err := Graph(context.Background(), a, &out, []string{"grep", "--limit=50", "hit"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("showing")) {
		t.Errorf("grep reported truncation when all matches fit; output = %q", out.String())
	}
}

// TestGraphCallersTypeHint proves tracing a type (no call edges) steers the agent
// to impls/grep instead of emitting a bare, misleading single line.
func TestGraphCallersTypeHint(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "f.cs"),
		[]byte("namespace N;\npublic interface IFoo { void Bar(); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callers", "IFoo"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("impls")) || !bytes.Contains(out.Bytes(), []byte("grep")) {
		t.Errorf("callers of a type did not steer to impls/grep; output = %q", out.String())
	}
}

// TestGraphCallersMissShowsSuggestions proves a misspelled trace symbol surfaces
// the lenient candidates instead of printing nothing (the CLI previously
// discarded the engine's suggestions, leaving the agent with a silent miss).
func TestGraphCallersMissShowsSuggestions(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	if err := os.WriteFile(filepath.Join(wt, "a.go"),
		[]byte("package p\nfunc Greet(name string) string { return name }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callers", "Gree"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Greet")) {
		t.Errorf("missed trace did not suggest 'Greet'; output = %q", out.String())
	}
}
