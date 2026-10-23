package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
	"git.gerwim.nl/gerwim/recarto/internal/agents/copilot"
	"git.gerwim.nl/gerwim/recarto/internal/agents/opencode"
	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Hook dispatches a lifecycle hook for the host named by --agent (default claude)
// against an already-open App, reading the hook payload from in and writing hook
// output to out. Hooks are advisory: the caller exits 0 even when this returns
// an error, so a hook failure never blocks the agent.
func Hook(ctx context.Context, a *app.App, in io.Reader, out io.Writer, args []string) error {
	return hook(ctx, agents.Opened(a), in, out, args)
}

// runHook is the `recarto hook` command. It opens the App only when the event
// has something to act on — an idle hook, or a post-edit or shell file view
// outside every served repo, costs little more than a process start — and
// skips the workspace store for the per-tool hooks, which never read it. It
// never fails the host's tool call: errors (a store that cannot open included)
// go to stderr only, and a panic is swallowed before anything reaches out.
func runHook(ctx context.Context, in io.Reader, out io.Writer, args []string) {
	var a *app.App
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "recarto hook: panic:", r)
		}
		if a != nil {
			_ = a.Close()
		}
	}()
	open := func(ev agents.Event) (*app.App, error) {
		cfg := app.Config{SkipWorkspace: ev.Kind != agents.KindSessionStart && ev.Kind != agents.KindStop}
		var files []string // the event acts only on these files
		switch {
		case ev.Kind == agents.KindPostEdit:
			files = []string{ev.FilePath}
		case ev.Kind == agents.KindShell && len(ev.Patterns) == 0:
			files = ev.Files
		}
		if files != nil {
			if wts, err := app.Worktrees(cfg); err == nil && !slices.ContainsFunc(files, func(f string) bool { return agents.Owned(wts, f) }) {
				return nil, nil // the handler ignores files outside every served repo
			}
		}
		var err error
		a, err = app.Open(cfg)
		return a, err
	}
	if err := hook(ctx, open, in, out, args); err != nil {
		fmt.Fprintln(os.Stderr, "recarto hook:", err)
	}
}

// hook parses the event, --agent and --cmd, then runs it through
// agents.RunHook, which calls open only when the event can act.
func hook(ctx context.Context, open func(agents.Event) (*app.App, error), in io.Reader, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("hook: event required (session-start|post-read|post-edit|augment|stop|shell)")
	}
	event := args[0]

	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	agent := fs.String("agent", "claude", "agent host: claude|opencode|copilot")
	cmd := fs.String("cmd", "", "shell: the command this per-command hook was started for (Claude Code's if filter)")
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
	if *cmd != "" {
		ad = triggered{ad, *cmd}
	}
	return agents.RunHook(ctx, open, ad, kind, in, out)
}

// triggered stamps the --cmd a per-command hook was started for on each event
// its adapter decodes.
type triggered struct {
	agents.Adapter
	cmd string
}

func (t triggered) Decode(kind agents.EventKind, in io.Reader) (agents.Event, error) {
	ev, err := t.Adapter.Decode(kind, in)
	ev.Trigger = t.cmd
	return ev, err
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
