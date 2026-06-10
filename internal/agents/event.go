// Package agents holds Recarto's agent-neutral lifecycle-hook engine. Host
// adapters (claudecode, opencode, copilot) translate each tool's payloads to and
// from the neutral Event/Result types; Handle runs the shared logic in between,
// and the per-host installers register the MCP server, hooks, and instructions.
package agents

import (
	"context"
	"fmt"
	"io"

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

// RunHook decodes the event with ad, runs the shared Handle, then encodes the
// result back through ad. Encode runs even when Handle errors, so advisory output
// built before a failing best-effort side effect is still emitted; the Handle
// error is returned afterward for the (non-blocking) caller to log.
func RunHook(ctx context.Context, a *app.App, ad Adapter, kind EventKind, in io.Reader, out io.Writer) error {
	ev, err := ad.Decode(kind, in)
	if err != nil {
		return err
	}
	res, herr := Handle(ctx, a, ev)
	if encErr := ad.Encode(kind, out, res); encErr != nil {
		return encErr
	}
	return herr
}
