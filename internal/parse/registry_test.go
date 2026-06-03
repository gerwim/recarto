package parse

import "testing"

func TestLangForExt(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go": "go", "x.cs": "csharp", "x.ts": "typescript",
		"x.tsx": "tsx", "x.js": "javascript", "x.jsx": "javascript",
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

func TestNewRegistryCompilesGoQuery(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.queries["go"] == nil || r.langs["go"] == nil {
		t.Fatal("go language/query not registered")
	}
}
