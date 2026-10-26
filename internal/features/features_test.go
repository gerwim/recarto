package features

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func configPath(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if content != "" {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryFeatureIsOnByDefault(t *testing.T) {
	s, err := Read(configPath(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range All() {
		if !s.On(f) || !Set(nil).On(f) {
			t.Errorf("%s is off without a config file", f.Key)
		}
	}
}

// TestRegistry keeps every feature addressable and its row within 80 columns.
func TestRegistry(t *testing.T) {
	key := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	seen := map[string]bool{}
	for _, f := range All() {
		if !key.MatchString(f.Key) || seen[f.Key] {
			t.Errorf("key %q must be unique kebab-case", f.Key)
		}
		seen[f.Key] = true
		if f.Group == "" || f.Desc == "" {
			t.Errorf("%s needs a group and a description", f.Key)
		}
		if n := len([]rune(f.Desc)); n > 54 {
			t.Errorf("%s: description is %d characters; keep it to 54 so the row fits 80 columns", f.Key, n)
		}
	}
	if keyWidth() > 16 {
		t.Errorf("a feature name is longer than 16 characters; the descriptions above assume 16")
	}
}

func TestReadHandEditedFile(t *testing.T) {
	p := configPath(t, "\ufeff# mine\r\n\r\ntodos = false   # too noisy\r\n  stop-nudge=false\r\ncode-map = true\r\n")
	s, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.On(Todos) || s.On(StopNudge) || !s.On(CodeMap) || !s.On(Sync) {
		t.Errorf("got %v", s)
	}
}

func TestReadReportsEveryProblemWithItsLine(t *testing.T) {
	p := configPath(t, "todo = false\ncode-map = no\ntodos = false\ntodos = true\n[features]\nsync = false\n")
	s, err := Read(p)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		p + `:1: unknown feature "todo" (did you mean todos?)`,
		p + `:2: code-map must be true or false, not "no"`,
		p + `:4: todos is set twice`,
		p + `:5: want <feature> = true or false`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	// What parses still applies, as hooks read it.
	if s.On(Todos) || s.On(Sync) || !s.On(CodeMap) {
		t.Errorf("got %v", s)
	}
}

func TestSaveNewFileListsEveryFeature(t *testing.T) {
	p := configPath(t, "")
	if err := Save(p, Set{Todos: false}); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, p)
	for _, f := range All() {
		want := entry(f, f != Todos)
		if !strings.Contains(text, want+"\n") || !strings.Contains(text, "# "+f.Group+"\n") {
			t.Errorf("new file lacks %q under # %s:\n%s", want, f.Group, text)
		}
	}
	s, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.On(Todos) || !s.On(StopNudge) {
		t.Errorf("read back %v", s)
	}
}

func TestSaveKeepsHandEdits(t *testing.T) {
	p := configPath(t, "")
	if err := Save(p, Set{}); err != nil {
		t.Fatal(err)
	}
	original := readFile(t, p)
	if err := Save(p, Set{Todos: false, StopNudge: false}); err != nil {
		t.Fatal(err)
	}
	off := readFile(t, p)
	if !strings.Contains(off, "\n"+entry(Todos, false)+"\n") {
		t.Errorf("value not rewritten in place, or comment moved:\n%s", off)
	}
	if err := Save(p, Set{Todos: true, StopNudge: true}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != original {
		t.Errorf("toggling back changed the file:\n%s\nwant:\n%s", got, original)
	}

	// A hand-written file keeps its comments and line endings; a feature it
	// does not list is appended.
	p = configPath(t, "# my settings\r\ntodos = false # noisy\r\n")
	if err := Save(p, Set{Todos: true, Sync: false}); err != nil {
		t.Fatal(err)
	}
	want := "# my settings\r\ntodos = true  # noisy\r\n" + entry(Sync, false) + "\r\n"
	if got := readFile(t, p); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSaveLeavesABadFileAlone(t *testing.T) {
	p := configPath(t, "todo = false\n")
	if err := Save(p, Set{Sync: false}); err == nil || !strings.Contains(err.Error(), ":1: unknown feature") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, p); got != "todo = false\n" {
		t.Errorf("file changed: %q", got)
	}
}

func TestPathAndLoadHonorRecartoConfig(t *testing.T) {
	p := configPath(t, "sync = false\n")
	t.Setenv("RECARTO_CONFIG", p)
	if got, err := Path(); err != nil || got != p {
		t.Fatalf("Path() = %q, %v", got, err)
	}
	if Load().On(Sync) {
		t.Error("Load ignored the file")
	}
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	if !Load().On(Sync) {
		t.Error("a missing file must leave features on")
	}
}

func TestLookup(t *testing.T) {
	if f, err := Lookup("stop-nudge"); err != nil || f != StopNudge {
		t.Fatalf("Lookup = %v, %v", f, err)
	}
	if _, err := Lookup("memories"); err == nil || !strings.Contains(err.Error(), "did you mean session-memories?") {
		t.Errorf("err = %v", err)
	}
	if _, err := Lookup("zzz"); err == nil || !strings.Contains(err.Error(), "recarto config list") {
		t.Errorf("err = %v", err)
	}
}
