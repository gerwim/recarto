package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// shellTree makes dir/{f.go, g.go, internal/x.go} and returns dir.
func shellTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"f.go", "g.go", filepath.Join("internal", "x.go")} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestParseShell(t *testing.T) {
	dir := shellTree(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, "h.go"), []byte("package h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, g, x := filepath.Join(dir, "f.go"), filepath.Join(dir, "g.go"), filepath.Join(dir, "internal", "x.go")
	type want struct {
		owner    string
		patterns []string
		files    []string
	}
	for _, tc := range []struct {
		cmd string
		want
	}{
		// Content searches.
		{`grep -rn "handleAugment" internal/`, want{"grep", []string{"handleAugment"}, nil}},
		{`rg handleAugment`, want{"rg", []string{"handleAugment"}, nil}},
		{`git grep -n handleAugment`, want{"git", []string{"handleAugment"}, nil}},
		{`egrep -R 'a|b' .`, want{"egrep", []string{"a|b"}, nil}},
		{`ugrep -r -t go Foo`, want{"ugrep", []string{"Foo"}, nil}},
		{`rg -n Foo internal/*.go`, want{"rg", []string{"Foo"}, nil}},
		{`git grep -e Foo HEAD -- '*.go'`, want{"git", []string{"Foo"}, nil}},
		{`grep -rn "handle\(.*\)" .`, want{"grep", []string{`handle\(.*\)`}, nil}},
		// Chains, pipelines, flag values.
		{`cd internal && rg -t go Foo | head -20`, want{"rg", []string{"Foo"}, nil}},
		{`grep -A 3 -e Foo -e Bar .`, want{"grep", []string{"Foo", "Bar"}, nil}},
		{`grep --include=*.go -rn Foo .`, want{"grep", []string{"Foo"}, nil}},
		{`grep --include '*.go' -rnA3 Foo .`, want{"grep", []string{"Foo"}, nil}},
		{`rg --color never -g '*.go' Foo; echo done`, want{"rg", []string{"Foo"}, nil}},
		{`grep --color -rn Foo .`, want{"grep", []string{"Foo"}, nil}},
		{`X=1 rg -e Foo -e Foo`, want{"rg", []string{"Foo"}, nil}},
		// File-name searches.
		{`find . -name "*augment*.go"`, want{"find", []string{"*augment*.go"}, nil}},
		{`bfs . -type f -iname '*.cs' -o -path '*/agents/*'`, want{"bfs", []string{"*.cs", "*/agents/*"}, nil}},
		// Filters of other commands' output and other non-searches.
		{`ps aux | grep foo`, want{"grep", nil, nil}},
		{`go test ./... | grep FAIL`, want{"grep", nil, nil}},
		{`git log --grep fix`, want{}},
		{`cat f.go | rg Foo`, want{"cat", nil, []string{f}}},
		{`git status && go build ./...`, want{}},
		{`rg --files | rg augment`, want{"rg", nil, nil}},
		{`grep -rf patterns.txt .`, want{"grep", nil, nil}},
		{`grep -rn "$PAT" .`, want{"grep", nil, nil}},
		{`find . -type f | wc -l`, want{"find", nil, nil}},
		// Views.
		{`cat f.go`, want{"cat", nil, []string{f}}},
		{`cat -n f.go g.go missing.go`, want{"cat", nil, []string{f, g}}},
		{`head -n 50 f.go`, want{"head", nil, []string{f}}},
		{`head -50 f.go`, want{"head", nil, []string{f}}},
		{`tail f.go`, want{"tail", nil, []string{f}}},
		{`tail -n +10 -f f.go`, want{"tail", nil, []string{f}}},
		{`sed -n '10,40p' f.go`, want{"sed", nil, []string{f}}},
		{`sed -ne '1,5p' -e 'p' f.go g.go`, want{"sed", nil, []string{f, g}}},
		{`cat f.go | head`, want{"cat", nil, []string{f}}},
		{`grep -n Foo f.go`, want{"grep", nil, []string{f}}},
		{`grep -rn Foo f.go internal`, want{"grep", []string{"Foo"}, []string{f}}},
		{`cat f.go 2>/dev/null >&2`, want{"cat", nil, []string{f}}},
		// Writes and edits are not views.
		{"cat > f.go <<'EOF'\npackage x\ncat g.go\nEOF", want{"cat", nil, nil}},
		{`cat f.go >> g.go`, want{"cat", nil, nil}},
		{`sed -i 's/a/b/' f.go`, want{"sed", nil, nil}},
		{`sed -n -i 's/a/b/p' f.go`, want{"sed", nil, nil}},
		{`sed 's/a/b/' f.go`, want{"sed", nil, nil}},
		{`head -5 f.go > out.txt`, want{"head", nil, nil}},
		// Paths: cd, absolute, ~, subshells.
		{`cd internal && cat x.go`, want{"cat", nil, []string{x}}},
		{`cd internal; cd ..; cat f.go`, want{"cat", nil, []string{f}}},
		{`(cd internal && cat x.go); cat f.go`, want{"cat", nil, []string{x, f}}},
		{`cd internal | true; cat f.go`, want{"cat", nil, []string{f}}},
		{`cat ` + x, want{"cat", nil, []string{x}}},
		{`cat ~/h.go`, want{"cat", nil, []string{filepath.Join(home, "h.go")}}},
		{`cd && cat h.go`, want{"cat", nil, []string{filepath.Join(home, "h.go")}}},
		{`cd - && cat f.go`, want{"cat", nil, nil}},
		{`cd "$D" && cat f.go`, want{"cat", nil, nil}},
		{`cat "$F" *.go`, want{"cat", nil, nil}},
		// The owner is the first ShellCommands key, bare or not.
		{`echo a | head && cat f.go`, want{"head", nil, []string{f}}},
		{`sed -i 's/a/b/' f.go && cat f.go`, want{"sed", nil, []string{f}}},
		// Unparseable lines are silent.
		{`cat "f.go`, want{}},
		{`grep -rn Foo . | (`, want{}},
	} {
		got := parseShell(tc.cmd, dir)
		if got.owner != tc.owner || !reflect.DeepEqual(got.patterns, tc.patterns) || !reflect.DeepEqual(got.files, tc.files) {
			t.Errorf("%s\n got owner=%q patterns=%q files=%q\nwant owner=%q patterns=%q files=%q",
				tc.cmd, got.owner, got.patterns, got.files, tc.owner, tc.patterns, tc.files)
		}
	}
}

// A relative path in an unknown working directory resolves to nothing.
func TestParseShellUnknownCWD(t *testing.T) {
	if got := parseShell(`cat f.go`, ""); got.files != nil || got.owner != "cat" {
		t.Errorf("got %+v", got)
	}
}

// Git Bash on Windows spells C:\Users as /c/Users.
func TestGitBashPath(t *testing.T) {
	for in, want := range map[string]string{
		"/c/Users/me/f.go": "C:/Users/me/f.go",
		"/d":               "D:/",
		"/usr/bin":         "/usr/bin",
		"/tmp":             "/tmp",
		"f.go":             "f.go",
	} {
		if got := gitBashPath(in); got != want {
			t.Errorf("gitBashPath(%q) = %q, want %q", in, got, want)
		}
	}
}
