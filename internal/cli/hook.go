package cli

import (
	"context"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Hook dispatches a Claude Code lifecycle hook, reading the hook payload from in
// and writing hook output to out. Hooks are advisory: the caller (Run) exits 0
// even when this returns an error, so a hook failure never blocks the agent.
func Hook(ctx context.Context, a *app.App, in io.Reader, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: event required (session-start|post-read|stop)")
	}
	switch args[0] {
	case "session-start":
		return claudecode.SessionStart(ctx, a, in, out)
	case "post-read":
		return claudecode.PostRead(ctx, a, in, out)
	case "stop":
		return claudecode.Stop(ctx, a, in, out)
	default:
		return fmt.Errorf("hook: unknown event %q (session-start|post-read|stop)", args[0])
	}
}
