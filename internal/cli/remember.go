package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Remember records a memory of the given kind from CLI flags:
//
//	recarto remember <kind> --body "..." [--title ...] [--files a,b] [--symbols ...]
//	                        [--tags ...] [--pin] [--global]
func Remember(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("remember: kind required (preference|learning|decision|do_not_repeat|bug|action)")
	}
	kind := memory.Kind(args[0])
	if !kind.Valid() {
		return fmt.Errorf("remember: invalid kind %q", args[0])
	}

	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	fs.SetOutput(w)
	title := fs.String("title", "", "short title")
	body := fs.String("body", "", "full content (required)")
	files := fs.String("files", "", "comma-separated file refs")
	symbols := fs.String("symbols", "", "comma-separated symbol refs")
	tags := fs.String("tags", "", "comma-separated tag refs")
	pin := fs.Bool("pin", false, "pin (never evicted)")
	global := fs.Bool("global", false, "store repo-global (not branch-scoped)")
	if helped, err := parseFlags(fs, args[1:]); err != nil || helped {
		return err
	}
	if *body == "" {
		return fmt.Errorf("remember: --body is required")
	}

	id, reinforced, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:     kind,
		Title:    *title,
		Body:     *body,
		Refs:     refsFromFlags(a.Git.Worktree, *files, *symbols, *tags),
		Branch:   branchFor(a, *global),
		Worktree: a.Git.Worktree,
		Author:   "cli",
		Source:   "cli",
		Pinned:   *pin,
	})
	if err != nil {
		return err
	}
	verb := "recorded"
	if reinforced {
		verb = "reinforced"
	}
	fmt.Fprintf(w, "%s %s %s\n", verb, kind, id)
	return nil
}
