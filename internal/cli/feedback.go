package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/feedback"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// Feedback dispatches `recarto feedback [list|show|resolve]`. It opens the global
// feedback store directly (no git/app resolution) so it works from anywhere,
// including outside a git repo.
func Feedback(ctx context.Context, w io.Writer, args []string) error {
	b, err := feedback.Open(clock.Real{})
	if err != nil {
		return err
	}
	defer b.Close()

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return feedbackList(ctx, b, w, args[1:])
	case "show":
		return feedbackShow(ctx, b, w, args[1:])
	case "resolve":
		return feedbackResolve(ctx, b, w, args[1:])
	default:
		return fmt.Errorf("feedback: unknown subcommand %q (list|show|resolve)", sub)
	}
}

func feedbackList(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("feedback list", flag.ContinueOnError)
	fs.SetOutput(w)
	all := fs.Bool("all", false, "include resolved reports")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}

	recs, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback})
	if err != nil {
		return err
	}
	if *all {
		resolved, err := b.Engine.List(ctx, memory.ListFilter{Kind: memory.KindFeedback, Status: "resolved"})
		if err != nil {
			return err
		}
		recs = append(recs, resolved...)
	}
	// Rank by reinforcement (occurrence) desc, then most-recent.
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].ReinforcementCount != recs[j].ReinforcementCount {
			return recs[i].ReinforcementCount > recs[j].ReinforcementCount
		}
		return recs[i].UpdatedAt > recs[j].UpdatedAt
	})
	if len(recs) == 0 {
		fmt.Fprintln(w, "(no feedback)")
		return nil
	}
	for _, r := range recs {
		sig, _ := r.Extra["signal_type"].(string)
		sev, _ := r.Extra["severity"].(string)
		line := fmt.Sprintf("• %s  [%s", r.ID, sig)
		if sev != "" {
			line += "/" + sev
		}
		line += fmt.Sprintf("]  ×%d  %s", r.ReinforcementCount+1, r.Title)
		if r.Status == "resolved" {
			line += "  (resolved)"
		}
		fmt.Fprintln(w, line)
		if repos := sourceRepos(r); repos != "" {
			fmt.Fprintf(w, "    repos: %s\n", repos)
		}
	}
	return nil
}

func feedbackShow(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("feedback show: id required")
	}
	r, err := b.Engine.GetByID(ctx, args[0])
	if err != nil {
		return err
	}
	if r == nil || r.Kind != memory.KindFeedback {
		fmt.Fprintln(w, "(no such feedback)")
		return nil
	}
	fmt.Fprintf(w, "id:        %s\n", r.ID)
	fmt.Fprintf(w, "status:    %s\n", r.Status)
	fmt.Fprintf(w, "occurrences: %d\n", r.ReinforcementCount+1)
	for _, k := range []string{"signal_type", "severity", "tool", "recarto_version", "source_repo_name", "repro", "suggested_fix", "resolution_note"} {
		if v, _ := r.Extra[k].(string); v != "" {
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
	if repos := sourceRepos(r); repos != "" {
		fmt.Fprintf(w, "repos:     %s\n", repos)
	}
	fmt.Fprintf(w, "\n%s\n", r.Body)
	return nil
}

func feedbackResolve(ctx context.Context, b *feedback.Bundle, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("feedback resolve: id required")
	}
	id := args[0]
	fs := flag.NewFlagSet("feedback resolve", flag.ContinueOnError)
	fs.SetOutput(w)
	note := fs.String("note", "", "resolution note")
	if helped, err := parseFlags(fs, args[1:]); err != nil || helped {
		return err
	}
	ok, err := b.Engine.ResolveFeedback(ctx, id, *note)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(w, "(no active feedback with that id)")
		return nil
	}
	fmt.Fprintf(w, "resolved %s\n", id)
	return nil
}

// sourceRepos extracts the comma-joined repo names from the record's repo: tag refs.
func sourceRepos(r *memory.Record) string {
	var repos []string
	for _, ref := range r.Refs {
		if ref.Type == memory.RefTag && strings.HasPrefix(ref.Value, "repo:") {
			repos = append(repos, strings.TrimPrefix(ref.Value, "repo:"))
		}
	}
	return strings.Join(repos, ", ")
}
