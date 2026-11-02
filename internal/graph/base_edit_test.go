package graph

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// reindexFile runs the post-edit hook path on one file: read it, stat it, and
// rewrite its graph incrementally.
func reindexFile(t *testing.T, e *Engine, dir, rel string) {
	t.Helper()
	abs := filepath.Join(dir, rel)
	content, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReindexFileIncremental(context.Background(), dir, rel, content, info.ModTime().UnixMilli(), info.Size()); err != nil {
		t.Fatalf("reindex %s: %v", rel, err)
	}
}

// ifaceTargets returns the qualified names a call `recv.name` dispatches to by
// interface (method 'iface'), sorted.
func ifaceTargets(t *testing.T, e *Engine, recv, name string) []string {
	t.Helper()
	var out []string
	for qn, method := range candidateTargets(t, e, name, recv) {
		if method == "iface" {
			out = append(out, qn)
		}
	}
	slices.Sort(out)
	return out
}

// TestBaseEditMatchesFromScratch: a base moved between the declarations of one
// class, added to a second declaration, or swapped between two Rust impl blocks,
// changes which calls interface dispatch answers even though the class and base
// names are unchanged. None of those edits may leave a stale candidate behind, on
// either the reconcile path or the post-edit hook path: both must end up exactly
// as a from-scratch index of the same tree.
func TestBaseEditMatchesFromScratch(t *testing.T) {
	const oneFileInit = "namespace App;\npartial class X : IA { public void M() {} }\n" +
		"partial class X { public void N() {} }\n"
	const csIface = "namespace App;\ninterface IA { void M(); void N(); }\n"
	const csCaller = "namespace App;\nclass C { void Go(IA a) { a.M(); a.N(); } }\n"

	cases := []struct {
		name string
		init map[string]string
		// edits are the worktree-relative paths rewritten, in order; each takes its
		// final content from edit.
		edits    []string
		edit     map[string]string
		full     bool
		dispatch map[string][]string
	}{
		{
			name: "cs one file",
			init: map[string]string{
				"i.cs": csIface,
				"x.cs": oneFileInit,
				"c.cs": csCaller,
			},
			edits: []string{"x.cs"},
			edit: map[string]string{"x.cs": "namespace App;\npartial class X { public void M() {} }\n" +
				"partial class X : IA { public void N() {} }\n"},
			dispatch: map[string][]string{"a.M": nil, "a.N": {"App.X.N"}},
		},
		{
			name: "cs two files",
			init: map[string]string{
				"i.cs":  csIface,
				"x1.cs": "namespace App;\npartial class X : IA { public void M() {} }\n",
				"x2.cs": "namespace App;\npartial class X { public void N() {} }\n",
				"c.cs":  csCaller,
			},
			edits: []string{"x1.cs", "x2.cs"},
			edit: map[string]string{
				"x1.cs": "namespace App;\npartial class X { public void M() {} }\n",
				"x2.cs": "namespace App;\npartial class X : IA { public void N() {} }\n",
			},
			dispatch: map[string][]string{"a.M": nil, "a.N": {"App.X.N"}},
		},
		{
			name: "cs base added to another part",
			init: map[string]string{
				"i.cs": csIface,
				"x.cs": oneFileInit,
				"c.cs": csCaller,
			},
			edits: []string{"x.cs"},
			edit: map[string]string{"x.cs": "namespace App;\npartial class X : IA { public void M() {} }\n" +
				"partial class X : IA { public void N() {} }\n"},
			dispatch: map[string][]string{"a.M": {"App.X.M"}, "a.N": {"App.X.N"}},
		},
		{
			name: "Rust",
			init: map[string]string{
				"t.rs": "trait T { fn a(&self); fn b(&self); }\ntrait U { fn a(&self); fn b(&self); }\n",
				"x.rs": "struct Foo;\nimpl T for Foo { fn a(&self) {} }\nimpl U for Foo { fn b(&self) {} }\n",
				"c.rs": "fn go(t: &T, u: &U) { t.a(); t.b(); u.a(); u.b(); }\n",
			},
			edits: []string{"x.rs"},
			edit: map[string]string{
				"x.rs": "struct Foo;\nimpl U for Foo { fn a(&self) {} }\nimpl T for Foo { fn b(&self) {} }\n",
			},
			dispatch: map[string][]string{"t.a": nil, "t.b": {"Foo.b"}, "u.a": {"Foo.a"}, "u.b": nil},
		},
		{
			name: "Python",
			init: map[string]string{
				"b.py": "class B:\n    def m(self): pass\n    def n(self): pass\n",
				"x.py": "from b import B\nclass X(B):\n    def m(self): pass\nclass X:\n    def n(self): pass\n",
				"c.py": "from b import B\ndef go(a: B):\n    a.m()\n    a.n()\n",
			},
			edits: []string{"x.py"},
			edit: map[string]string{
				"x.py": "from b import B\nclass X:\n    def m(self): pass\nclass X(B):\n    def n(self): pass\n",
			},
			full:     true,
			dispatch: map[string][]string{"a.m": nil, "a.n": {"X.n"}},
		},
		{
			name: "cs class gains a base",
			init: map[string]string{
				"i.cs": "namespace App;\ninterface IA { void M(); }\n",
				"x.cs": "namespace App;\nclass X { public void M() {} }\n",
				"c.cs": "namespace App;\nclass C { void Go(IA a) { a.M(); } }\n",
			},
			edits: []string{"x.cs"},
			edit: map[string]string{
				"x.cs": "namespace App;\nclass X : IA { public void M() {} }\n",
			},
			dispatch: map[string][]string{"a.M": {"App.X.M"}},
		},
		{
			name: "TypeScript class gains a base",
			init: map[string]string{
				"b.ts": "class B { m() {} }\n",
				"x.ts": "class X { m() {} }\n",
				"c.ts": "function go(a: B) { a.m(); }\n",
			},
			edits: []string{"x.ts"},
			edit: map[string]string{
				"x.ts": "class X extends B { m() {} }\n",
			},
			dispatch: map[string][]string{"a.m": nil},
		},
		{
			name: "JavaScript class gains a base",
			init: map[string]string{
				"b.js": "class B { m() {} }\n",
				"x.js": "class X { m() {} }\n",
				"c.js": "function go() { new X().m(); }\n",
			},
			edits: []string{"x.js"},
			edit: map[string]string{
				"x.js": "class X extends B { m() {} }\n",
			},
			dispatch: map[string][]string{".m": nil},
		},
		{
			name: "Rust impl gains a trait",
			init: map[string]string{
				"t.rs": "trait T { fn a(&self); }\n",
				"x.rs": "struct Foo;\nimpl Foo { fn a(&self) {} }\n",
				"c.rs": "fn go(t: &T) { t.a(); }\n",
			},
			edits: []string{"x.rs"},
			edit: map[string]string{
				"x.rs": "struct Foo;\nimpl T for Foo { fn a(&self) {} }\n",
			},
			dispatch: map[string][]string{"t.a": {"Foo.a"}},
		},
		{
			name: "Python class gains a base",
			init: map[string]string{
				"b.py": "class B:\n    def m(self): pass\n",
				"x.py": "from b import B\nclass X:\n    def m(self): pass\n",
				"c.py": "from b import B\ndef go(a: B):\n    a.m()\n",
			},
			edits: []string{"x.py"},
			edit: map[string]string{
				"x.py": "from b import B\nclass X(B):\n    def m(self): pass\n",
			},
			full:     true,
			dispatch: map[string][]string{"a.m": {"X.m"}},
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		for _, path := range []string{"index", "hook"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				wt := t.TempDir()
				for rel, content := range tc.init {
					writeBumped(t, wt, rel, content)
				}
				e := newTestEngine(t)
				if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
					t.Fatal(err)
				}
				for _, rel := range tc.edits {
					writeBumped(t, wt, rel, tc.edit[rel])
				}

				if path == "index" {
					p := &phaseLog{}
					if err := e.Reconcile(ctx, wt, FSSource{Root: wt}, WithProgress(p)); err != nil {
						t.Fatal(err)
					}
					gotFull := slices.ContainsFunc(p.phases, func(s string) bool { return strings.Contains(s, "/8:") })
					if gotFull != tc.full {
						t.Errorf("full resolve = %v, want %v (phases %v)", gotFull, tc.full, p.phases)
					}
				} else {
					for _, rel := range tc.edits {
						reindexFile(t, e, wt, rel)
					}
				}

				got := resolveSignature(t, e, wt)
				want := resolveSignature(t, fromScratch(t, wt), wt)
				if got != want {
					t.Fatalf("%s path differs from a from-scratch index\n--- incremental ---\n%s\n--- from-scratch ---\n%s",
						path, got, want)
				}
				for call, expected := range tc.dispatch {
					recv, name, _ := strings.Cut(call, ".")
					if got := ifaceTargets(t, e, recv, name); !slices.Equal(got, expected) {
						t.Errorf("%s: iface targets for %q = %v, want %v", path, call, got, expected)
					}
				}
			})
		}
	}
}
