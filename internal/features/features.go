// Package features holds Recarto's feature switches: the registry of every
// feature a user can turn off, and the config file recording their choice. A
// feature is on unless the file sets it to false, so a missing file or key
// leaves everything on.
package features

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Feature is one switch in the config file.
type Feature struct {
	Key   string // its name in the config file and on the command line
	Group string // the heading it is listed under
	Desc  string // what it does, in one line
}

var all []*Feature

// add registers a feature. Declaring one below is all it takes for it to show
// up in `recarto config`, its list, the config file and its validation.
func add(group, key, desc string) *Feature {
	f := &Feature{Key: key, Group: group, Desc: desc}
	all = append(all, f)
	return f
}

const (
	sessionStart = "Session start"
	whileWorking = "While working"
	endOfTurn    = "End of turn"
	agentTools   = "Agent tools"
	memoryStore  = "Memory store"
)

// The features, in display order.
var (
	SessionMemories = add(sessionStart, "session-memories", "Inject the most relevant memories for this branch")
	CodeMap         = add(sessionStart, "code-map", "Add a map of the codebase's modules")
	RecentBugs      = add(sessionStart, "recent-bugs", "List recently recorded bugs")
	RecentChanges   = add(sessionStart, "recent-changes", "Summarize code changes since your last session")

	BugWarnings   = add(whileWorking, "bug-warnings", "Warn about known bugs in a file you read or edit")
	Guidance      = add(whileWorking, "guidance", "Show a file's do-not-repeats, decisions and learnings")
	Todos         = add(whileWorking, "todos", "Track open work as todos that resurface until done")
	RepeatedReads = add(whileWorking, "repeated-reads", "Warn when a read repeats unchanged lines already read")
	LargeReads    = add(whileWorking, "large-reads", "Suggest an outline once after a large whole-file read")
	SearchHints   = add(whileWorking, "search-hints", "Add matching symbols and bugs to Grep/Glob results")
	ShellHints    = add(whileWorking, "shell-hints", "Hint on grep, find, cat and other shell commands")

	StopReminder = add(endOfTurn, "stop-reminder", "Remind to record learnings when the agent stops")
	StopNudge    = add(endOfTurn, "stop-nudge", "Ask the agent once to record learnings after edits")

	MemoryTools  = add(agentTools, "memory-tools", "MCP tools to record, recall and search memories")
	GraphTools   = add(agentTools, "graph-tools", "MCP tools to search and navigate the code graph")
	IssueReports = add(agentTools, "issue-reports", "Let the agent report problems with Recarto itself")

	SemanticRecall = add(memoryStore, "semantic-recall", "Rank memories by meaning, not only by keywords")
	TeamSnapshot   = add(memoryStore, "team-snapshot", "Import the team snapshot (.recarto/memory.jsonl)")
	AutoCompact    = add(memoryStore, "auto-compact", "Prune and decay stale memories automatically")
	Sync           = add(memoryStore, "sync", "Sync memories with RECARTO_SYNC_URL when it is set")
	AutoRepair     = add(memoryStore, "auto-repair", "Rebuild a corrupt store, keeping your memories")
)

// All returns every feature in display order.
func All() []*Feature { return all }

// Lookup returns the feature named key, or an error suggesting the one meant.
func Lookup(key string) (*Feature, error) {
	for _, f := range all {
		if f.Key == key {
			return f, nil
		}
	}
	for _, f := range all {
		if key != "" && (strings.Contains(f.Key, key) || strings.Contains(key, f.Key)) {
			return nil, fmt.Errorf("unknown feature %q (did you mean %s?)", key, f.Key)
		}
	}
	return nil, fmt.Errorf("unknown feature %q (see recarto config list)", key)
}

// Set records which features are on. A feature it does not mention is on, so
// a nil Set has every feature on.
type Set map[*Feature]bool

// On reports whether f is on.
func (s Set) On(f *Feature) bool {
	on, ok := s[f]
	return on || !ok
}

// Path returns the config file: $RECARTO_CONFIG, else recarto/config.toml in
// the user's config directory.
func Path() (string, error) {
	if p := os.Getenv("RECARTO_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "recarto", "config.toml"), nil
}

// Load returns what the config file sets, for hooks and the MCP server: a
// missing or unreadable file, or a bad line, leaves those features on.
func Load() Set {
	p, err := Path()
	if err != nil {
		return nil
	}
	s, _ := Read(p)
	return s
}

// Read returns the features the file at path sets, and every problem in it as
// "path:line: message". A missing file sets nothing.
func Read(path string) (Set, error) {
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Set{}, nil
	}
	if err != nil {
		return Set{}, err
	}
	s, _, err := parse(path, strings.TrimPrefix(string(src), "\ufeff"))
	return s, err
}

// parse reads `feature = true|false` lines, skipping blank lines and # comments.
// It returns the features set, the line index of each, and every problem.
func parse(path, src string) (Set, map[*Feature]int, error) {
	s, at := Set{}, map[*Feature]int{}
	var errs []error
	for i, line := range strings.Split(src, "\n") {
		line, _, _ = strings.Cut(line, "#")
		key, val, ok := strings.Cut(line, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok && key == "" {
			continue
		}
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s:%d: %s", path, i+1, fmt.Sprintf(format, args...)))
		}
		f, err := Lookup(key)
		_, dup := at[f]
		switch {
		case !ok || key == "":
			bad("want <feature> = true or false")
		case err != nil:
			bad("%v", err)
		case val != "true" && val != "false":
			bad("%s must be true or false, not %q", key, val)
		case dup:
			bad("%s is set twice", key)
		default:
			s[f], at[f] = val == "true", i
		}
	}
	return s, at, errors.Join(errs...)
}

// Save switches the features in changes in the file at path. A new file lists
// every feature, grouped and described. An existing one keeps its comments and
// layout: only the changed values are rewritten, and a changed feature it does
// not list yet is appended. A file with problems is left alone rather than
// lose the lines that do not parse.
func Save(path string, changes Set) error {
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return write(path, template(changes))
	}
	if err != nil {
		return err
	}
	text := strings.TrimPrefix(string(src), "\ufeff")
	_, at, err := parse(path, text)
	if err != nil {
		return err
	}
	lines := strings.Split(text, "\n")
	eol := "" // keeps a CRLF file CRLF
	if strings.Contains(text, "\r\n") {
		eol = "\r"
	}
	var added []string
	for _, f := range all {
		on, ok := changes[f]
		if !ok {
			continue
		}
		if i, ok := at[f]; ok {
			lines[i] = setValue(lines[i], on)
		} else {
			added = append(added, entry(f, on)+eol)
		}
	}
	if len(added) > 0 {
		if lines[len(lines)-1] == "" {
			lines, added = lines[:len(lines)-1], append(added, "")
		}
		lines = append(lines, added...)
	}
	return write(path, strings.Join(lines, "\n"))
}

// setValue rewrites the true/false after a line's "=", keeping the rest of the
// line; a trailing comment keeps its column when the padding allows.
func setValue(line string, on bool) string {
	after := line[strings.IndexByte(line, '=')+1:]
	start := len(line) - len(strings.TrimLeft(after, " \t"))
	end := start + len("false")
	if strings.HasPrefix(line[start:], "true") {
		end = start + len("true")
	}
	val, rest := strconv.FormatBool(on), line[end:]
	switch {
	case len(val) > end-start && strings.HasPrefix(rest, "  "):
		rest = rest[1:]
	case len(val) < end-start && strings.HasPrefix(rest, " "):
		rest = " " + rest
	}
	return line[:start] + val + rest
}

// template renders a config file listing every feature, grouped, as set in s.
func template(s Set) string {
	var b strings.Builder
	b.WriteString("# Recarto features: true is on, false is off. A feature left out is on.\n")
	b.WriteString("# Edit this file, or run `recarto config` to switch them interactively.\n")
	group := ""
	for _, f := range all {
		if f.Group != group {
			group = f.Group
			fmt.Fprintf(&b, "\n# %s\n", group)
		}
		b.WriteString(entry(f, s.On(f)) + "\n")
	}
	return b.String()
}

// entry is f's line in the config file, with its description in a column.
func entry(f *Feature, on bool) string {
	return fmt.Sprintf("%-*s # %s", keyWidth()+len(" = false"), f.Key+" = "+strconv.FormatBool(on), f.Desc)
}

// keyWidth is the length of the longest feature name.
func keyWidth() int {
	n := 0
	for _, f := range all {
		n = max(n, len(f.Key))
	}
	return n
}

// write replaces the file at path in one rename, so a hook reading it never
// sees half a file.
func write(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
