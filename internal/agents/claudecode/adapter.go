package claudecode

import (
	"context"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Adapter translates Claude Code hook payloads to and from agents.Event/Result.
type Adapter struct{}

// Decode parses the subset of the Claude Code hook payload we use into a neutral
// Event. The grep pattern falls back to the glob query field.
func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID string `json:"session_id"`
		Source    string `json:"source"`
		ToolInput struct {
			FilePath string `json:"file_path"`
			Pattern  string `json:"pattern"`
			Query    string `json:"query"`
		} `json:"tool_input"`
		ToolResult     json.RawMessage `json:"tool_result"`
		StopHookActive bool            `json:"stop_hook_active"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	pattern := p.ToolInput.Pattern
	if pattern == "" {
		pattern = p.ToolInput.Query
	}
	return agents.Event{
		Kind:       kind,
		Agent:      "claude-code",
		SessionID:  p.SessionID,
		Source:     p.Source,
		FilePath:   p.ToolInput.FilePath,
		Pattern:    pattern,
		ToolResult: string(p.ToolResult),
		StopActive: p.StopHookActive,
	}, nil
}

// Encode emits the Claude Code hook output shape: hookSpecificOutput.additionalContext
// for context-injecting events (omitted when there is nothing to inject), and
// systemMessage for Stop.
func (Adapter) Encode(kind agents.EventKind, out io.Writer, res agents.Result) error {
	if kind == agents.KindStop {
		return json.NewEncoder(out).Encode(map[string]any{"systemMessage": res.SystemMessage})
	}
	if res.Context == "" {
		return nil // nothing to inject
	}
	event := "SessionStart"
	if kind != agents.KindSessionStart {
		event = "PostToolUse"
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": res.Context,
		},
	})
}

// Public hook entrypoints (stable API for cli + tests): decode → Handle → encode.

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
