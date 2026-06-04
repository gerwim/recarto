package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
	"git.gerwim.nl/gerwim/recarto/internal/agents/copilot"
	"git.gerwim.nl/gerwim/recarto/internal/agents/opencode"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Hook dispatches a lifecycle hook for the host named by --agent (default claude),
// reading the hook payload from in and writing hook output to out. Hooks are
// advisory: the caller (Run) exits 0 even when this returns an error, so a hook
// failure never blocks the agent.
func Hook(ctx context.Context, a *app.App, in io.Reader, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: event required (session-start|post-read|post-edit|augment|stop)")
	}
	event := args[0]

	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent host: claude|opencode|copilot")
	if helped, err := parseFlags(fs, args[1:]); helped || err != nil {
		return err
	}

	kind, err := agents.KindFromString(event)
	if err != nil {
		return err
	}

	ad, err := adapterFor(*agent)
	if err != nil {
		return err
	}
	return agents.RunHook(ctx, a, ad, kind, in, out)
}

// adapterFor returns the hook adapter for an agent host name.
func adapterFor(agent string) (agents.Adapter, error) {
	switch agent {
	case "claude", "claude-code":
		return claudecode.Adapter{}, nil
	case "opencode":
		return opencode.Adapter{}, nil
	case "copilot":
		return copilot.Adapter{}, nil
	}
	return nil, fmt.Errorf("hook: unknown --agent %q (claude|opencode|copilot)", agent)
}
