// Package agents holds Recarto's agent-neutral lifecycle-hook engine. Host
// adapters (claudecode, opencode, copilot) translate each tool's payloads to and
// from the neutral Event/Result types; Handle runs the shared logic in between,
// and the per-host installers register the MCP server, hooks, and instructions.
package agents

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
)

// EventKind enumerates the lifecycle events Recarto reacts to.
type EventKind int

const (
	KindSessionStart EventKind = iota
	KindPostRead
	KindPostEdit
	KindAugment
	KindStop
	KindShell
)

// Event is the agent-neutral hook input. Adapters populate the fields relevant to
// the Kind; unused fields stay zero.
type Event struct {
	Kind      EventKind
	Agent     string // sessions.agent: "claude-code" | "opencode" | "copilot"
	SessionID string
	// AgentID names the conversation within the session the hook fired in when
	// the host runs several under one session id (a Claude Code subagent's
	// agent_id); "" for the session's main conversation.
	AgentID    string
	Source     string
	FilePath   string // absolute path (PostRead/PostEdit)
	Pattern    string // grep/glob query (Augment)
	ToolResult string // raw tool result text (PostRead token estimate)
	StopActive bool
	Command    string // shell command line (Shell)
	CWD        string // the shell's working directory (Shell)
	// Trigger is the ShellCommands key a per-command host hook was started for
	// ("" when the host starts one hook for every shell call). Only the hook of
	// the line's first such command acts, so a line matching several starts
	// one hint, not one per match.
	Trigger string
	// Patterns and Files are Command as RunHook read it: the searches it runs
	// and the absolute paths of the files it prints (Shell).
	Patterns []string
	Files    []string
	// ReadRanged reports that the agent asked a read for a line range.
	// ReadFirst..ReadLast are the 1-based lines the read returned; both are 0
	// when the host did not say (PostRead).
	ReadRanged          bool
	ReadFirst, ReadLast int
}

// Result is the agent-neutral hook output. Context is injected as the host's
// "additional context"; SystemMessage is shown to the human. Block asks the
// host to refuse this Stop once and feed Reason back to the model as an
// instruction — the only Stop channel an LLM acts on. Adapters without a block
// channel ignore Block/Reason.
type Result struct {
	Context       string
	SystemMessage string
	Block         bool
	Reason        string
}

// Adapter converts a host's hook payloads to and from the neutral types.
type Adapter interface {
	Decode(kind EventKind, in io.Reader) (Event, error)
	Encode(kind EventKind, out io.Writer, res Result) error
}

// KindFromString maps a CLI event token to an EventKind.
func KindFromString(s string) (EventKind, error) {
	switch s {
	case "session-start":
		return KindSessionStart, nil
	case "post-read":
		return KindPostRead, nil
	case "post-edit":
		return KindPostEdit, nil
	case "augment":
		return KindAugment, nil
	case "stop":
		return KindStop, nil
	case "shell":
		return KindShell, nil
	}
	return 0, fmt.Errorf("unknown event %q (session-start|post-read|post-edit|augment|stop|shell)", s)
}

// RunHook decodes the event with ad, runs the shared Handle against the App
// open returns, then encodes the result back through ad. Hooks fail open: an
// open or Handle error returns before any output — even advisory context built
// before a failing side effect is dropped — for the (non-blocking) caller to
// log. open is called only for an event with something to act on: an idle one,
// or one whose features are all off, encodes the empty result without
// resolving git or opening a store. open may itself find the event idle (a nil
// App, e.g. a post-edit outside every served repo).
func RunHook(ctx context.Context, open func(Event) (*app.App, error), ad Adapter, kind EventKind, in io.Reader, out io.Writer) error {
	ev, err := ad.Decode(kind, in)
	if err != nil {
		return err
	}
	if ev.Kind == KindShell {
		if p := parseShell(ev.Command, ev.CWD); ev.Trigger == "" || ev.Trigger == p.owner {
			ev.Patterns, ev.Files = p.patterns, p.files
		}
	}
	if idle(ev) || !wanted(ev, features.Load()) {
		return ad.Encode(kind, out, Result{})
	}
	a, err := open(ev)
	if err != nil {
		return err
	}
	if a == nil {
		return ad.Encode(kind, out, Result{})
	}
	res, err := Handle(ctx, a, ev)
	if err != nil {
		return err
	}
	return ad.Encode(kind, out, res)
}

// Opened adapts an already-open App to RunHook's opener.
func Opened(a *app.App) func(Event) (*app.App, error) {
	return func(Event) (*app.App, error) { return a, nil }
}

// wanted reports whether f leaves on a feature of ev's hook. Handle checks each
// feature itself; this only spares a hook with none on from opening a store.
func wanted(ev Event, f features.Set) bool {
	switch ev.Kind {
	case KindPostRead:
		return f.On(features.RepeatedReads) || f.On(features.LargeReads) ||
			f.On(features.BugWarnings) || f.On(features.Guidance) || f.On(features.Todos)
	case KindAugment:
		return f.On(features.SearchHints)
	case KindShell:
		return f.On(features.ShellHints)
	}
	return true
}

// idle reports whether ev has nothing to act on, judged from the payload alone.
// It mirrors the handlers' own early returns, so skipping Handle for it changes
// neither output nor ledger. A Grep/Glob pattern that names no identifier
// (a stemless glob like "*.cs", a path, regex syntax alone) is idle too: it
// gets no hint and never touches the ledger. A shell line is idle unless it searches or prints a
// file: builds, tests, git and writes never reach the store.
func idle(ev Event) bool {
	switch ev.Kind {
	case KindPostRead:
		return ev.FilePath == ""
	case KindPostEdit:
		return !filepath.IsAbs(ev.FilePath) // owningRepo rejects relative paths
	case KindAugment:
		return len(searchIdentifiers(ev.Pattern)) == 0
	case KindStop:
		return ev.SessionID == ""
	case KindShell:
		return len(ev.Patterns) == 0 && len(ev.Files) == 0
	}
	return false
}
