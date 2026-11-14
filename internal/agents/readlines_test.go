package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNumberedLines(t *testing.T) {
	for name, c := range map[string]struct {
		text, sep   string
		first, last int
	}{
		"opencode 2": {"Read file x.go, lines 10-12\n10: a\n11: \n12: c\n\n[Output truncated. Continue reading with offset: 13]", ": ", 10, 12},
		"opencode 1": {"<path>/x.go</path>\n<type>file</type>\n<content>\n1: x\n2: y\n\n(End of file - total 2 lines)\n</content>\n<system-reminder>\n1: not a file line\n</system-reminder>", ": ", 1, 2},
		"copilot":    {"5. a\n6. b\n\n[Output truncated. Use view_range=[7, ...] to continue.]", ". ", 5, 6},
		"none":       {"Image read successfully", ": ", 0, 0},
		"gap":        {"1: a\n2: b\n7: c", ": ", 1, 2},
	} {
		if first, last := NumberedLines(c.text, c.sep); first != c.first || last != c.last {
			t.Errorf("%s: %d-%d, want %d-%d", name, first, last, c.first, c.last)
		}
	}
}

func TestFileLines(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2} {
		path := filepath.Join(dir, "f")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if n, err := fileLines(path); err != nil || n != want {
			t.Errorf("fileLines(%q) = %d, %v; want %d", content, n, err, want)
		}
	}
}
