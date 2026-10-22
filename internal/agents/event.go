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

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// EventKind enumerates the lifecycle events Recarto reacts to.
type EventKind int

const (
	KindSessionStart EventKind = iota
	KindPostRead
	KindPostEdit
	KindAugment
	KindStop
)

// Event is the agent-neutral hook input. Adapters populate the fields relevant to
// the Kind; unused fields stay zero.
type Event struct {
	Kind       EventKind
	Agent      string // sessions.agent: "claude-code" | "opencode" | "copilot"
	SessionID  string
	Source     string
	FilePath   string // absolute path (PostRead/PostEdit)
	Pattern    string // grep/glob query (Augment)
	ToolResult string // raw tool result text (PostRead token estimate)
	StopActive bool
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
	}
	return 0, fmt.Errorf("unknown event %q (session-start|post-read|post-edit|augment|stop)", s)
}

// RunHook decodes the event with ad, runs the shared Handle against the App
// open returns, then encodes the result back through ad. Hooks fail open: an
// open or Handle error returns before any output — even advisory context built
// before a failing side effect is dropped — for the (non-blocking) caller to
// log. open is called only for an event with something to act on: an idle one
// encodes the empty result without resolving git or opening a store. open may
// itself find the event idle (a nil App, e.g. a post-edit outside every served
// repo).
func RunHook(ctx context.Context, open func(Event) (*app.App, error), ad Adapter, kind EventKind, in io.Reader, out io.Writer) error {
	ev, err := ad.Decode(kind, in)
	if err != nil {
		return err
	}
	if idle(ev) {
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

// idle reports whether ev has nothing to act on, judged from the payload alone.
// It mirrors the handlers' own early returns, so skipping Handle for it changes
// neither output nor ledger. A stemless file glob ("*.cs") is idle too:
// handleAugment injects nothing for it and so never marks it for the
// repeated-search dedup.
func idle(ev Event) bool {
	switch ev.Kind {
	case KindPostRead:
		return ev.FilePath == ""
	case KindPostEdit:
		return !filepath.IsAbs(ev.FilePath) // owningRepo rejects relative paths
	case KindAugment:
		_, ok := augmentSearchTerm(ev.Pattern)
		return ev.Pattern == "" || !ok
	case KindStop:
		return ev.SessionID == ""
	}
	return false
}
