package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLanguageFixtures discovers every language fixture under testdata/ and
// runs the full index-then-query verification suite against it.
func TestLanguageFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("testdata", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
			continue
		}
		ran = true
		t.Run(e.Name(), func(t *testing.T) { runLanguageSuite(t, dir) })
	}
	if !ran {
		t.Fatal("no language fixtures found under testdata/")
	}
}

func runLanguageSuite(t *testing.T, dir string) {
	fx := loadFixture(t, dir)

	// The fixture polices itself before the engine is consulted: a failure here
	// is an AUTHORING error in testdata (fix the markers), never an engine bug.
	t.Run("fixture-hygiene", func(t *testing.T) { checkFixtureHygiene(t, fx) })

	wt := t.TempDir()
	copyTree(t, fx, wt)
	a := newTestApp(t)
	a.Git.Worktree = wt

	indexOut := runIndex(t, a)

	t.Run("census", func(t *testing.T) {
		var files, symbols int
		if _, err := fmt.Sscanf(indexOut, "indexed %d files, %d symbols", &files, &symbols); err != nil {
			t.Fatalf("unparseable index summary %q: %v", indexOut, err)
		}
		if files != len(fx.srcFiles) {
			t.Errorf("indexed %d files, fixture has %d source files", files, len(fx.srcFiles))
		}
		if symbols != len(fx.defs) {
			t.Errorf("indexed %d symbols, fixture declares %d (missing or phantom definitions)", symbols, len(fx.defs))
		}
	})

	t.Run("outline", func(t *testing.T) {
		for _, f := range fx.srcFiles {
			got := map[string]int{} // "kind:line" -> count
			for _, r := range parseOutline(t, runGraph(t, a, "outline", f)) {
				got[fmt.Sprintf("%s:%d", r.Kind, r.Line)]++
			}
			want := map[string]int{}
			for _, d := range fx.defs {
				if d.File == f {
					want[fmt.Sprintf("%s:%d", d.Kind, d.Line)]++
				}
			}
			compareCounts(t, "outline "+f, want, got)
		}
	})

	t.Run("query", func(t *testing.T) {
		for _, f := range fx.srcFiles {
			got := map[string]int{}
			for _, r := range parseBullets(t, runGraph(t, a, "query", "--path="+f, "--limit=500")) {
				got[fmt.Sprintf("[%s] %s %s", r.Kind, r.QN, r.Loc)]++
			}
			want := map[string]int{}
			for _, d := range fx.defs {
				if d.File == f {
					want[fmt.Sprintf("[%s] %s %s", d.Kind, d.QN, d.loc())]++
				}
			}
			compareCounts(t, "query --path="+f, want, got)
		}
	})

	t.Run("grep", func(t *testing.T) {
		for _, name := range fx.trackedNames() {
			out := runGraph(t, a, "grep", "-r", wordPattern(name),
				"--limit=100000", "--max-per-file=100000")
			rows := parseGrep(t, out)
			got := map[string]bool{}
			rowByLoc := map[string]grepRow{}
			for _, r := range rows {
				got[r.Loc] = true
				rowByLoc[r.Loc] = r
			}
			want := fx.oracle(name, false)
			if diff := diffSets(want, got); diff != "" {
				t.Errorf("grep -r '%s' disagrees with reference scan:%s", wordPattern(name), diff)
				continue
			}
			// A hit on a definition's own declaration line must be attributed to
			// that definition (the innermost enclosing symbol), not a container or
			// an unrelated symbol — wrong attribution misleads harder than no hit.
			for _, d := range fx.defs {
				if d.name() != name {
					continue
				}
				row, ok := rowByLoc[d.loc()]
				if !ok {
					continue // already reported by the set diff
				}
				if wantAnnot := d.Kind + " " + d.QN; row.Annot != wantAnnot {
					t.Errorf("grep '%s' at %s: enclosing symbol = %q, want %q",
						name, d.loc(), row.Annot, wantAnnot)
				}
			}
		}
	})

	t.Run("search", func(t *testing.T) {
		byLeaf := map[string]map[string]bool{} // leaf -> expected qualified names
		for _, d := range fx.defs {
			if d.Kind == "namespace" {
				continue // ranked search demotes namespace declarations by design
			}
			if byLeaf[d.name()] == nil {
				byLeaf[d.name()] = map[string]bool{}
			}
			byLeaf[d.name()][d.QN] = true
		}
		leaves := make([]string, 0, len(byLeaf))
		for l := range byLeaf {
			leaves = append(leaves, l)
		}
		sort.Strings(leaves)
		for _, l := range leaves {
			out := runGraph(t, a, "search", "--symbol="+l, "--lang="+fx.lang, "--limit=20")
			found := map[string]bool{}
			for _, r := range parseBullets(t, out) {
				found[r.QN] = true
			}
			for qn := range byLeaf[l] {
				if !found[qn] {
					t.Errorf("search --symbol=%s does not surface %s\noutput:\n%s", l, qn, out)
				}
			}
		}
	})

	t.Run("snippet", func(t *testing.T) {
		byQN := map[string][]def{}
		for _, d := range fx.defs {
			byQN[d.QN] = append(byQN[d.QN], d)
		}
		qns := make([]string, 0, len(byQN))
		for qn := range byQN {
			qns = append(qns, qn)
		}
		sort.Strings(qns)
		for _, qn := range qns {
			defs := byQN[qn]
			out := runGraph(t, a, "snippet", qn)
			switch {
			case strings.Contains(out, "(no symbol named"):
				t.Errorf("snippet %s: symbol not found", qn)
			case strings.Contains(out, "is ambiguous"):
				// Acceptable for one qualified name defined in several files
				// (partial classes, a namespace declared per file) — but then every
				// definition must be offered as a candidate.
				for _, d := range defs {
					if !strings.Contains(out, d.loc()) {
						t.Errorf("snippet %s: ambiguous, but candidate list omits %s\noutput:\n%s", qn, d.loc(), out)
					}
				}
				if len(defs) == 1 {
					t.Errorf("snippet %s: ambiguous result for a unique definition\noutput:\n%s", qn, out)
				}
			default:
				for _, d := range defs {
					// The snippet must show the declaration: either it contains the
					// declaration line's code, or — for single-token symbols like enum
					// members, whose node is just the identifier — the whole snippet is
					// a sub-span of that line.
					decl := strings.TrimSpace(fx.stripMarker(fx.lines[d.File][d.Line-1]))
					// graph snippet leads with a "// path:start-end" header line; the
					// sub-span fallback below must compare the BODY only, or a one-token
					// snippet (enum member) can never be a sub-span of its line.
					body := strings.TrimSpace(out)
					if first, rest, ok := strings.Cut(body, "\n"); ok && strings.HasPrefix(first, "// ") {
						body = strings.TrimSpace(rest)
					}
					if !strings.Contains(out, decl) && !(body != "" && strings.Contains(decl, body)) {
						t.Errorf("snippet %s: missing declaration %q from %s\noutput:\n%s", qn, decl, d.loc(), out)
					}
				}
			}
		}
	})

	t.Run("impls", func(t *testing.T) {
		for _, rel := range fx.impls {
			where := fmt.Sprintf("impl marker at %s:%d", rel.File, rel.Line)
			subQN := fx.resolveUniqueDef(t, rel.Sub, typeKinds, where)
			superQN := fx.resolveUniqueDef(t, rel.Super, typeKinds, where)
			down := runGraph(t, a, "impls", superQN, "--limit=100")
			if !bulletsContainQN(t, down, subQN) {
				t.Errorf("%s: impls %s does not list implementer %s\noutput:\n%s", where, superQN, subQN, down)
			}
			up := runGraph(t, a, "impls", subQN, "--super", "--limit=100")
			if !bulletsContainQN(t, up, superQN) {
				t.Errorf("%s: impls %s --super does not list supertype %s\noutput:\n%s", where, subQN, superQN, up)
			}
		}
	})

	t.Run("calls", func(t *testing.T) {
		for _, c := range fx.calls {
			where := fmt.Sprintf("call marker at %s:%d", c.File, c.Line)
			if got := fx.resolveCallee(c.Callee); len(got) == 0 {
				t.Fatalf("%s: callee spec %q matches no def marker", where, c.Callee)
			}
			callerQN := fx.resolveUniqueDef(t, c.Caller, callableKinds, where)
			callers := runGraph(t, a, "callers", c.Callee)
			if !chainHasOrderedPair(callers, leaf(c.Caller), leaf(c.Callee)) {
				t.Errorf("%s: callers %s has no chain %s → %s\noutput:\n%s",
					where, c.Callee, leaf(c.Caller), leaf(c.Callee), callers)
			}
			callees := runGraph(t, a, "callees", callerQN)
			if !chainHasOrderedPair(callees, leaf(c.Caller), leaf(c.Callee)) {
				t.Errorf("%s: callees %s has no chain %s → %s\noutput:\n%s",
					where, callerQN, leaf(c.Caller), leaf(c.Callee), callees)
			}
		}
	})

	t.Run("annotated", func(t *testing.T) {
		for _, an := range fx.annots {
			where := fmt.Sprintf("annot marker at %s:%d", an.File, an.Line)
			targetQN := fx.resolveUniqueDef(t, an.Target, anyKind, where)
			out := runGraph(t, a, "annotated", an.Attr, "--limit=100")
			ok := false
			for _, r := range parseBullets(t, out) {
				if r.QN == targetQN && strings.Contains(r.Attr, an.Attr) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: annotated %s does not list %s\noutput:\n%s", where, an.Attr, targetQN, out)
			}
		}
	})

	t.Run("type-dead-end-hint", func(t *testing.T) {
		// A type that is never constructed (an interface) has no caller chains;
		// the dead-end must be diagnosed as "this is a type" — steering to impls
		// and grep — never as "may be an entrypoint", which describes methods and
		// sent a real user hunting in the wrong direction.
		for _, d := range fx.defs {
			if d.Kind != "interface" {
				continue
			}
			out := runGraph(t, a, "callers", d.QN)
			if strings.Contains(out, "→") {
				continue // it has chains after all; nothing to diagnose
			}
			if !strings.Contains(out, "is a type") {
				t.Errorf("callers %s: dead-end for a type lacks the type hint\noutput:\n%s", d.QN, out)
			}
			if strings.Contains(out, "entrypoint") {
				t.Errorf("callers %s: a type is misdiagnosed as an entrypoint\noutput:\n%s", d.QN, out)
			}
		}
	})

	t.Run("reindex-idempotent", func(t *testing.T) {
		if len(fx.defs) == 0 {
			t.Skip("no defs")
		}
		probe := wordPattern(fx.defs[0].name())
		before := runGraph(t, a, "grep", "-r", probe, "--limit=100000", "--max-per-file=100000")
		again := runIndex(t, a)
		if again != indexOut {
			t.Errorf("re-index changed the census:\n first: %s\nsecond: %s", indexOut, again)
		}
		after := runGraph(t, a, "grep", "-r", probe, "--limit=100000", "--max-per-file=100000")
		if before != after {
			t.Errorf("re-index changed grep output for %s:\nbefore:\n%s\nafter:\n%s", probe, before, after)
		}
	})
}

// checkFixtureHygiene mechanically enforces the authoring discipline that makes
// grep assertions exact: in the marker-stripped SOURCE text, every whole-word
// occurrence of a tracked simple name must be a def or ref marker line, and
// every def/ref marker must have a matching occurrence on its line. Dotted
// names (namespaces) are exempt: a word-boundary regex on a dotted name also
// matches its dotted extensions, which is textual-search semantics, not a
// fixture defect. Relation specs must also resolve against the def table.
func checkFixtureHygiene(t *testing.T, fx *fixture) {
	for _, name := range fx.trackedNames() {
		if strings.Contains(name, ".") {
			continue
		}
		want := fx.markedLines(name, true)
		got := fx.oracle(name, true)
		if diff := diffSets(want, got); diff != "" {
			t.Errorf("fixture accounting for %q is off (def/ref markers vs code text):%s", name, diff)
		}
	}
	for _, rel := range fx.impls {
		where := fmt.Sprintf("impl marker at %s:%d", rel.File, rel.Line)
		fx.resolveUniqueDef(t, rel.Sub, typeKinds, where)
		fx.resolveUniqueDef(t, rel.Super, typeKinds, where)
	}
	for _, c := range fx.calls {
		where := fmt.Sprintf("call marker at %s:%d", c.File, c.Line)
		fx.resolveUniqueDef(t, c.Caller, callableKinds, where)
		if len(fx.resolveCallee(c.Callee)) == 0 {
			t.Errorf("%s: callee spec %q matches no def marker", where, c.Callee)
		}
	}
	for _, an := range fx.annots {
		fx.resolveUniqueDef(t, an.Target, anyKind, fmt.Sprintf("annot marker at %s:%d", an.File, an.Line))
	}
}

// compareCounts diffs two multisets rendered as item->count maps.
func compareCounts(t *testing.T, context string, want, got map[string]int) {
	t.Helper()
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if want[k] != got[k] {
			t.Errorf("%s: %s — expected %d, engine returned %d", context, k, want[k], got[k])
		}
	}
}

func bulletsContainQN(t *testing.T, out, qn string) bool {
	t.Helper()
	for _, r := range parseBullets(t, out) {
		if r.QN == qn {
			return true
		}
	}
	return false
}
