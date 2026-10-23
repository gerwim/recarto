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

// toolKinds routes Copilot's built-in tools: each one Recarto handles to the one
// hook kind it feeds, the others to none. The hook file's matchers already do
// this, but Copilot before v1.0.63 ignored postToolUse matchers and ran every
// postToolUse hook for every tool (as does a hook file from before matchers).
var toolKinds = map[string]agents.EventKind{
	"view":       agents.KindPostRead,
	"edit":       agents.KindPostEdit,
	"create":     agents.KindPostEdit,
	"grep":       agents.KindAugment,
	"glob":       agents.KindAugment,
	"bash":       agents.KindShell,
	"powershell": noKind,
	"task":       noKind,
	"web_fetch":  noKind,
	"ask_user":   noKind,
}

// noKind marks a built-in tool no Recarto hook handles.
const noKind agents.EventKind = -1

// Decode parses Copilot's camelCase payload. A single postToolUse invocation is
// routed to its EventKind by the caller; the file path and grep pattern are pulled
// generically from toolArgs so the adapter is resilient to Copilot's tool names.
// A built-in tool reaching a hook kind it does not feed (a view at post-edit, an
// edit at post-read) decodes without path or pattern, i.e. as an idle event. The
// shell kind reads only the bash tool's command, run in the payload's cwd.
func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID  string          `json:"sessionId"`
		Source     string          `json:"source"`
		CWD        string          `json:"cwd"`
		ToolName   string          `json:"toolName"`
		ToolArgs   json.RawMessage `json:"toolArgs"`
		ToolResult json.RawMessage `json:"toolResult"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	ev := agents.Event{
		Kind:       kind,
		Agent:      "copilot",
		SessionID:  p.SessionID,
		Source:     p.Source,
		ToolResult: string(p.ToolResult),
	}
	if want, builtin := toolKinds[p.ToolName]; builtin && want != kind {
		return ev, nil
	}
	if kind == agents.KindShell {
		if p.ToolName == "bash" {
			ev.Command, ev.CWD = pickString(p.ToolArgs, "command"), p.CWD
		}
		return ev, nil
	}
	ev.FilePath = pickString(p.ToolArgs, "path", "filePath", "file_path", "file")
	ev.Pattern = pickString(p.ToolArgs, "pattern", "query", "regex")
	return ev, nil
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

// pickString returns the first non-empty string value among keys in a JSON object,
// given as an object or as a JSON-encoded string of one (Copilot has sent both).
func pickString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var enc string
	if json.Unmarshal(raw, &enc) == nil {
		raw = json.RawMessage(enc)
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
