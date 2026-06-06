package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Recall runs a ranked, token-budgeted recall and prints the selected memories.
//
//	recarto recall [--files a,b] [--symbols x] [--kind learning] [--limit N]
//	               [--budget N] [free-text task hint...]
//
// NOTE: stdlib flag stops at the first non-flag argument, so flags must precede
// the free-text hint (e.g. `recall --files a.go fix the leak`). The hint is
// optional; omitting it recalls by structural refs and kind alone.
func Recall(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	fs.SetOutput(w)
	files := fs.String("files", "", "comma-separated file refs")
	symbols := fs.String("symbols", "", "comma-separated symbol refs")
	kind := fs.String("kind", "", "restrict to a kind")
	limit := fs.Int("limit", 0, "max candidates (0=default)")
	budget := fs.Int("budget", 0, "token budget (0=default)")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}

	set, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint:    strings.Join(fs.Args(), " "),
		Files:       splitCSV(*files),
		Symbols:     splitCSV(*symbols),
		Kind:        memory.Kind(*kind),
		Branch:      a.Git.Branch,
		Worktree:    a.Git.Worktree,
		Limit:       *limit,
		TokenBudget: *budget,
	})
	if err != nil {
		return err
	}
	results := set.Results
	if len(results) == 0 {
		fmt.Fprintln(w, "(no memories recalled)")
		return nil
	}
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• [%s] %s  (score %.3f)\n", r.Record.Kind, title, r.Score)
		if r.Record.Body != "" {
			// Indent continuation lines so multi-line bodies stay aligned under
			// the bullet instead of rendering flush-left.
			fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(r.Record.Body, "\n", "\n    "))
		}
		if c := r.Caveat(); c != "" {
			fmt.Fprintf(w, "    %s\n", c)
		}
	}
	return nil
}
