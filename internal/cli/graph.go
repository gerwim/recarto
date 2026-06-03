package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Graph dispatches `graph <sub>`: search | callers | callees | arch | query | snippet.
func Graph(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("graph: subcommand required (search|callers|callees|arch|query|snippet)")
	}
	wt := a.Git.Worktree
	// Lazy reconcile so query results reflect the current checkout (spec §11).
	// Best-effort: a reconcile failure must not fail the query.
	_ = a.Graph.Reconcile(ctx, wt, graph.FSSource{Root: wt})
	sub, rest := args[0], args[1:]
	switch sub {
	case "search":
		res, err := a.Graph.Search(ctx, wt, strings.Join(rest, " "), "", "", 20)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, r := range res {
			fmt.Fprintf(w, "• [%s] %s  %s:%d\n", r.Kind, r.QualifiedName, r.Path, r.Line)
		}
		return nil
	case "callers", "callees":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		dir := "callees"
		if sub == "callers" {
			dir = "callers"
		}
		chains, err := a.Graph.TraceCallPath(ctx, wt, rest[0], dir, 4)
		if err != nil {
			return err
		}
		for _, ch := range chains {
			parts := make([]string, len(ch))
			for i, n := range ch {
				parts[i] = n.Name
			}
			fmt.Fprintln(w, strings.Join(parts, " → "))
		}
		return nil
	case "arch":
		arch, err := a.Graph.Architecture(ctx, wt, "")
		if err != nil {
			return err
		}
		for _, m := range arch.Modules {
			fmt.Fprintf(w, "%-20s %d files  %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for _, d := range arch.Deps {
			fmt.Fprintf(w, "  %s → %s  (%d)\n", d.From, d.To, d.Count)
		}
		return nil
	case "query":
		spec := graph.QuerySpec{}
		if len(rest) > 0 {
			spec.Kind = rest[0]
		}
		if len(rest) > 1 {
			spec.NameLike = rest[1]
		}
		nodes, err := a.Graph.Query(ctx, wt, spec)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			fmt.Fprintf(w, "• [%s] %s  %s:%d\n", n.Kind, n.QualifiedName, n.Path, n.Line)
		}
		return nil
	case "snippet":
		if len(rest) < 2 {
			return fmt.Errorf("graph snippet: <file> <symbol> required")
		}
		snip, err := a.Graph.Snippet(ctx, wt, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(w, snip)
		return nil
	default:
		return fmt.Errorf("graph: unknown subcommand %q", sub)
	}
}
