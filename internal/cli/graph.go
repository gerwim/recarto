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
		return fmt.Errorf("graph: subcommand required (search|grep|callers|callees|impls|arch|query|snippet)")
	}
	wt := a.Git.Worktree
	// Lazy reconcile so query results reflect the current checkout (spec §11).
	// Best-effort: a reconcile failure must not fail the query.
	_ = a.Graph.ReconcileIfStale(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt))
	sub, rest := args[0], args[1:]
	switch sub {
	case "search":
		var syms, qwords []string
		for _, tok := range rest {
			if s, ok := strings.CutPrefix(tok, "--symbol="); ok {
				if s != "" {
					syms = append(syms, s)
				}
				continue
			}
			qwords = append(qwords, tok)
		}
		res, err := a.Graph.SearchGuided(ctx, wt, strings.Join(qwords, " "), syms, "", "", 20)
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
				label := n.Name
				if n.Resolution != "" {
					label += fmt.Sprintf(" [%s·%s]", n.Resolution, n.Confidence)
				}
				if n.Receiver != "" {
					label += " recv:" + n.Receiver
				}
				parts[i] = label
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
	case "impls", "implementations":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		dir := "down"
		var sym []string
		for _, tok := range rest {
			switch tok {
			case "--super", "--up":
				dir = "up"
			default:
				sym = append(sym, tok)
			}
		}
		rels, err := a.Graph.Implementations(ctx, wt, strings.Join(sym, " "), dir, 50)
		if err != nil {
			return err
		}
		if len(rels) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, r := range rels {
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			name := r.QualifiedName
			if name == "" {
				name = r.Name
			}
			fmt.Fprintf(w, "• [%s] %s  %s\n", r.Kind, name, loc)
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
	case "grep", "content":
		var opts graph.ContentSearchOpts
		var pat []string
		for _, tok := range rest {
			switch {
			case tok == "-r" || tok == "--regex":
				opts.Regex = true
			case tok == "-i" || tok == "--ignore-case":
				opts.IgnoreCase = true
			case strings.HasPrefix(tok, "--glob="):
				opts.PathGlob = strings.TrimPrefix(tok, "--glob=")
			case strings.HasPrefix(tok, "--lang="):
				opts.Language = strings.TrimPrefix(tok, "--lang=")
			default:
				pat = append(pat, tok)
			}
		}
		if len(pat) == 0 {
			return fmt.Errorf("graph %s: pattern required", sub)
		}
		matches, err := a.Graph.SearchContent(ctx, wt, strings.Join(pat, " "), opts)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, m := range matches {
			sym := m.Symbol
			if sym == "" {
				sym = "(file scope)"
			}
			fmt.Fprintf(w, "%s:%d  [%s %s]  %s\n", m.Path, m.Line, m.Kind, sym, m.Text)
		}
		return nil
	case "snippet":
		if len(rest) < 2 {
			return fmt.Errorf("graph snippet: <file> <symbol> required")
		}
		res, err := a.Graph.Snippet(ctx, wt, rest[0], rest[1])
		if err != nil {
			return err
		}
		if !res.Found {
			return fmt.Errorf("graph snippet: symbol %q not found in %s", rest[1], rest[0])
		}
		fmt.Fprintln(w, res.Snippet)
		return nil
	default:
		return fmt.Errorf("graph: unknown subcommand %q", sub)
	}
}
