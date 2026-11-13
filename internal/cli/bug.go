package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/memory"
)

// Bug dispatches `bug add` and `bug search`.
func Bug(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("bug: subcommand required (add|search)")
	}
	switch args[0] {
	case "add":
		return bugAdd(ctx, a, w, args[1:])
	case "search":
		return bugSearch(ctx, a, w, args[1:])
	default:
		return fmt.Errorf("bug: unknown subcommand %q (add|search)", args[0])
	}
}

// bugAdd records a kind=bug memory with structured OpenWolf-style fields in Extra.
func bugAdd(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("bug add", flag.ContinueOnError)
	fs.SetOutput(w)
	msg := fs.String("error", "", "error message or user complaint (required)")
	var causeVal string
	// --cause is the short form; --root-cause mirrors the record_bug MCP tool's
	// field name so the two surfaces stay in sync. Both write the same value.
	fs.StringVar(&causeVal, "cause", "", "root cause")
	fs.StringVar(&causeVal, "root-cause", "", "root cause (alias of --cause)")
	cause := &causeVal
	fix := fs.String("fix", "", "what fixed it")
	file := fs.String("file", "", "file that was fixed")
	symbols := fs.String("symbols", "", "comma-separated related symbol names")
	tags := fs.String("tags", "", "comma-separated tags")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *msg == "" {
		return fmt.Errorf("bug add: --error is required")
	}

	extra := map[string]any{
		"error_message": *msg,
		"root_cause":    *cause,
		"fix":           *fix,
		"file":          *file,
	}
	if tg := splitCSV(*tags); len(tg) > 0 {
		extra["tags"] = tg
	}
	// Only --file becomes a structural ref (the recall dimension that resurfaces a
	// bug when that file is touched). --tags are descriptive metadata kept in Extra,
	// not structural RefTag entries, so they don't skew structural scoring.
	var refs []memory.Ref
	if *file != "" {
		refs = append(refs, memory.Ref{Type: memory.RefFile, Value: memory.RelFileRef(a.Git.Worktree, *file)})
	}
	for _, s := range splitCSV(*symbols) {
		refs = append(refs, memory.Ref{Type: memory.RefSymbol, Value: s})
	}
	// The body carries error + fix so the lexical index can match either.
	body := *msg
	if *fix != "" {
		body = *msg + " — fix: " + *fix
	}

	id, reinforced, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:     memory.KindBug,
		Title:    *msg,
		Body:     body,
		Refs:     refs,
		Branch:   a.Git.Branch,
		Worktree: a.Git.Worktree,
		Author:   "cli",
		Source:   "cli",
		Extra:    extra,
	})
	if err != nil {
		return err
	}
	verb := "recorded"
	if reinforced {
		verb = "reinforced"
	}
	fmt.Fprintf(w, "%s bug %s\n", verb, id)
	return nil
}

// bugSearch is OpenWolf "bug search" parity: a recall restricted to kind=bug.
func bugSearch(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("bug search", flag.ContinueOnError)
	fs.SetOutput(w)
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	set, err := a.Engine.Recall(ctx, memory.RecallQuery{
		TaskHint: strings.Join(fs.Args(), " "),
		Kind:     memory.KindBug,
		Branch:   a.Git.Branch,
	})
	if err != nil {
		return err
	}
	results := set.Results
	if len(results) == 0 {
		_, err := fmt.Fprintln(w, "(no matching bugs)")
		return err
	}
	for _, r := range results {
		title := r.Record.Title
		if title == "" {
			title = "(untitled)"
		}
		// Include the kind: recall always includes pinned do_not_repeat memories
		// (budget-exempt), so a result here isn't guaranteed to be a bug.
		fmt.Fprintf(w, "• [%s] %s  (score %.3f)\n", r.Record.Kind, title, r.Score)
		if fix, ok := r.Record.Extra["fix"].(string); ok && fix != "" {
			fmt.Fprintf(w, "    fix: %s\n", fix)
		}
	}
	return nil
}
