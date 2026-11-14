// Package opencode integrates Recarto with OpenCode 1 and 2: opencode.json MCP
// registration, a generated JS plugin that bridges OpenCode lifecycle hooks to
// `recarto hook … --agent opencode`, the AGENTS.md instructions file, and a neutral
// hook-payload adapter. Because recarto owns both ends of the plugin bridge, the
// adapter speaks a simple neutral JSON shape rather than an OpenCode-native one.
package opencode

import (
	"context"
	"encoding/json"
	"io"

	"github.com/gerwim/recarto/internal/agents"
	"github.com/gerwim/recarto/internal/app"
)

// Adapter translates the neutral JSON the generated plugin sends/receives to and
// from agents.Event/Result.
type Adapter struct{}

// Decode parses the neutral payload the plugin writes to recarto's stdin.
func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string `json:"session_id"`
		Source     string `json:"source"`
		FilePath   string `json:"file_path"`
		Pattern    string `json:"pattern"`
		ToolResult string `json:"tool_result"`
		Command    string `json:"command"`
		CWD        string `json:"cwd"`
		Offset     any    `json:"offset"`
		Limit      any    `json:"limit"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	ev := agents.Event{
		Kind:       kind,
		Agent:      "opencode",
		SessionID:  p.SessionID,
		Source:     p.Source,
		FilePath:   p.FilePath,
		Pattern:    p.Pattern,
		ToolResult: p.ToolResult,
		Command:    p.Command,
		CWD:        p.CWD,
	}
	if kind == agents.KindPostRead {
		ev.ReadRanged = p.Offset != nil || p.Limit != nil
		// OpenCode 1 and 2 both number the lines they return "N: ".
		ev.ReadFirst, ev.ReadLast = agents.NumberedLines(p.ToolResult, ": ")
	}
	return ev, nil
}

// Encode writes the neutral result the plugin reads from recarto's stdout. The
// plugin applies additionalContext to the system prompt (session start) or by
// appending to a tool result; a Stop block becomes decision/reason, which the
// OpenCode 2 plugin hands back to the model as a synthetic message.
func (Adapter) Encode(_ agents.EventKind, out io.Writer, res agents.Result) error {
	m := map[string]any{
		"additionalContext": res.Context,
		"systemMessage":     res.SystemMessage,
	}
	if res.Block {
		m["decision"], m["reason"] = "block", res.Reason
	}
	return json.NewEncoder(out).Encode(m)
}

// Public hook entrypoints (used by `recarto hook <event> --agent opencode`).

func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindSessionStart, in, out)
}

func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindPostRead, in, out)
}

func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindPostEdit, in, out)
}

func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindAugment, in, out)
}

func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindStop, in, out)
}

func Shell(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, agents.Opened(a), Adapter{}, agents.KindShell, in, out)
}
