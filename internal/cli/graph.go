package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// Graph dispatches `graph <sub>`: search | callers | callees | arch | query | snippet.
func Graph(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("graph: subcommand required (search|grep|refs|callers|callees|impls|annotated|outline|arch|query|snippet)")
	}
	sub, rest := args[0], args[1:]
	// Accept space-separated flags (--kind class) as equivalent to the documented
	// --kind=class form. LLMs routinely emit the space form; without this it was
	// silently swallowed as a positional, so the filter AND limit were ignored and
	// the agent got a flood of unfiltered results that pushed it back to shell grep.
	rest = coalesceValueFlags(rest)
	// Discoverability: an agent cannot read the source, so the flags (regex,
	// ignore-case, glob, …) must be reachable from the CLI itself. `graph help`
	// or `graph <sub> --help` prints the usage block and exits before any work.
	if isHelpToken(sub) || containsHelpToken(rest) {
		fmt.Fprint(w, graphUsage)
		return nil
	}
	wt := a.Git.Worktree
	// Lazy reconcile so query results reflect the current checkout (spec §11).
	// Best-effort: a reconcile failure must not fail the query.
	_ = a.Graph.ReconcileIfStale(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt))
	switch sub {
	case "search":
		var syms, qwords []string
		searchLimit := 20
		var searchKind, searchLang string
		for _, tok := range rest {
			switch {
			case strings.HasPrefix(tok, "--symbol="):
				if s := strings.TrimPrefix(tok, "--symbol="); s != "" {
					syms = append(syms, s)
				}
			case strings.HasPrefix(tok, "--kind="):
				searchKind = strings.TrimPrefix(tok, "--kind=")
			case strings.HasPrefix(tok, "--lang="):
				searchLang = strings.TrimPrefix(tok, "--lang=")
			case strings.HasPrefix(tok, "--limit="):
				if n, err := strconv.Atoi(strings.TrimPrefix(tok, "--limit=")); err == nil && n > 0 {
					searchLimit = n
				}
			default:
				qwords = append(qwords, tok)
			}
		}
		// A search with neither query terms nor a candidate --symbol has nothing to
		// rank, so it returned a bare "(no matches)" — which reads as "the repo has
		// nothing", eroding trust, rather than "you gave me no query". Fail with a
		// usage error and point at `query` for filter-only (kind/lang) lookups.
		if len(qwords) == 0 && len(syms) == 0 {
			return fmt.Errorf("graph search: query terms or --symbol required (for a filter-only lookup use `graph query --kind=/--lang=`)")
		}
		res, err := a.Graph.SearchGuided(ctx, wt, strings.Join(qwords, " "), syms, searchKind, searchLang, searchLimit)
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
		// A full page of ranked hits likely means more exist beyond the cap. Results
		// are relevance-ordered, so the top page usually suffices — but say so rather
		// than imply exhaustiveness, and point at the levers that sharpen the ranking.
		if len(res) >= searchLimit {
			fmt.Fprintf(w, "… showing top %d (more may match); narrow with --symbol=, a kind, or more specific terms\n", searchLimit)
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
		sym := rest[0]
		chains, suggestions, err := a.Graph.TraceCallPathFiltered(ctx, wt, sym, dir, 4, 0)
		if err != nil {
			return err
		}
		// Symbol miss: the engine offers lenient candidates. Surfacing them keeps
		// the agent inside recarto instead of reading a silent empty result as
		// "nothing here" and falling back to grep.
		if len(chains) == 0 {
			fmt.Fprintf(w, "(no symbol named %q)\n", sym)
			if len(suggestions) > 0 {
				fmt.Fprintf(w, "did you mean: %s\n", strings.Join(suggestions, ", "))
			}
			return nil
		}
		// Dead-end: the symbol resolved but has no call edges (a single root-only
		// chain). For a type this is expected — calls target its methods, not the
		// declaration — so steer to the queries that *do* answer "who uses this".
		if len(chains) == 1 && len(chains[0]) == 1 {
			root := chains[0][0]
			fmt.Fprintf(w, "(no %s found for %s)\n", dir, sym)
			switch {
			case graph.IsTypeKind(root.Kind):
				fmt.Fprintf(w, "%s is a type (%s); call traces follow methods, not the declaration. Try: `graph impls %s` (implementers), `graph grep -r '\\b%s\\b'` (references), or `graph callers %s.<Method>`\n",
					sym, root.Kind, sym, sym, sym)
			case dir == "callers":
				// A method that resolved but has no callers in the graph: it may be an
				// entrypoint (a public API, test, or DI-invoked handler), or its call
				// sites bind dynamically/through an interface and weren't statically
				// resolved. Steer to a reference search that is still inside recarto
				// (`graph grep`) so the agent doesn't reach for shell grep.
				fmt.Fprintf(w, "it may be an entrypoint, or callers bind dynamically and weren't statically resolved; `graph grep -r '\\b%s\\b'` finds textual references.\n", leafName(sym))
			}
			return nil
		}
		// Legend: the engine emits chains rooted at the queried symbol, so for callers
		// the raw order is target→caller, which renders with the same `→` glyph as
		// callees (target→callee) but the opposite meaning. An LLM then reads the call
		// direction backwards — worse than grep. We make the arrow ALWAYS mean "calls"
		// (left calls right) by reversing callers chains below, and state it here so a
		// line quoted out of context is still unambiguous.
		if dir == "callers" {
			fmt.Fprintf(w, "callers of %s — each line reads outer caller → … → %s (left calls right):\n", sym, leafName(sym))
		} else {
			fmt.Fprintf(w, "callees of %s — each line reads %s → callee → … (left calls right):\n", sym, leafName(sym))
		}
		// Collapse byte-identical rendered chains. One ambiguous call site can yield
		// several low-confidence name candidates that differ only by their (unshown)
		// definition node — they render identically, so N copies cost N× the tokens
		// for zero added signal. We keep first-seen order and annotate the collapsed
		// count with (×N) so the ambiguity is reported, not silently dropped.
		lines := make([]string, 0, len(chains))
		counts := map[string]int{}
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
			// Reverse callers so the arrow uniformly means "calls": outer caller first,
			// queried symbol last. Callees already reads queried→callee, so leave it.
			if dir == "callers" {
				for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
					parts[l], parts[r] = parts[r], parts[l]
				}
			}
			line := strings.Join(parts, " → ")
			if counts[line] == 0 {
				lines = append(lines, line)
			}
			counts[line]++
		}
		for _, line := range lines {
			if n := counts[line]; n > 1 {
				line += fmt.Sprintf("  (×%d)", n)
			}
			fmt.Fprintln(w, line)
		}
		return nil
	case "arch":
		scope := ""
		if len(rest) > 0 {
			scope = rest[0]
		}
		arch, err := a.Graph.Architecture(ctx, wt, scope)
		if err != nil {
			return err
		}
		// Budget identically to the MCP get_architecture tool: a large monorepo
		// otherwise dumps thousands of long-tail dep edges (graphql-platform:
		// 4,404 edges ≈ 90k tokens). Pass a scope arg to drill into omitted detail.
		mods, deps, totalMods, totalDeps, truncated := graph.BudgetArchitecture(arch, 0)
		for _, m := range mods {
			fmt.Fprintf(w, "%-20s %d files  %d symbols\n", m.Name, m.Files, m.Symbols)
		}
		for _, d := range deps {
			fmt.Fprintf(w, "  %s → %s  (%d)\n", d.From, d.To, d.Count)
		}
		if truncated {
			fmt.Fprintf(w, "… showing %d/%d modules, %d/%d deps (highest-signal); pass a path scope to drill in\n",
				len(mods), totalMods, len(deps), totalDeps)
		}
		return nil
	case "impls", "implementations":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		dir := "down"
		implLimit := 50
		var sym []string
		for _, tok := range rest {
			switch {
			case tok == "--super" || tok == "--up":
				dir = "up"
			case strings.HasPrefix(tok, "--limit="):
				implLimit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			case strings.HasPrefix(tok, "-"):
				// An unrecognized flag: skip it rather than folding it into the type
				// name, which silently corrupted the query to "IFoo --limit=5" → no
				// matches and a fallback to grep.
				continue
			default:
				sym = append(sym, tok)
			}
		}
		if implLimit <= 0 {
			implLimit = 50
		}
		rels, err := a.Graph.Implementations(ctx, wt, strings.Join(sym, " "), dir, implLimit)
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
	case "refs", "references":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: symbol required", sub)
		}
		refLimit := 50
		var sym []string
		for _, tok := range rest {
			switch {
			case strings.HasPrefix(tok, "--limit="):
				refLimit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			case strings.HasPrefix(tok, "-"):
				continue // skip stray flags, don't fold them into the symbol
			default:
				sym = append(sym, tok)
			}
		}
		if refLimit <= 0 {
			refLimit = 50
		}
		refs, meta, err := a.Graph.References(ctx, wt, strings.Join(sym, " "), refLimit)
		if err != nil {
			return err
		}
		if !meta.Resolved {
			fmt.Fprintf(w, "(%q did not resolve to a graph node — hits below are text-only)\n", strings.Join(sym, " "))
		}
		if len(refs) == 0 {
			fmt.Fprintln(w, "(no references)")
			return nil
		}
		for _, r := range refs {
			line := fmt.Sprintf("%s:%d  [%s]", r.Path, r.Line, r.Kind)
			if r.From != "" {
				line += "  from " + r.From
			}
			if r.IsTest {
				line += "  [test]"
			}
			if r.Kind == "text" && r.Text != "" {
				line += "  " + r.Text
			}
			fmt.Fprintln(w, line)
		}
		if meta.Truncated {
			fmt.Fprintf(w, "… truncated at %d; raise --limit=\n", refLimit)
		}
		return nil
	case "annotated", "annotations", "decorated":
		if len(rest) == 0 {
			return fmt.Errorf("graph %s: attribute name required", sub)
		}
		annLimit := 50
		var attr []string
		for _, tok := range rest {
			switch {
			case strings.HasPrefix(tok, "--limit="):
				annLimit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			case strings.HasPrefix(tok, "-"):
				continue // skip stray flags, don't fold them into the attribute name
			default:
				attr = append(attr, tok)
			}
		}
		if annLimit <= 0 {
			annLimit = 50
		}
		syms, err := a.Graph.Annotated(ctx, wt, strings.Join(attr, " "), annLimit)
		if err != nil {
			return err
		}
		if len(syms) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, s := range syms {
			name := s.QualifiedName
			if name == "" {
				name = s.Name
			}
			fmt.Fprintf(w, "• [%s] %s  [%s]  %s:%d\n", s.Kind, name, s.Attribute, s.Path, s.Line)
		}
		return nil
	case "query":
		spec := graph.QuerySpec{}
		var edgeKind, edgeDir string
		var names []string
		for _, tok := range rest {
			switch {
			case strings.HasPrefix(tok, "--kind="):
				spec.Kind = strings.TrimPrefix(tok, "--kind=")
			case strings.HasPrefix(tok, "--name="):
				spec.NameLike = strings.TrimPrefix(tok, "--name=")
			case strings.HasPrefix(tok, "--path="):
				spec.Path = strings.TrimPrefix(tok, "--path=")
			case strings.HasPrefix(tok, "--lang="):
				spec.Language = strings.TrimPrefix(tok, "--lang=")
			case strings.HasPrefix(tok, "--edge="):
				edgeKind = strings.TrimPrefix(tok, "--edge=")
			case strings.HasPrefix(tok, "--edge-dir="):
				edgeDir = strings.TrimPrefix(tok, "--edge-dir=")
			case strings.HasPrefix(tok, "--limit="):
				spec.Limit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			default:
				names = append(names, tok)
			}
		}
		// A bare positional is the name to match — the common "find this symbol"
		// case, and what `query <name>` documents. Without this, the first arg was
		// silently read as a KIND filter, so `query OperationExecutionMiddleware`
		// matched nothing and bounced the agent to shell grep. Explicit --name wins.
		if spec.NameLike == "" && len(names) > 0 {
			spec.NameLike = names[0]
		}
		if edgeKind != "" {
			spec.Edge = &graph.EdgeFilter{Kind: edgeKind, Direction: edgeDir}
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
			case strings.HasPrefix(tok, "--limit="):
				opts.Limit, _ = strconv.Atoi(strings.TrimPrefix(tok, "--limit="))
			case strings.HasPrefix(tok, "--max-per-file="):
				opts.MaxPerFile, _ = strconv.Atoi(strings.TrimPrefix(tok, "--max-per-file="))
			default:
				pat = append(pat, tok)
			}
		}
		if len(pat) == 0 {
			return fmt.Errorf("graph %s: pattern required", sub)
		}
		matches, meta, err := a.Graph.SearchContentWithMeta(ctx, wt, strings.Join(pat, " "), opts)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			fmt.Fprintln(w, "(no matches)")
			return nil
		}
		for _, m := range matches {
			fmt.Fprintf(w, "%s:%d  [%s]  %s\n", m.Path, m.Line, grepAnnot(m.Kind, m.Symbol), m.Text)
		}
		// A grep replacement must never present a capped page as the whole answer.
		// Report the true total and how to widen so the agent can trust completeness.
		if meta.Truncated {
			note := fmt.Sprintf("… showing %d of %d matches", meta.Shown, meta.Total)
			if meta.PerFileCapped {
				note += " (some files capped — raise --max-per-file=)"
			}
			note += "; raise --limit= or narrow with --glob=/--lang="
			fmt.Fprintln(w, note)
		}
		return nil
	case "outline", "file":
		if len(rest) == 0 {
			return fmt.Errorf("graph outline: <file> required")
		}
		path := rest[0]
		syms, err := a.Graph.Outline(ctx, wt, path)
		if err != nil {
			return err
		}
		if len(syms) == 0 {
			fmt.Fprintln(w, "(no symbols — file not indexed, or path differs from the indexed relative path)")
			return nil
		}
		// Index symbol names so a child (parent_name matches a sibling) can be
		// indented under its container — the structure is the point of an outline.
		present := make(map[string]bool, len(syms))
		for _, s := range syms {
			present[s.Name] = true
		}
		// Print the path once as a header rather than on every row: repeating a
		// long path per symbol is the bulk of the output and exactly the token
		// cost this command exists to avoid (the path is already the argument).
		fmt.Fprintf(w, "%s\n", path)
		for _, s := range syms {
			indent := ""
			if s.ParentName != "" && present[s.ParentName] {
				indent = "  "
			}
			name := s.Name
			if sig := strings.TrimSpace(s.Signature); sig != "" && sig != s.Name {
				if len(sig) > 100 {
					sig = sig[:100] + "…"
				}
				name = sig
			}
			fmt.Fprintf(w, "%s[%s] %s  :%d\n", indent, s.Kind, name, s.StartLine)
		}
		return nil
	case "snippet":
		switch len(rest) {
		case 0:
			return fmt.Errorf("graph snippet: <symbol> (or <file> <symbol>) required")
		case 1:
			// File-less: resolve the symbol's location ourselves. This is the common
			// path right after a search returns a name — no need to know the file.
			res, cands, err := a.Graph.SnippetByName(ctx, wt, rest[0])
			if err != nil {
				return err
			}
			if res.Found {
				fmt.Fprintln(w, res.Snippet)
				return nil
			}
			if len(cands) == 0 {
				fmt.Fprintf(w, "(no symbol named %q)\n", rest[0])
				return nil
			}
			// Ambiguous: list the definitions so the agent can re-query with a
			// qualified name (usually unique) or the file path, staying in recarto.
			fmt.Fprintf(w, "%q is ambiguous (%d definitions); re-run with a qualified name or the file:\n", rest[0], len(cands))
			for _, c := range cands {
				fmt.Fprintf(w, "• [%s] %s  %s:%d\n", c.Kind, c.QualifiedName, c.Path, c.StartLine)
			}
			return nil
		default:
			res, err := a.Graph.Snippet(ctx, wt, rest[0], rest[1])
			if err != nil {
				return err
			}
			if !res.Found {
				return fmt.Errorf("graph snippet: symbol %q not found in %s", rest[1], rest[0])
			}
			fmt.Fprintln(w, res.Snippet)
			return nil
		}
	default:
		return fmt.Errorf("graph: unknown subcommand %q", sub)
	}
}

// valueFlags are the graph flags that take a value. A bare occurrence of one of
// these (space form, "--kind class") is rewritten to its = form so the existing
// "--flag=" parsing in every subcommand handles it uniformly. Boolean flags
// (-r/-i/--regex/--ignore-case/--super/--up) are absent and so are never coalesced.
var valueFlags = map[string]bool{
	"--symbol": true, "--kind": true, "--lang": true, "--limit": true,
	"--glob": true, "--max-per-file": true, "--name": true, "--path": true,
	"--edge": true, "--edge-dir": true,
}

// coalesceValueFlags rewrites "--flag value" → "--flag=value" for known value
// flags, leaving "--flag=value", boolean flags, and positionals untouched. A
// trailing value flag with no following token, or one followed by another flag,
// is left as-is (it then falls through to that subcommand's own handling).
func coalesceValueFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if valueFlags[tok] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out = append(out, tok+"="+args[i+1])
			i++
			continue
		}
		out = append(out, tok)
	}
	return out
}

// leafName returns the last dotted segment of a (possibly qualified) symbol —
// the identifier to feed a word-boundary grep. "A.B.Create" → "Create".
func leafName(sym string) string {
	if i := strings.LastIndex(sym, "."); i >= 0 && i < len(sym)-1 {
		return sym[i+1:]
	}
	return sym
}

// grepAnnot renders a content match's enclosing-symbol tag. Indexed code lines
// carry "kind qualified.name"; lines in files the graph never parsed (config,
// docs, data) have neither, so they read as "file scope" without a stray space.
func grepAnnot(kind, symbol string) string {
	if symbol == "" {
		return "file scope"
	}
	return kind + " " + symbol
}

// isHelpToken reports whether a token requests usage.
func isHelpToken(tok string) bool {
	return tok == "help" || tok == "-h" || tok == "--help"
}

// containsHelpToken reports whether any token requests usage.
func containsHelpToken(toks []string) bool {
	for _, t := range toks {
		if isHelpToken(t) {
			return true
		}
	}
	return false
}

// graphUsage documents every subcommand and its flags. It is the CLI's only
// discoverability surface — an agent cannot read the source, so search modifiers
// (regex, ignore-case, glob) that exist on the engine must be listed here or they
// are invisible and the agent falls back to shell grep.
const graphUsage = `recarto graph <sub> — query the code graph

  search <terms…> [flags]           concept/identifier search (ranked)
      --symbol=<Name>               candidate identifier you expect (repeatable; sharpens ranking)
      --kind=<k> --lang=<l>         filter by node kind / language
      --limit=<n>                   max results (default 20)
  grep <pattern> [flags]            full-text search over the whole worktree (code +
                                    config/docs/data, honoring .gitignore); each hit
                                    in indexed code shows its enclosing symbol
      -r, --regex                   treat pattern as a regular expression (RE2)
      -i, --ignore-case             case-insensitive match
      --glob=<glob>                 restrict to paths matching glob (e.g. *.cs, src/**)
      --lang=<lang>                 restrict to a language (go, csharp, typescript, …)
      --limit=<n>                   max matches (default 50)
      --max-per-file=<n>            max matches per file (default 5)
  refs <symbol> [--limit=<n>]       every place a symbol is used: call sites, type usages,
                                    imports (graph edges) + a word-boundary text sweep;
                                    [test] marks hits in test code
  callers <symbol>                  who calls a function/method (reads outer caller → … → symbol)
  callees <symbol>                  what a function/method calls (reads symbol → callee → …)
  impls <type> [--super|--up] [--limit=<n>]  implementers (or supertypes) of an interface/base
  annotated <attr> [--limit=<n>]    symbols carrying an attribute/decorator
  outline <file>                    a file's symbols (kind, signature, line) in order
  arch [scope]                      module/dependency map
  query [name] [flags]              structured lookup; bare name = substring on name/qualified-name
                                    needs ≥1 of name/--kind/--lang/--path/--edge (no filter = error, not a dump)
      --kind=<k> --lang=<l>         filter by node kind / language
      --path=<p> --limit=<n>        filter by exact file path / cap results
      --edge=<kind> --edge-dir=in|out  keep only nodes with such an edge (calls, imports, …)
  snippet <symbol> | <file> <symbol>  exact source for a symbol (file optional)
                                    symbol = bare name, qualified name, or Type.Method suffix
                                    overloads (one name, one file) print together

Flags accept either form: --kind=class or --kind class.

Tip: for references to a type/symbol, ` + "`refs <Name>`" + ` is the structural answer;
` + "`grep -r '\\bName\\b'`" + ` is the raw-text fallback.
`
