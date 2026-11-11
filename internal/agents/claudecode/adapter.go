package claudecode

import (
	"context"
	"encoding/json"
	"io"

	"github.com/gerwim/recarto/internal/agents"
	"github.com/gerwim/recarto/internal/app"
)

// Adapter translates Claude Code hook payloads to and from agents.Event/Result.
type Adapter struct{}

// Decode parses the subset of the Claude Code hook payload we use into a neutral
// Event. The grep pattern falls back to the glob query field; the tool output
// (sized for the read ledger) comes from tool_response, falling back to the
// tool_result spelling older payloads used. A Bash call carries its command in
// tool_input and the session's working directory in cwd.
func (Adapter) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	var p struct {
		SessionID string `json:"session_id"`
		Source    string `json:"source"`
		CWD       string `json:"cwd"`
		ToolInput struct {
			FilePath string `json:"file_path"`
			Pattern  string `json:"pattern"`
			Query    string `json:"query"`
			Command  string `json:"command"`
		} `json:"tool_input"`
		ToolResult     json.RawMessage `json:"tool_result"`
		ToolResponse   json.RawMessage `json:"tool_response"` // what Claude Code actually sends
		StopHookActive bool            `json:"stop_hook_active"`
	}
	if err := json.NewDecoder(in).Decode(&p); err != nil && err != io.EOF {
		return agents.Event{}, err
	}
	pattern := p.ToolInput.Pattern
	if pattern == "" {
		pattern = p.ToolInput.Query
	}
	result := p.ToolResult
	if len(result) == 0 {
		result = p.ToolResponse
	}
	return agents.Event{
		Kind:       kind,
		Agent:      "claude-code",
		SessionID:  p.SessionID,
		Source:     p.Source,
		FilePath:   p.ToolInput.FilePath,
		Pattern:    pattern,
		ToolResult: string(result),
		StopActive: p.StopHookActive,
		Command:    p.ToolInput.Command,
		CWD:        p.CWD,
	}, nil
}

// Encode emits the Claude Code hook output shape: hookSpecificOutput.additionalContext
// for context-injecting events (omitted when there is nothing to inject), and
// systemMessage for Stop (or decision:block when the nudge fires).
func (Adapter) Encode(kind agents.EventKind, out io.Writer, res agents.Result) error {
	if kind == agents.KindStop {
		if res.Block {
			// decision:block makes Claude Code continue one turn with reason as the
			// instruction — the only Stop output the model acts on.
			return json.NewEncoder(out).Encode(map[string]any{
				"decision": "block", "reason": res.Reason,
			})
		}
		if res.SystemMessage != "" {
			return json.NewEncoder(out).Encode(map[string]any{"systemMessage": res.SystemMessage})
		}
		return nil // nothing to say: emit nothing
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
