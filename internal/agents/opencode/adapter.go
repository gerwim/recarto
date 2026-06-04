// Package opencode integrates Recarto with OpenCode: opencode.json MCP
// registration, a generated JS plugin that bridges OpenCode lifecycle hooks to
// `recarto hook … --agent opencode`, the AGENTS.md instructions file, and a neutral
// hook-payload adapter. Because recarto owns both ends of the plugin bridge, the
// adapter speaks a simple neutral JSON shape rather than an OpenCode-native one.
package opencode

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
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
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	return agents.Event{
		Kind:       kind,
		Agent:      "opencode",
		SessionID:  p.SessionID,
		Source:     p.Source,
		FilePath:   p.FilePath,
		Pattern:    p.Pattern,
		ToolResult: p.ToolResult,
	}, nil
}

// Encode writes the neutral result the plugin reads from recarto's stdout. The
// plugin applies additionalContext via output.system.push (session start) or by
// appending to a tool result.
func (Adapter) Encode(_ agents.EventKind, out io.Writer, res agents.Result) error {
	return json.NewEncoder(out).Encode(map[string]any{
		"additionalContext": res.Context,
		"systemMessage":     res.SystemMessage,
	})
}

// Public hook entrypoints (used by `recarto hook <event> --agent opencode`).

func SessionStart(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindSessionStart, in, out)
}

func PostRead(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostRead, in, out)
}

func PostEdit(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindPostEdit, in, out)
}

func Augment(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindAugment, in, out)
}

func Stop(ctx context.Context, a *app.App, in io.Reader, out io.Writer) error {
	return agents.RunHook(ctx, a, Adapter{}, agents.KindStop, in, out)
}
