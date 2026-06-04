// Package copilot integrates Recarto with GitHub Copilot CLI: MCP registration,
// a .github/hooks (or ~/.copilot/hooks) hook file, the instructions file, and the
// camelCase hook-payload adapter. Copilot CLI hooks pass camelCase JSON on stdin
// and read additionalContext from stdout (see the GitHub Copilot hooks reference).
package copilot

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Adapter translates Copilot CLI hook payloads to and from agents.Event/Result.
type Adapter struct{}

// Decode parses Copilot's camelCase payload. A single postToolUse invocation is
// routed to its EventKind by the caller; the file path and grep pattern are pulled
// generically from toolArgs so the adapter is resilient to Copilot's tool names.
func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string          `json:"sessionId"`
		Source     string          `json:"source"`
		ToolName   string          `json:"toolName"`
		ToolArgs   json.RawMessage `json:"toolArgs"`
		ToolResult json.RawMessage `json:"toolResult"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	return agents.Event{
		Kind:       kind,
		Agent:      "copilot",
		SessionID:  p.SessionID,
		Source:     p.Source,
		FilePath:   pickString(p.ToolArgs, "path", "filePath", "file_path", "file"),
		Pattern:    pickString(p.ToolArgs, "pattern", "query", "regex"),
		ToolResult: string(p.ToolResult),
	}, nil
}

// Encode emits Copilot's hook output. sessionStart/postToolUse accept an
// additionalContext string; agentStop is informational (we never block), so it
// emits an empty object.
func (Adapter) Encode(kind agents.EventKind, out io.Writer, res agents.Result) error {
	if kind == agents.KindStop || res.Context == "" {
		return json.NewEncoder(out).Encode(map[string]any{})
	}
	return json.NewEncoder(out).Encode(map[string]any{"additionalContext": res.Context})
}

// pickString returns the first non-empty string value among keys in a JSON object.
func pickString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Public hook entrypoints (used by `recarto hook <event> --agent copilot`).

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
