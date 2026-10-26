package parse

import "testing"

func TestLangForExt(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go": "go", "x.cs": "csharp", "x.ts": "typescript",
		"x.tsx": "tsx", "x.js": "javascript", "x.jsx": "javascript",
		"x.mjs": "javascript", "x.cjs": "javascript", "src/lib.rs": "rust",
		"pkg/mod.py": "python",
	}
	for path, want := range cases {
		got, ok := LangForExt(path)
		if !ok || got != want {
			t.Errorf("LangForExt(%q) = %q,%v; want %q,true", path, got, ok, want)
		}
	}
	if _, ok := LangForExt("README.md"); ok {
		t.Error("LangForExt(.md) should be unsupported")
	}
}

func TestNewRegistryCompilesAllQueries(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, lang := range []string{"go", "csharp", "typescript", "tsx", "javascript", "rust", "python"} {
		if r.langs[lang] == nil {
			t.Errorf("%s language not registered", lang)
		}
		if _, err := r.query(lang); err != nil {
			t.Error(err)
		}
	}
}
