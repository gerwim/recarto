package features

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// screen renders e as a w×h terminal shows it: plain text, one entry per line.
func screen(e *editor, w, h int) []string {
	return strings.Split(ansi.ReplaceAllString(e.view(w, h), ""), "\r\n")
}

func newEditor(t *testing.T) *editor {
	t.Helper()
	return &editor{path: filepath.Join(t.TempDir(), "config.toml"), set: Set{}, color: true}
}

func TestKeys(t *testing.T) {
	in := "j\x1b[A\x1bOB\x1b[H\x1b[4~ \r\x1b\x03q\x1b[1;5Aé"
	want := []string{"j", "up", "down", "home", "end", " ", "enter", "esc", "ctrl-c", "q", "é"}
	if got, rest := keys([]byte(in)); strings.Join(got, ",") != strings.Join(want, ",") || rest != nil {
		t.Errorf("keys = %q, %q; want %q", got, rest, want)
	}
}

// TestKeysKeepCutOffInput: what a read cut off mid key comes back as rest.
func TestKeysKeepCutOffInput(t *testing.T) {
	for in, want := range map[string]struct{ keys, rest string }{
		"j\x1b":       {"j", "\x1b"},
		"j\x1b[":      {"j", "\x1b["},
		"\x1bO":       {"", "\x1bO"},
		"\x1b[1;5":    {"", "\x1b[1;5"},
		"q\xc3":       {"q", "\xc3"}, // the first byte of é
		"\x1bj\x1b[B": {"esc,j,down", ""},
	} {
		got, rest := keys([]byte(in))
		if strings.Join(got, ",") != want.keys || string(rest) != want.rest {
			t.Errorf("keys(%q) = %q, %q; want %q, %q", in, got, rest, want.keys, want.rest)
		}
	}
}

// waits makes e hand the test each wait it starts for the rest of a key, so
// the test decides whether the rest comes first or the wait runs out.
func waits(e *editor) chan chan time.Time {
	c := make(chan chan time.Time, 8)
	e.after = func(time.Duration) <-chan time.Time {
		w := make(chan time.Time, 1)
		c <- w
		return w
	}
	return c
}

// start runs e on keys typed into the returned pipe, drawing into frames.
// The pipe closes when the editor ends, so typing into an ended editor fails.
func start(e *editor, drawn io.Writer, stop chan os.Signal) (*io.PipeWriter, chan error) {
	in, typing := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- e.run(in, drawn, func() (int, int) { return 80, 40 }, nil, stop)
		in.Close()
	}()
	return typing, done
}

func TestEditorJoinsKeysSplitAcrossReads(t *testing.T) {
	e := newEditor(t)
	waits(e) // never run out: the rest of each key comes first
	typing, done := start(e, io.Discard, nil)
	for _, chunk := range []string{"\x1b[", "B", "\x1b", "[B", "\x1bO", "B", "\x1b[", "1;5", "A", "q"} {
		if _, err := typing.Write([]byte(chunk)); err != nil {
			t.Fatalf("editor ended before %q (cursor %d): %v", chunk, e.cursor, err)
		}
	}
	if err := <-done; err != nil || e.cursor != 3 {
		t.Errorf("err %v, cursor %d; want three downs and an ignored ctrl-up", err, e.cursor)
	}
}

func TestEditorEscapeAloneQuitsOnceTheWaitRunsOut(t *testing.T) {
	e := newEditor(t)
	w := waits(e)
	drawn := make(frames)
	typing, done := start(e, drawn, nil)
	<-drawn
	if _, err := typing.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	<-drawn              // the ESC is read and waiting
	(<-w) <- time.Time{} // and nothing follows it
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEditorDropsACutOffSequenceOnceTheWaitRunsOut(t *testing.T) {
	e := newEditor(t)
	w := waits(e)
	drawn := make(frames)
	typing, done := start(e, drawn, nil)
	<-drawn
	if _, err := typing.Write([]byte("\x1b[")); err != nil {
		t.Fatal(err)
	}
	<-drawn
	(<-w) <- time.Time{}
	<-drawn // the cut-off sequence is dropped without ending the editor
	if _, err := typing.Write([]byte("j q")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || e.cursor != 1 || e.set.On(CodeMap) {
		t.Errorf("err %v, cursor %d, code-map on %v; want j, space and q applied", err, e.cursor, e.set.On(CodeMap))
	}
}

func TestEditorEndsOnAStopSignal(t *testing.T) {
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	typing, done := start(newEditor(t), io.Discard, stop)
	defer typing.Close()
	if err := <-done; err == nil || err.Error() != "terminated" {
		t.Errorf("err = %v, want terminated", err)
	}
}

func TestEditorTogglesAndSavesAtOnce(t *testing.T) {
	e := newEditor(t)
	// down twice to recent-bugs, toggle it off; end to auto-repair, toggle off,
	// then back on; quit.
	in := strings.NewReader("jj \x1b[F x\x1b[Aq")
	var out strings.Builder
	if err := e.run(in, &out, func() (int, int) { return 80, 40 }, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Read(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if s.On(RecentBugs) || !s.On(AutoRepair) || !s.On(Sync) {
		t.Errorf("saved %v", s)
	}
	if e.cursor != len(all)-2 || e.status != "Saved auto-repair = true" {
		t.Errorf("cursor %d, status %q", e.cursor, e.status)
	}
}

func TestEditorAllOn(t *testing.T) {
	e := newEditor(t)
	e.handle(" ")
	e.handle("j")
	e.handle(" ")
	if e.set.On(SessionMemories) || e.set.On(CodeMap) {
		t.Fatal("toggles did not apply")
	}
	e.handle("a")
	s, _ := Read(e.path)
	for _, f := range all {
		if !s.On(f) || !e.set.On(f) {
			t.Errorf("%s still off", f.Key)
		}
	}
	e.handle("a")
	if e.status != "All features are on" || e.failed {
		t.Errorf("status %q", e.status)
	}
}

func TestEditorQuits(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl-c"} {
		if !newEditor(t).handle(k) {
			t.Errorf("%s does not quit", k)
		}
	}
	// End of input ends the editor too.
	if err := newEditor(t).run(strings.NewReader("j"), &strings.Builder{}, func() (int, int) { return 80, 24 }, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// TestEditorListsEveryFeature: a feature declared in the registry shows up in
// the editor with no other change.
func TestEditorListsEveryFeature(t *testing.T) {
	e := newEditor(t)
	e.set[Todos] = false
	text := strings.Join(screen(e, 120, 100), "\n")
	for _, f := range all {
		box := "[x]"
		if f == Todos {
			box = "[ ]"
		}
		if !regexp.MustCompile(regexp.QuoteMeta(box)+` +`+regexp.QuoteMeta(f.Key)+` +`+regexp.QuoteMeta(f.Desc)).MatchString(text) ||
			!strings.Contains(text, "\n "+f.Group+"\n") {
			t.Errorf("%s (%s) missing:\n%s", f.Key, f.Group, text)
		}
	}
}

func TestEditorKeepsCursorInSight(t *testing.T) {
	e := newEditor(t)
	const h = 12
	for i := range all {
		e.cursor = i
		lines := screen(e, 80, h)
		if len(lines) > h {
			t.Fatalf("%d lines for a %d-line terminal", len(lines), h)
		}
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, "› [x] "+all[i].Key) {
			t.Fatalf("cursor on %s is off screen:\n%s", all[i].Key, text)
		}
		if i == 0 || all[i-1].Group != all[i].Group {
			if !strings.Contains(text, " "+all[i].Group+"\n") {
				t.Errorf("heading of %s off screen:\n%s", all[i].Key, text)
			}
		}
	}
	// Back at the top the title block and first heading show again.
	e.cursor = 0
	if lines := screen(e, 80, h); !strings.HasPrefix(lines[0], " Recarto features") || lines[3] != " Session start" {
		t.Errorf("top:\n%s", strings.Join(lines, "\n"))
	}
}

// frames receives each screen the editor draws.
type frames chan string

func (f frames) Write(p []byte) (int, error) { f <- string(p); return len(p), nil }

func TestEditorRedrawsOnResize(t *testing.T) {
	e := newEditor(t)
	var width atomic.Int64
	width.Store(100)
	resized := make(chan os.Signal, 1)
	in, typing := io.Pipe()
	defer typing.Close()
	drawn := make(frames)
	done := make(chan error)
	go func() { done <- e.run(in, drawn, func() (int, int) { return int(width.Load()), 40 }, resized, nil) }()
	before := <-drawn
	width.Store(30)
	resized <- os.Interrupt // any signal: run only needs to hear of the resize
	after := <-drawn
	go func() { _, _ = typing.Write([]byte("q")) }()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	title := func(frame string) string {
		return strings.SplitN(ansi.ReplaceAllString(frame, ""), "\r\n", 2)[0]
	}
	if !strings.HasSuffix(title(before), "config.toml") || len([]rune(title(after))) != 29 {
		t.Errorf("no redraw for the new width:\nbefore %q\nafter  %q", title(before), title(after))
	}
}

func TestEditorShowsPositionWhenTheListDoesNotFit(t *testing.T) {
	e := newEditor(t)
	e.cursor = 4
	if text := strings.Join(screen(e, 80, 12), "\n"); !strings.HasSuffix(text, fmt.Sprintf("q quit  5/%d", len(all))) {
		t.Errorf("short terminal, no position:\n%s", text)
	}
	if text := strings.Join(screen(e, 80, 60), "\n"); !strings.HasSuffix(text, "q quit") {
		t.Errorf("everything fits, position shown:\n%s", text)
	}
}

func TestEditorFitsNarrowTerminals(t *testing.T) {
	e := newEditor(t)
	e.status = strings.Repeat("long status ", 20)
	for _, w := range []int{20, 40, 80} {
		for _, l := range screen(e, w, 50) {
			if n := len([]rune(l)); n > w-1 {
				t.Errorf("width %d: %d columns: %q", w, n, l)
			}
		}
	}
}

func TestEditorShowsASaveErrorAndKeepsTheState(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e := &editor{path: filepath.Join(blocker, "config.toml"), set: Set{}} // its parent is a file
	e.handle(" ")
	if !e.failed || !e.set.On(SessionMemories) {
		t.Fatalf("failed=%v on=%v status=%q", e.failed, e.set.On(SessionMemories), e.status)
	}
	if text := strings.Join(screen(e, 200, 50), "\n"); !strings.Contains(text, e.status) {
		t.Errorf("error not shown:\n%s", text)
	}
}

func TestEditorWithoutColor(t *testing.T) {
	e := newEditor(t)
	e.color = false
	if v := e.view(80, 40); strings.Contains(v, "\x1b[1m") || strings.Contains(v, "\x1b[32m") {
		t.Error("styled output with color off")
	}
}
