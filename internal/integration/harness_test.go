// Package integration holds the language-fixture integration suite: it indexes
// a real, compilable project per language (testdata/<lang>/src) with the actual
// CLI (`recarto index`) and verifies every graph query surface — outline, query,
// grep, search, snippet, impls, callers/callees, annotated — against
// expectations embedded in the fixture sources as structured markers.
//
// Expectations live INLINE in the fixture files (the technique Roslyn's own
// find-references tests use) so line numbers can never drift from the code:
//
//	public record GeoPoint(double Lat, double Lon); //# def record NS.GeoPoint
//	var p = new GeoPoint(1, 2); //# ref GeoPoint
//
// Marker grammar (after `<comment>#`, directives separated by `;`):
//
//	def <kind> <qualified.name>      a symbol the graph must index, declared here
//	ref <Name>                       this line is a usage of <Name> (LSP find-references semantics)
//	tok <Name>                       textual-only occurrence (grep finds it; an LSP may not count it)
//	implicit <Name>                  semantic-only reference (the LSP reports it; no token on the line)
//	impl <Sub> of <Super>            subtype/implementation relation declared here
//	call <Caller> -> <Callee>        a call edge; both sides accept Leaf or Type.Method
//	annot <Attr> on <Target>         <Target> is decorated with attribute <Attr> here
//
// Authoring discipline (mechanically enforced by the fixture-hygiene subtest):
// every whole-word occurrence of a tracked simple name in the code text (marker
// comments stripped) must be its def line or carry a ref marker — so textual
// matches and semantic usages coincide by construction, and `graph grep -r
// '\bName\b'` can be asserted EXACTLY against an independent oracle, in both
// directions (no missing hits, no phantom hits). Names mentioned only inside
// marker comments are fine: the oracle that feeds the engine comparison reads
// the same full bytes the engine does.
//
// Adding a language = adding testdata/<lang>/{manifest.json,src/...} with the
// same markers; no Go changes needed.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/cli"
	"github.com/gerwim/recarto/internal/clock"
)

// manifest configures one language fixture.
type manifest struct {
	Language    string   `json:"language"`
	Comment     string   `json:"comment"`      // line-comment opener, e.g. "//"
	SourceExts  []string `json:"source_exts"`  // extensions the indexer parses, e.g. [".cs"]
	ExcludeDirs []string `json:"exclude_dirs"` // build-artifact dirs to ignore, e.g. ["bin","obj"]
	LSPCommand  []string `json:"lsp_command"`  // language server for the LSP cross-check, e.g. ["csharp-ls"]
	// LSPKnownMisses exempts references the fixture's language server fails to
	// report (name -> "path:line" -> why). An entry that the server DOES report
	// fails the cross-check as stale, so upgrades clean the list up.
	LSPKnownMisses map[string]map[string]string `json:"lsp_known_misses"`
}

func (m manifest) excluded(dirName string) bool {
	for _, d := range m.ExcludeDirs {
		if d == dirName {
			return true
		}
	}
	return false
}

type def struct {
	Kind, QN, File string
	Line           int
}

// name is the identifier a reader would grep for: the qualified name's leaf,
// except namespaces whose declared name IS the dotted path.
func (d def) name() string {
	if d.Kind == "namespace" {
		return d.QN
	}
	if i := strings.LastIndex(d.QN, "."); i >= 0 {
		return d.QN[i+1:]
	}
	return d.QN
}

func (d def) loc() string { return fmt.Sprintf("%s:%d", d.File, d.Line) }

type ref struct {
	Name, File string
	Line       int
}

type implRel struct {
	Sub, Super, File string
	Line             int
}

type callRel struct {
	Caller, Callee, File string // Leaf or Type.Method specs
	Line                 int
}

type annotRel struct {
	Attr, Target, File string
	Line               int
}

// fixture is one language's parsed testdata: sources plus marker-derived truth.
type fixture struct {
	lang      string
	man       manifest
	root      string              // absolute path of testdata/<lang>/src
	files     []string            // every file, repo-relative slash paths
	srcFiles  []string            // the subset with a source extension
	lines     map[string][]string // file -> raw lines (no trailing newline)
	defs      []def
	refs      []ref
	toks      []ref // textual-only occurrences (grep finds them; an LSP may not count them)
	implicits []ref // semantic-only references (an LSP reports them; no token on the line, so grep cannot)
	impls     []implRel
	calls     []callRel
	annots    []annotRel
}

func (fx *fixture) markerToken() string { return fx.man.Comment + "#" }

func (fx *fixture) isSource(path string) bool {
	for _, ext := range fx.man.SourceExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// stripMarker returns the code portion of a line: everything before the
// expectation marker (the marker comment is test metadata, not fixture code).
func (fx *fixture) stripMarker(line string) string {
	if i := strings.Index(line, fx.markerToken()); i >= 0 {
		return line[:i]
	}
	return line
}

func loadFixture(t *testing.T, dir string) *fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var man manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if man.Language == "" || man.Comment == "" || len(man.SourceExts) == 0 {
		t.Fatalf("manifest %s: language, comment and source_exts are all required", dir)
	}
	root, err := filepath.Abs(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{lang: man.Language, man: man, root: root, lines: map[string][]string{}}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if man.excluded(d.Name()) {
				return filepath.SkipDir // stray local build artifacts (bin/, obj/)
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		fx.files = append(fx.files, rel)
		fx.lines[rel] = strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
		if fx.isSource(rel) {
			fx.srcFiles = append(fx.srcFiles, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixture: %v", err)
	}
	sort.Strings(fx.files)
	sort.Strings(fx.srcFiles)
	for _, f := range fx.srcFiles {
		fx.parseMarkers(t, f)
	}
	if len(fx.defs) == 0 {
		t.Fatalf("fixture %s has no def markers", dir)
	}
	return fx
}

func (fx *fixture) parseMarkers(t *testing.T, file string) {
	t.Helper()
	for i, line := range fx.lines[file] {
		idx := strings.Index(line, fx.markerToken())
		if idx < 0 {
			continue
		}
		n := i + 1
		for _, directive := range strings.Split(line[idx+len(fx.markerToken()):], ";") {
			fields := strings.Fields(directive)
			if len(fields) == 0 {
				continue
			}
			bad := func() {
				t.Fatalf("%s:%d: malformed marker directive %q", file, n, strings.TrimSpace(directive))
			}
			switch fields[0] {
			case "def":
				if len(fields) != 3 {
					bad()
				}
				fx.defs = append(fx.defs, def{Kind: fields[1], QN: fields[2], File: file, Line: n})
			case "ref":
				if len(fields) != 2 {
					bad()
				}
				fx.refs = append(fx.refs, ref{Name: fields[1], File: file, Line: n})
			case "tok":
				if len(fields) != 2 {
					bad()
				}
				fx.toks = append(fx.toks, ref{Name: fields[1], File: file, Line: n})
			case "implicit":
				if len(fields) != 2 {
					bad()
				}
				fx.implicits = append(fx.implicits, ref{Name: fields[1], File: file, Line: n})
			case "impl":
				if len(fields) != 4 || fields[2] != "of" {
					bad()
				}
				fx.impls = append(fx.impls, implRel{Sub: fields[1], Super: fields[3], File: file, Line: n})
			case "call":
				if len(fields) != 4 || fields[2] != "->" {
					bad()
				}
				fx.calls = append(fx.calls, callRel{Caller: fields[1], Callee: fields[3], File: file, Line: n})
			case "annot":
				if len(fields) != 4 || fields[2] != "on" {
					bad()
				}
				fx.annots = append(fx.annots, annotRel{Attr: fields[1], Target: fields[3], File: file, Line: n})
			default:
				bad()
			}
		}
	}
}

// wordPattern is the reference-search regex for an identifier — the same shape
// the agent-facing docs recommend (`\bName\b`, dots escaped).
func wordPattern(name string) string {
	return `\b` + regexp.QuoteMeta(name) + `\b`
}

// oracle computes, independently of the engine, the exact set of "file:line"
// locations whose text matches the word-boundary pattern for name. With
// stripped=true marker comments are removed first and only source files are
// scanned (the semantic view); otherwise the full bytes of every fixture file
// are scanned (the engine view — the engine greps the same bytes).
func (fx *fixture) oracle(name string, stripped bool) map[string]bool {
	re := regexp.MustCompile(wordPattern(name))
	hits := map[string]bool{}
	for _, f := range fx.files {
		if stripped && !fx.isSource(f) {
			continue
		}
		for i, line := range fx.lines[f] {
			if stripped {
				line = fx.stripMarker(line)
			}
			if re.MatchString(line) {
				hits[fmt.Sprintf("%s:%d", f, i+1)] = true
			}
		}
	}
	return hits
}

// trackedNames returns every identifier named by a def, ref or tok marker, sorted.
func (fx *fixture) trackedNames() []string {
	set := map[string]bool{}
	for _, d := range fx.defs {
		set[d.name()] = true
	}
	for _, r := range fx.refs {
		set[r.Name] = true
	}
	for _, r := range fx.toks {
		set[r.Name] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// markedLines returns the def∪ref marker locations for name — the SEMANTIC
// usage set (what an LSP's find-references should cover). Pass withToks=true to
// also include tok lines — the TEXTUAL set (what grep returns).
func (fx *fixture) markedLines(name string, withToks bool) map[string]bool {
	lines := map[string]bool{}
	for _, d := range fx.defs {
		if d.name() == name {
			lines[d.loc()] = true
		}
	}
	for _, r := range fx.refs {
		if r.Name == name {
			lines[fmt.Sprintf("%s:%d", r.File, r.Line)] = true
		}
	}
	if withToks {
		for _, r := range fx.toks {
			if r.Name == name {
				lines[fmt.Sprintf("%s:%d", r.File, r.Line)] = true
			}
		}
	}
	return lines
}

// implicitLines returns the locations marked as semantic-only references of
// name (e.g. an implicit base-constructor call): visible to an LSP, invisible
// to any textual search.
func (fx *fixture) implicitLines(name string) map[string]bool {
	lines := map[string]bool{}
	for _, r := range fx.implicits {
		if r.Name == name {
			lines[fmt.Sprintf("%s:%d", r.File, r.Line)] = true
		}
	}
	return lines
}

// qnMatches reports whether qualified name qn is identified by spec — equal, or
// dot-aligned suffix (spec "Type.Method" or "Leaf").
func qnMatches(qn, spec string) bool {
	return qn == spec || strings.HasSuffix(qn, "."+spec)
}

// Kind families for resolving relation specs: an impl marker names types, a
// call marker's caller names something callable. The filter keeps a leaf spec
// like "InMemoryRepository" from also matching its constructor (whose qualified
// name ends with the same segment).
var (
	typeKinds     = map[string]bool{"class": true, "interface": true, "struct": true, "record": true, "enum": true, "delegate": true, "type": true, "trait": true, "impl": true}
	callableKinds = map[string]bool{"method": true, "constructor": true, "function": true}
	anyKind       map[string]bool // nil: no kind restriction
)

// resolveDefs returns the distinct def qualified names matching spec, optionally
// restricted to a kind family.
func (fx *fixture) resolveDefs(spec string, kinds map[string]bool) []string {
	set := map[string]bool{}
	for _, d := range fx.defs {
		if kinds != nil && !kinds[d.Kind] {
			continue
		}
		if qnMatches(d.QN, spec) {
			set[d.QN] = true
		}
	}
	out := make([]string, 0, len(set))
	for qn := range set {
		out = append(out, qn)
	}
	sort.Strings(out)
	return out
}

// resolveCallee resolves a call marker's callee: something callable, or — for a
// constructor invocation spelled as the type (`new GeoPoint(...)` on a record
// with no explicit constructor) — the type definition standing in for its
// implicit constructor.
func (fx *fixture) resolveCallee(spec string) []string {
	if got := fx.resolveDefs(spec, callableKinds); len(got) > 0 {
		return got
	}
	return fx.resolveDefs(spec, typeKinds)
}

// resolveUniqueDef resolves spec to exactly one def qualified name or fails.
func (fx *fixture) resolveUniqueDef(t *testing.T, spec string, kinds map[string]bool, context string) string {
	t.Helper()
	qns := fx.resolveDefs(spec, kinds)
	if len(qns) != 1 {
		t.Fatalf("%s: spec %q resolves to %d defs %v, need exactly 1", context, spec, len(qns), qns)
	}
	return qns[0]
}

func leaf(spec string) string {
	if i := strings.LastIndex(spec, "."); i >= 0 {
		return spec[i+1:]
	}
	return spec
}

// diffSets formats the symmetric difference of want/got location sets.
func diffSets(want, got map[string]bool) string {
	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var b strings.Builder
	if len(missing) > 0 {
		fmt.Fprintf(&b, "\n  missing (expected, not returned): %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "\n  extra (returned, not expected): %s", strings.Join(extra, ", "))
	}
	return b.String()
}

// --- engine plumbing -------------------------------------------------------

// fakeGit pins the repo identity so the store key is stable; the worktree the
// engine walks is set explicitly on the App (mirrors internal/cli tests).
type fakeGit struct{}

func (fakeGit) Run(args ...string) (string, error) {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	switch {
	case args[0] == "rev-parse" && has("--git-common-dir") && has("--path-format=absolute"):
		return "/repo/.git", nil
	case args[0] == "rev-parse" && has("--show-toplevel"):
		return "/repo", nil
	case args[0] == "symbolic-ref":
		return "main", nil
	case args[0] == "rev-list":
		return "rootsha0000", nil
	}
	return "", fmt.Errorf("unexpected git args: %v", args)
}

func newTestApp(t *testing.T) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// copyTree copies the fixture sources into an isolated worktree so indexing
// never touches the repo's own checkout (and the temp dir is not a git repo,
// exercising the documented filesystem-walk fallback deterministically).
func copyTree(t *testing.T, fx *fixture, dst string) {
	t.Helper()
	src := fx.root
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if fx.man.excluded(d.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
}

func runIndex(t *testing.T, a *app.App) string {
	t.Helper()
	var out strings.Builder
	if err := cli.Index(context.Background(), a, &out, nil); err != nil {
		t.Fatalf("recarto index: %v", err)
	}
	return out.String()
}

func runGraph(t *testing.T, a *app.App, args ...string) string {
	t.Helper()
	var out strings.Builder
	if err := cli.Graph(context.Background(), a, &out, args); err != nil {
		t.Fatalf("recarto graph %s: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

// --- CLI output parsing ----------------------------------------------------

// bulletRow is one `• [kind] qn  …  path:line` result line.
type bulletRow struct {
	Kind, QN, Attr, Loc string
}

// parseBullets parses `• [kind] qn  path:line` rows (search, query, impls) and
// the 3-column annotated form `• [kind] qn  [attr]  path:line`.
func parseBullets(t *testing.T, out string) []bulletRow {
	t.Helper()
	var rows []bulletRow
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "• [") {
			continue
		}
		rest := strings.TrimPrefix(line, "• [")
		end := strings.Index(rest, "] ")
		if end < 0 {
			t.Fatalf("unparseable bullet row %q", line)
		}
		row := bulletRow{Kind: rest[:end]}
		parts := strings.Split(strings.TrimSpace(rest[end+2:]), "  ")
		switch len(parts) {
		case 2:
			row.QN, row.Loc = parts[0], parts[1]
		case 3: // annotated: qn  [attr]  path:line
			row.QN, row.Attr, row.Loc = parts[0], strings.Trim(parts[1], "[]"), parts[2]
		default:
			t.Fatalf("unparseable bullet row %q", line)
		}
		rows = append(rows, row)
	}
	return rows
}

// outlineRow is one `[kind] name-or-signature  :line` row of `graph outline`.
type outlineRow struct {
	Kind string
	Line int
}

func parseOutline(t *testing.T, out string) []outlineRow {
	t.Helper()
	var rows []outlineRow
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue // header path line, blank, or "(no symbols …)"
		}
		end := strings.Index(trimmed, "]")
		colon := strings.LastIndex(trimmed, "  :")
		if end < 0 || colon < 0 {
			t.Fatalf("unparseable outline row %q", line)
		}
		n, err := strconv.Atoi(trimmed[colon+3:])
		if err != nil {
			t.Fatalf("unparseable outline line number in %q", line)
		}
		rows = append(rows, outlineRow{Kind: trimmed[1:end], Line: n})
	}
	return rows
}

// grepRow is one `path:line  [annot]  text` row of `graph grep`.
type grepRow struct {
	Loc, Annot, Text string
}

func parseGrep(t *testing.T, out string) []grepRow {
	t.Helper()
	var rows []grepRow
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line == "(no matches)" {
			continue
		}
		if strings.HasPrefix(line, "… showing") {
			t.Fatalf("grep page truncated despite raised limits: %q", line)
		}
		parts := strings.SplitN(line, "  ", 3)
		if len(parts) != 3 {
			t.Fatalf("unparseable grep row %q", line)
		}
		rows = append(rows, grepRow{Loc: parts[0], Annot: strings.Trim(parts[1], "[]"), Text: parts[2]})
	}
	return rows
}

// chainSegments splits a trace chain line into its call-direction segments
// (the CLI renders every chain left-calls-right in both modes).
func chainSegments(line string) []string {
	parts := strings.Split(line, "→")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// chainHasOrderedPair reports whether any chain line in out names callerLeaf in
// an earlier segment than calleeLeaf.
func chainHasOrderedPair(out, callerLeaf, calleeLeaf string) bool {
	callerRe := regexp.MustCompile(wordPattern(callerLeaf))
	calleeRe := regexp.MustCompile(wordPattern(calleeLeaf))
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "→") {
			continue
		}
		seenCaller := false
		for _, s := range chainSegments(line) {
			if !seenCaller && callerRe.MatchString(s) {
				seenCaller = true
				continue
			}
			if seenCaller && calleeRe.MatchString(s) {
				return true
			}
		}
	}
	return false
}
