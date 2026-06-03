package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/export"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Memory dispatches `memory show` — render all active memory as markdown to w.
func Memory(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("memory: subcommand required (show)")
	}
	if args[0] != "show" {
		return fmt.Errorf("memory: unknown subcommand %q (show)", args[0])
	}
	fs := flag.NewFlagSet("memory show", flag.ContinueOnError)
	fs.SetOutput(w)
	branch := fs.String("branch", "", "limit to a branch (default: all branches)")
	if helped, err := parseFlags(fs, args[1:]); err != nil || helped {
		return err
	}

	recs, err := a.Engine.List(ctx, memory.ListFilter{Branch: *branch})
	if err != nil {
		return err
	}
	fmt.Fprint(w, export.Render(a.Git.RepoName, recs))
	return nil
}
