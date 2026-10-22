package agents

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ShellCommands are the commands Recarto reads in a shell tool call: content
// searches (grep, egrep, fgrep, rg, ugrep, git grep), file-name searches (find,
// bfs) and file views (cat, head, tail, sed -n). "git" stands for `git grep`
// only. A host that filters hooks per command (Claude Code's `if`) starts one
// handler per key and passes the key as the hook's --cmd.
var ShellCommands = []string{"grep", "egrep", "fgrep", "rg", "ugrep", "git", "find", "bfs", "cat", "head", "tail", "sed"}

// goos indirects runtime.GOOS so tests can exercise Git Bash path handling
// without running on Windows.
var goos = runtime.GOOS

// shellPlan is what a shell command line means to Recarto.
type shellPlan struct {
	owner    string   // the first ShellCommands key the line runs ("" when none)
	patterns []string // search patterns: content regexes and file-name globs
	files    []string // absolute paths of the files the line prints
}

// parseShell reads a POSIX shell command line run in cwd ("" when unknown). A
// line that does not parse yields the empty plan, so Recarto stays silent.
func parseShell(command, cwd string) shellPlan {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return shellPlan{}
	}
	w := &shellWalker{}
	w.stmts(f.Stmts, cwd, false, true)
	return w.plan
}

type shellWalker struct{ plan shellPlan }

// stmts walks a statement list in order, threading the cwd a `cd` changes.
// piped marks a statement whose input comes from a pipe; printed one whose
// output reaches the agent rather than a file.
func (w *shellWalker) stmts(list []*syntax.Stmt, cwd string, piped, printed bool) string {
	for _, s := range list {
		cwd = w.stmt(s, cwd, piped, printed)
	}
	return cwd
}

// stmt walks one statement and returns the cwd after it. Pipeline parts and
// subshells run in their own process, so their `cd` does not leak out. A
// redirect on a group, subshell or pipeline's last stage sends every command
// inside it (or upstream of it) to the file.
func (w *shellWalker) stmt(s *syntax.Stmt, cwd string, piped, printed bool) string {
	printed = printed && !stdoutToFile(s.Redirs)
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		return w.call(c, cwd, piped, printed)
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			w.stmt(c.X, cwd, piped, printed && pipeOutPrinted(c.Y))
			w.stmt(c.Y, cwd, true, printed)
			return cwd
		}
		return w.stmt(c.Y, w.stmt(c.X, cwd, piped, printed), piped, printed)
	case *syntax.Subshell:
		w.stmts(c.Stmts, cwd, piped, printed)
	case *syntax.Block:
		return w.stmts(c.Stmts, cwd, piped, printed)
	}
	return cwd
}

// pipeOutPrinted reports whether a pipeline stage's output, after any later
// stages, reaches the agent rather than a file.
func pipeOutPrinted(s *syntax.Stmt) bool {
	if stdoutToFile(s.Redirs) {
		return false
	}
	if c, ok := s.Cmd.(*syntax.BinaryCmd); ok && (c.Op == syntax.Pipe || c.Op == syntax.PipeAll) {
		return pipeOutPrinted(c.Y)
	}
	return true
}

// call reads one simple command. One whose output goes to a file is a write,
// not a search or view: it still owns the line but adds nothing.
func (w *shellWalker) call(c *syntax.CallExpr, cwd string, piped, printed bool) string {
	args := make([]shellWord, len(c.Args))
	for i, a := range c.Args {
		args[i] = wordOf(a)
	}
	if len(args) == 0 || args[0].dyn {
		return cwd
	}
	key, args := args[0].val, args[1:]
	if key == "cd" {
		return cd(args, cwd)
	}
	if key == "git" {
		if len(args) == 0 || args[0].val != "grep" {
			return cwd
		}
		args = args[1:]
	}
	if !slices.Contains(ShellCommands, key) {
		return cwd
	}
	if w.plan.owner == "" {
		w.plan.owner = key
	}
	if !printed {
		return cwd
	}
	switch key {
	case "find", "bfs":
		for i := 0; i+1 < len(args); i++ {
			if a := args[i].val; a == "-name" || a == "-iname" || a == "-path" {
				i++
				w.pattern(args[i])
			}
		}
	case "cat":
		w.views(options(args, "", nil, nil), cwd)
	case "head", "tail":
		w.views(options(args, "ncs", headLong, nil), cwd)
	case "sed":
		quiet, script, inPlace := false, false, false
		ops := options(args, "efl", sedLong, func(name string, _ shellWord) {
			switch name {
			case "n", "quiet", "silent":
				quiet = true
			case "e", "expression", "f", "file":
				script = true
			case "i", "in-place":
				inPlace = true
			}
		})
		if !script && len(ops) > 0 {
			ops = ops[1:] // the script
		}
		if quiet && !inPlace {
			w.views(ops, cwd)
		}
	default:
		w.search(key, args, cwd, piped)
	}
	return cwd
}

// search reads a grep-family content search. Operands that are directories or
// globs make it a search; a search over regular files only is a view of them.
// With no file operand it searches only when the tool searches by default:
// git grep always, rg and grep -r unless they read piped input.
func (w *shellWalker) search(key string, args []shellWord, cwd string, piped bool) {
	short, long := grepShort, grepLong
	switch key {
	case "rg":
		short, long = rgShort, rgLong
	case "ugrep":
		short = ugrepShort
	}
	var pats []shellWord
	patFile, recursive, listing := false, false, false
	ops := options(args, short, long, func(name string, v shellWord) {
		switch name {
		case "e", "regexp":
			pats = append(pats, v)
		case "f", "file":
			patFile = true
		case "r", "R", "recursive", "dereference-recursive":
			recursive = true // rg's -r is --replace, but rg recurses anyway
		case "d", "directories":
			recursive = recursive || v.val == "recurse"
		case "files":
			listing = key == "rg"
		}
	})
	if listing {
		return
	}
	if len(pats) == 0 && !patFile {
		if len(ops) == 0 {
			return
		}
		pats, ops = ops[:1], ops[1:]
	}
	var files []shellWord
	scoped := false
	for _, o := range ops {
		if o.dyn {
			continue
		}
		if o.glob {
			scoped = true
			continue
		}
		if fi, err := os.Stat(shellPath(o.val, cwd)); err == nil {
			if fi.IsDir() {
				scoped = true
			} else if fi.Mode().IsRegular() {
				files = append(files, o)
			}
		}
	}
	if scoped || len(files) == 0 && (key == "git" || !piped && (key == "rg" || recursive)) {
		for _, p := range pats {
			w.pattern(p)
		}
	}
	w.views(files, cwd)
}

// grepShort, ugrepShort and rgShort are the short options that take a value;
// grepLong the long ones across the grep family. rg alone takes --color's
// value as a separate word (grep's --color only binds an =value).
var (
	grepShort     = "efmABCdD"
	ugrepShort    = grepShort + "gOtMKNJ"
	rgShort       = "efgtTmABCMdjEr"
	grepLongNames = []string{"regexp", "file", "max-count", "after-context", "before-context", "context",
		"directories", "devices", "include", "exclude", "exclude-dir", "exclude-from", "label",
		"binary-files", "group-separator", "glob", "iglob", "type", "type-not", "type-add",
		"type-clear", "max-columns", "max-depth", "threads", "encoding", "replace", "pre",
		"pre-glob", "sort", "sortr", "path-separator", "context-separator", "field-match-separator",
		"field-context-separator", "colors", "dfa-size-limit", "regex-size-limit", "ignore-file",
		"engine", "hyperlink-format", "max-filesize", "file-type"}
	grepLong = setOf(grepLongNames...)
	rgLong   = setOf(append(grepLongNames, "color")...)
	headLong = setOf("lines", "bytes", "pid", "sleep-interval", "max-unchanged-stats")
	sedLong  = setOf("expression", "file", "line-length")
)

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// options walks args the getopt way — options may follow operands, "--" ends
// them — calling opt (when non-nil) with each option's name and value, and
// returns the operands. short lists the letters whose option takes a value
// (-A 3, -A3); long the long names that do (--include X; an =value always
// binds).
func options(args []shellWord, short string, long map[string]bool, opt func(name string, val shellWord)) []shellWord {
	if opt == nil {
		opt = func(string, shellWord) {}
	}
	var ops []shellWord
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a.val == "--":
			return append(ops, args[i+1:]...)
		case strings.HasPrefix(a.val, "--"):
			name, val, ok := strings.Cut(a.val[2:], "=")
			v := shellWord{val: val, dyn: a.dyn}
			if !ok && long[name] && i+1 < len(args) {
				i++
				v = args[i]
			}
			opt(name, v)
		case len(a.val) > 1 && a.val[0] == '-' && !a.dyn:
			for j := 1; j < len(a.val); j++ {
				c := a.val[j : j+1]
				if !strings.Contains(short, c) {
					opt(c, shellWord{})
					continue
				}
				v := shellWord{val: a.val[j+1:]}
				if v.val == "" && i+1 < len(args) {
					i++
					v = args[i]
				}
				opt(c, v)
				break
			}
		default:
			ops = append(ops, a)
		}
	}
	return ops
}

// pattern records a search pattern once.
func (w *shellWalker) pattern(p shellWord) {
	if !p.dyn && p.val != "" && !slices.Contains(w.plan.patterns, p.val) {
		w.plan.patterns = append(w.plan.patterns, p.val)
	}
}

// views records the regular files among ops as printed.
func (w *shellWalker) views(ops []shellWord, cwd string) {
	for _, o := range ops {
		if o.dyn || o.glob {
			continue
		}
		p := shellPath(o.val, cwd)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && !slices.Contains(w.plan.files, p) {
			w.plan.files = append(w.plan.files, p)
		}
	}
}

// stdoutToFile reports whether a statement sends its standard output to a
// file (>, >>, >|, &>, &>>), as `cat > f <<EOF` does.
func stdoutToFile(redirs []*syntax.Redirect) bool {
	for _, r := range redirs {
		switch r.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.ClbOut, syntax.RdrAll, syntax.AppAll:
			if r.N == nil || r.N.Value == "1" {
				return true
			}
		}
	}
	return false
}

// cd returns the working directory after `cd args` in cwd: home without an
// argument, unknown ("") for `cd -` or an expanded argument.
func cd(args []shellWord, cwd string) string {
	for len(args) > 0 && len(args[0].val) > 1 && args[0].val[0] == '-' {
		args = args[1:] // -P, -L
	}
	if len(args) == 0 {
		home, _ := os.UserHomeDir()
		return home
	}
	if args[0].dyn || args[0].val == "-" {
		return ""
	}
	return shellPath(args[0].val, cwd)
}

// shellPath resolves a path argument against cwd; "" when it is relative to an
// unknown cwd.
func shellPath(p, cwd string) string {
	if goos == "windows" {
		p = gitBashPath(p)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, p)
}

// gitBashPath maps a Git Bash drive path (/c/Users/…) to its Windows form
// (C:/Users/…); any other path is returned as is.
func gitBashPath(p string) string {
	if len(p) < 2 || p[0] != '/' || p[1]|0x20 < 'a' || p[1]|0x20 > 'z' || len(p) > 2 && p[2] != '/' {
		return p
	}
	return strings.ToUpper(p[1:2]) + ":/" + strings.TrimPrefix(p[2:], "/")
}

// shellWord is a word's value after quote removal and tilde expansion. dyn
// marks a value that depends on an expansion ($VAR, $(…), `…`); glob an
// unquoted wildcard.
type shellWord struct {
	val       string
	dyn, glob bool
}

func wordOf(w *syntax.Word) shellWord {
	var out shellWord
	var b strings.Builder
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			v := unescape(p.Value, false) // before ~ expansion: a Windows home has backslashes
			if i == 0 && (p.Value == "~" || strings.HasPrefix(p.Value, "~/")) {
				home, err := os.UserHomeDir()
				out.dyn = out.dyn || err != nil
				v = home + v[1:]
			}
			out.glob = out.glob || strings.ContainsAny(p.Value, "*?[")
			b.WriteString(v)
		case *syntax.SglQuoted:
			out.dyn = out.dyn || p.Dollar // $'…' escapes are not decoded
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if lit, ok := q.(*syntax.Lit); ok {
					b.WriteString(unescape(lit.Value, true))
				} else {
					out.dyn = true
				}
			}
		default:
			out.dyn = true
		}
	}
	out.val = b.String()
	return out
}

// unescape removes the shell's backslash escapes from a literal; inside double
// quotes a backslash escapes only $ ` " \ and newline.
func unescape(s string, quoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (!quoted || strings.IndexByte("$`\"\\\n", s[i+1]) >= 0) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
