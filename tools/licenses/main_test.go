package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoticesMatchInputs(t *testing.T) {
	got, err := inputs("../..")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../internal/licenses/inputs.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatal("go.mod or recarto's third-party imports changed since the notices were generated: run go run ./tools/licenses from the repository root")
	}
}

func TestInputsSeePlatformOnlyImports(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/m\n\ngo 1.26.4\n\nrequire example.org/winonly v1.2.3\n")
	writeFile("cmd/recarto/main.go", "package main\n\nimport _ \"example.com/m/internal/x\"\n\nfunc main() {}\n")
	writeFile("internal/x/x.go", "package x\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n")
	writeFile("internal/x/x_windows.go", "package x\n\nimport _ \"example.org/winonly\"\n")
	writeFile("internal/x/x_ort.go", "//go:build ORT\n\npackage x\n\nimport _ \"example.org/ort\"\n")

	got, err := inputs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"require example.org/winonly v1.2.3\n",
		"import windows/amd64 example.org/winonly\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("inputs missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"import linux/amd64 example.org/winonly\n",
		"example.org/ort",
		" fmt\n",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("inputs unexpectedly contain %q:\n%s", unwanted, got)
		}
	}
}

func TestLicenseFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, body string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("LICENSE", "MIT\n")
	writeFile("NOTICE", "notice\n")
	writeFile("COPYING.txt", "line one\r\nline two\r\n")
	writeFile("LICENSE.MIT", "mit\n")
	writeFile("LICENSE.minisig", "not a licence\n")
	writeFile("copyright_test.go", "package x\n")
	writeFile("sub/License", "nested\n")
	writeFile("testdata/LICENSE", "ignored\n")
	writeFile(".git/LICENSE", "ignored\n")

	texts, err := licenseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, text := range texts {
		paths = append(paths, text.Path)
	}
	want := "COPYING.txt LICENSE LICENSE.MIT NOTICE sub/License"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("licence files = %q, want %q", got, want)
	}
	if texts[0].Body != "line one\nline two\n" {
		t.Errorf("CRLF not normalized: %q", texts[0].Body)
	}
}
