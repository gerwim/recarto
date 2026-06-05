package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestIndexProgressPlainLinesNonTTY: when output is not a terminal, the reporter
// emits a plain "indexed N/total files" line every `every` files plus a final
// line on Done — never an animated bar (which would be unreadable in a log/pipe).
func TestIndexProgressPlainLinesNonTTY(t *testing.T) {
	var buf bytes.Buffer
	p := newIndexProgress(&buf, false)
	p.every = 1000

	p.Start(2500)
	for range 2500 {
		p.Inc()
	}
	p.Phase("resolving references")
	p.Done()

	out := buf.String()
	if !strings.Contains(out, "resolving references...") {
		t.Errorf("output missing phase line\n--- got ---\n%s", out)
	}
	for _, want := range []string{"1000/2500", "2000/2500", "2500/2500"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestIndexProgressSilentWhenNothingToDo: an empty worklist (an up-to-date index)
// must produce no output at all in a non-TTY context.
func TestIndexProgressSilentWhenNothingToDo(t *testing.T) {
	var buf bytes.Buffer
	p := newIndexProgress(&buf, false)
	p.Start(0)
	p.Done()
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty worklist, got %q", buf.String())
	}
}
