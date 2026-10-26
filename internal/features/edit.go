package features

import (
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Edit runs the interactive feature list on the terminal in/out, saving the
// file at path on every change, and prints what changed once the user quits.
func Edit(path string, in, out *os.File) error {
	s, err := Read(path)
	if err != nil {
		return err
	}
	start := maps.Clone(s)
	e := &editor{path: path, set: s, color: os.Getenv("NO_COLOR") == ""}
	if err := e.onTerminal(in, out); err != nil {
		return err
	}
	changed := false
	for _, f := range all {
		if on := e.set.On(f); on != start.On(f) {
			fmt.Fprintf(out, "%s: %s\n", f.Key, OnOff(on))
			changed = true
		}
	}
	if changed {
		fmt.Fprintf(out, "Saved to %s\n", path)
	}
	return nil
}

// OnOff names a feature state.
func OnOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// onTerminal runs the editor full screen in raw mode, restoring the terminal
// however it ends.
func (e *editor) onTerminal(in, out *os.File) error {
	fd := int(in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	defer enableVT(out)()
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // alternate screen, hidden cursor
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
	resized := make(chan os.Signal, 1)
	notifyResize(resized)
	defer signal.Stop(resized)
	return e.run(in, out, func() (int, int) {
		w, h, err := term.GetSize(int(out.Fd()))
		if err != nil {
			return 80, 24
		}
		return w, h
	}, resized)
}

// editor is the state of the interactive feature list.
type editor struct {
	path   string
	set    Set
	cursor int    // index into all
	top    int    // first list line on screen
	status string // outcome of the last change
	failed bool   // status is an error
	color  bool
}

// run draws the editor and applies the keys read from in until the user
// quits or in ends, redrawing too when the terminal is resized.
func (e *editor) run(in io.Reader, out io.Writer, size func() (int, int), resized <-chan os.Signal) error {
	type input struct {
		keys []string
		err  error
	}
	inputs := make(chan input, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := in.Read(buf)
			inputs <- input{keys(buf[:n]), err}
			if err != nil {
				return
			}
		}
	}()
	for {
		if _, err := io.WriteString(out, e.view(size())); err != nil {
			return err
		}
		select {
		case <-resized:
		case in := <-inputs:
			for _, k := range in.keys {
				if e.handle(k) {
					return nil
				}
			}
			if in.err == io.EOF {
				return nil
			}
			if in.err != nil {
				return in.err
			}
		}
	}
}

// handle applies one key and reports whether the editor should close.
func (e *editor) handle(k string) bool {
	switch k {
	case "q", "esc", "ctrl-c":
		return true
	case "up", "k":
		e.cursor = max(e.cursor-1, 0)
	case "down", "j":
		e.cursor = min(e.cursor+1, len(all)-1)
	case "home", "g":
		e.cursor = 0
	case "end", "G":
		e.cursor = len(all) - 1
	case " ", "enter", "x":
		f := all[e.cursor]
		e.save(Set{f: !e.set.On(f)})
	case "a":
		changes := Set{}
		for _, f := range all {
			if !e.set.On(f) {
				changes[f] = true
			}
		}
		e.save(changes)
	}
	return false
}

// save writes changes to the file, then to the editor's state.
func (e *editor) save(changes Set) {
	e.status, e.failed = "All features are on", false
	if len(changes) == 0 {
		return
	}
	if err := Save(e.path, changes); err != nil {
		e.status, e.failed = strings.ReplaceAll(err.Error(), "\n", "; "), true
		return
	}
	var saved []string
	for _, f := range all {
		if on, ok := changes[f]; ok {
			e.set[f] = on
			saved = append(saved, fmt.Sprintf("%s = %t", f.Key, on))
		}
	}
	e.status = "Saved " + strings.Join(saved, ", ")
}

// SGR styles.
const (
	bold  = "1"
	dim   = "2"
	red   = "31"
	green = "32"
	cyan  = "36"
)

// seg is a run of text in one style ("" for plain).
type seg struct{ style, text string }

// view renders the editor for a w×h terminal: the title, the grouped feature
// list scrolled to keep the cursor in sight, then the status and key help.
func (e *editor) view(w, h int) string {
	var list []string
	cur := 0 // the cursor's line in list
	for i, f := range all {
		if i == 0 || f.Group != all[i-1].Group {
			if i > 0 {
				list = append(list, "")
			}
			list = append(list, e.line(w, seg{bold, " " + f.Group}))
		}
		if i == e.cursor {
			cur = len(list)
		}
		list = append(list, e.row(w, f, i == e.cursor))
	}
	rows := max(h-6, 1) // the title block and the footer take three lines each
	first := cur        // keep a group's heading in sight with its first feature
	if e.cursor == 0 || all[e.cursor-1].Group != all[e.cursor].Group {
		first = cur - 1
	}
	e.top = max(min(e.top, first), cur-rows+1)
	e.top = max(min(e.top, len(list)-rows), 0)

	var b strings.Builder
	b.WriteString("\x1b[H")
	put := func(s string) { b.WriteString(s + "\x1b[K\r\n") }
	put(e.line(w, seg{bold, " Recarto features"}, seg{dim, "  " + e.path}))
	put(e.line(w, seg{dim, " Changes are saved at once and apply to new agent sessions."}))
	put("")
	for _, l := range list[e.top:min(e.top+rows, len(list))] {
		put(l)
	}
	put("")
	status := seg{green, " " + e.status}
	if e.failed {
		status.style = red
	}
	put(e.line(w, status))
	help := []seg{{bold, " ↑/↓"}, {dim, " move  "}, {bold, "space"}, {dim, " toggle  "}, {bold, "a"}, {dim, " all on  "}, {bold, "q"}, {dim, " quit"}}
	if len(list) > rows { // part of the list is off screen
		help = append(help, seg{dim, fmt.Sprintf("  %d/%d", e.cursor+1, len(all))})
	}
	b.WriteString(e.line(w, help...))
	b.WriteString("\x1b[K\x1b[J")
	return b.String()
}

// row renders one feature: cursor mark, checkbox, name and description.
func (e *editor) row(w int, f *Feature, at bool) string {
	mark, box, name, desc := seg{"", "   "}, seg{dim, "[ ]"}, seg{dim, ""}, seg{dim, f.Desc}
	if e.set.On(f) {
		box, name.style = seg{green, "[x]"}, ""
	}
	if at {
		mark, name.style, desc.style = seg{cyan, " › "}, bold, ""
	}
	name.text = fmt.Sprintf(" %-*s  ", keyWidth(), f.Key)
	return e.line(w, mark, box, name, desc)
}

// line renders segs as one screen line, cut to w-1 columns so that no
// terminal wraps it.
func (e *editor) line(w int, segs ...seg) string {
	var b strings.Builder
	room := w - 1
	for _, s := range segs {
		r := []rune(s.text)
		if len(r) > room {
			r = r[:max(room, 0)]
			if len(r) > 0 {
				r[len(r)-1] = '…'
			}
		}
		room -= len(r)
		switch {
		case len(r) == 0:
		case e.color && s.style != "":
			b.WriteString("\x1b[" + s.style + "m" + string(r) + "\x1b[0m")
		default:
			b.WriteString(string(r))
		}
	}
	return b.String()
}

// escKeys names the escape sequences (after "ESC [" or "ESC O") of the keys
// the editor uses.
var escKeys = map[string]string{
	"A": "up", "B": "down", "H": "home", "F": "end",
	"1~": "home", "7~": "home", "4~": "end", "8~": "end",
}

// keys splits raw terminal input into key names: "up", "down", "home", "end",
// "enter", "esc", "ctrl-c", or the typed character itself.
func keys(b []byte) []string {
	var out []string
	for len(b) > 0 {
		k, n := "", 1
		switch c := b[0]; {
		case c == 0x1b && len(b) > 1 && (b[1] == '[' || b[1] == 'O'):
			n = 2
			for n < len(b) && (b[n] < 0x40 || b[n] > 0x7e) {
				n++
			}
			if n == len(b) {
				return out // a sequence cut off by the read
			}
			n++
			k = escKeys[string(b[2:n])]
		case c == 0x1b:
			k = "esc"
		case c == '\r' || c == '\n':
			k = "enter"
		case c == 3:
			k = "ctrl-c"
		default:
			var r rune
			r, n = utf8.DecodeRune(b)
			k = string(r)
		}
		if k != "" {
			out = append(out, k)
		}
		b = b[n:]
	}
	return out
}
