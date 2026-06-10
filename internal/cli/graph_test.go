package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// writeRepoFile writes content to a file at wt/rel, creating it with 0o644.
func writeRepoFile(t *testing.T, wt, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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

// TestGraphQuerySpaceFormFlags proves that space-separated flags (--kind class) —
// a very common LLM habit — are honoured exactly like the documented --kind=class
// form. Previously they were silently swallowed as positionals, so the filter AND
// the limit were ignored and the agent got a flood of unfiltered, unranked results
// (in a real repo, generated-code noise) that pushed it to shell grep.
func TestGraphQuerySpaceFormFlags(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\ntype Authenticate struct{}\nfunc AuthenticateUser(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	// Space form must filter to the type, excluding the function — identical to the
	// --kind=type form.
	var space, eq bytes.Buffer
	if err := Graph(context.Background(), a, &space, []string{"query", "Authenticate", "--kind", "type"}); err != nil {
		t.Fatal(err)
	}
	if err := Graph(context.Background(), a, &eq, []string{"query", "Authenticate", "--kind=type"}); err != nil {
		t.Fatal(err)
	}
	if space.String() != eq.String() {
		t.Errorf("space-form flags must match = form:\n space: %q\n eq:    %q", space.String(), eq.String())
	}
	if !bytes.Contains(space.Bytes(), []byte("Authenticate")) || bytes.Contains(space.Bytes(), []byte("AuthenticateUser")) {
		t.Errorf("--kind type must keep the type and exclude the function, got %q", space.String())
	}
}

// TestGraphQueryEmptyErrors proves `graph query` with no name and no filter is
// rejected with an error instead of dumping a capped page of the whole graph
// (on a real repo: vendored/generated symbols that sort first). The dump cost
// ~50 lines of noise returned as a success — exactly the kind of misleading,
// token-wasting output that pushes an agent back to shell grep.
func TestGraphQueryEmptyErrors(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Graph(context.Background(), a, &out, []string{"query"})
	if err == nil {
		t.Fatalf("graph query with no filter should error, got output %q", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("graph query with no filter should print nothing, got %q", out.String())
	}
}

// TestGraphSearchEmptyErrors proves `graph search` with no terms and no --symbol
// is rejected with a usage error rather than the misleading "(no matches)",
// which implied the repository contained nothing for the (absent) query.
func TestGraphSearchEmptyErrors(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Graph(context.Background(), a, &out, []string{"search"})
	if err == nil {
		t.Fatalf("graph search with no terms should error, got output %q", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("no matches")) {
		t.Errorf("graph search with no terms should not print '(no matches)', got %q", out.String())
	}
}

// TestGraphGrepCoversUnindexedFiles proves `graph grep` searches files the code
// graph never parses (config/build/docs/data), so an agent grepping a dependency
// in a .csproj or a key in a .json stays in recarto instead of shelling out. The
// unindexed hit reads as "[file scope]"; the indexed one keeps its symbol tag.
func TestGraphGrepCoversUnindexedFiles(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Use(){ _ = \"DepMarker\" }\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "proj.csproj"), []byte("<Project>\n  <PackageReference Include=\"DepMarker\"/>\n</Project>\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"grep", "DepMarker"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !bytes.Contains(out.Bytes(), []byte("proj.csproj")) {
		t.Errorf("grep should find the unindexed .csproj match, got %q", s)
	}
	if !bytes.Contains(out.Bytes(), []byte("[file scope]")) {
		t.Errorf("unindexed match should render as [file scope], got %q", s)
	}
	if !bytes.Contains(out.Bytes(), []byte("a.go")) {
		t.Errorf("grep should still find the indexed match, got %q", s)
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

// TestGraphCallersReadInCallsDirection proves `graph callers` renders each chain
// so the arrow uniformly means "calls" (left calls right): the queried symbol is
// the RIGHTMOST node and its outer callers are to the left. Without this, callers
// and callees emit the same `A → B` glyph with opposite meaning, and an LLM reads
// the call direction exactly backwards — worse than grep, which is unambiguous.
func TestGraphCallersReadInCallsDirection(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "App.cs"),
		[]byte("namespace N;\n"+
			"class App {\n"+
			"  void Outer() { Middle(); }\n"+
			"  void Middle() { Target(); }\n"+
			"  void Target() {}\n"+
			"}\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callers", "N.App.Target"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// A legend must state the reading direction so a quoted line is never ambiguous.
	if !bytes.Contains(out.Bytes(), []byte("calls")) {
		t.Errorf("callers output lacks a direction legend mentioning 'calls'; got:\n%s", s)
	}
	// The queried symbol Target must appear to the RIGHT of its caller Middle, which
	// is to the right of the outer caller Outer: Outer → Middle → Target.
	iOuter := bytes.Index(out.Bytes(), []byte("Outer"))
	iMiddle := bytes.Index(out.Bytes(), []byte("Middle"))
	iTarget := bytes.LastIndex(out.Bytes(), []byte("Target"))
	if iOuter < 0 || iMiddle < 0 || iTarget < 0 {
		t.Fatalf("expected Outer→Middle→Target chain, got:\n%s", s)
	}
	if !(iOuter < iMiddle && iMiddle < iTarget) {
		t.Errorf("callers chain must read outer→inner→target (calls direction); got order Outer@%d Middle@%d Target@%d:\n%s", iOuter, iMiddle, iTarget, s)
	}
}

// TestGraphCalleesReadInCallsDirection is the dual: callees already reads in the
// "calls" direction (queried symbol leftmost), and must stay that way.
func TestGraphCalleesReadInCallsDirection(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "App.cs"),
		[]byte("namespace N;\n"+
			"class App {\n"+
			"  void Outer() { Middle(); }\n"+
			"  void Middle() { Target(); }\n"+
			"  void Target() {}\n"+
			"}\n"), 0o644)
	if err := Index(context.Background(), a, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Graph(context.Background(), a, &out, []string{"callees", "N.App.Outer"}); err != nil {
		t.Fatal(err)
	}
	iOuter := bytes.Index(out.Bytes(), []byte("Outer"))
	iMiddle := bytes.Index(out.Bytes(), []byte("Middle"))
	if iOuter < 0 || iMiddle < 0 || !(iOuter < iMiddle) {
		t.Errorf("callees must read queried→callee (Outer before Middle); got:\n%s", out.String())
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
	// A trailing --limit flag must be honoured as a flag, not folded into the type
	// name (which silently corrupted it to "IGreeter --limit=5" → no matches and a
	// fallback to grep). The implementers must still appear.
	var flagged bytes.Buffer
	if err := Graph(context.Background(), a, &flagged, []string{"impls", "IGreeter", "--limit", "5"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(flagged.Bytes(), []byte("EnglishGreeter")) ||
		!bytes.Contains(flagged.Bytes(), []byte("FrenchGreeter")) {
		t.Errorf("impls with a trailing --limit flag must still find implementers, got %q", flagged.String())
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
// to impls/refs instead of emitting a bare, misleading single line.
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
	if !bytes.Contains(out.Bytes(), []byte("impls")) || !bytes.Contains(out.Bytes(), []byte("refs")) {
		t.Errorf("callers of a type did not steer to impls/refs; output = %q", out.String())
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

func TestGraphRefs(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	writeRepoFile(t, wt, "b.go", "package p\nfunc Caller() { Target() }\n")
	writeRepoFile(t, wt, "a_test.go", "package p\nfunc helper() { Target() }\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"refs", "Target"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "b.go:2") || !strings.Contains(out, "calls") {
		t.Errorf("output missing the calls reference from b.go:2:\n%s", out)
	}
	if !strings.Contains(out, "[test]") {
		t.Errorf("output missing the [test] marker for a_test.go:\n%s", out)
	}
}

func TestGraphRefsUnknownSymbol(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Only() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"refs", "NoSuchSym"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "did not resolve") {
		t.Errorf("unknown symbol must say it did not resolve to a graph node:\n%s", buf.String())
	}
}

func TestGraphGrepOffset(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "t.go", "package p\n// needle one\n// needle two\nfunc F() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"grep", "needle", "--offset=1", "--limit=1"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "t.go:3") || strings.Contains(out, "t.go:2") {
		t.Errorf("offset=1 must return only the second match (t.go:3):\n%s", out)
	}
}

func TestGraphSnippetPrintsPathHeader(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"snippet", "Target"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "// a.go:2-2\n") {
		t.Errorf("snippet must lead with its // path:start-end header:\n%s", buf.String())
	}
}

func TestGraphCallersMarksTestCallers(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	writeRepoFile(t, wt, "a.go", "package p\nfunc Target() {}\n")
	writeRepoFile(t, wt, "a_test.go", "package p\nfunc helperT() { Target() }\n")
	if err := a.Graph.Reconcile(context.Background(), wt, graph.FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Graph(context.Background(), a, &buf, []string{"callers", "Target"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "helperT [test]") && !strings.Contains(out, "helperT [test·") {
		// label order: name, then [test], then [resolution·confidence] — assert leniently on "helperT" + "[test]"
		if !(strings.Contains(out, "helperT") && strings.Contains(out, "[test]")) {
			t.Errorf("test caller must carry a [test] marker:\n%s", out)
		}
	}
	if !strings.Contains(out, "every caller shown is test code") {
		t.Errorf("all-test callers must print the steer note:\n%s", out)
	}
}
