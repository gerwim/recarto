package mcp

import (
	"context"
	"math"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
)

// ensureFresh lazily reconciles the worktree's graph so query results reflect the
// current checkout (spec §11 query-time freshness). Best-effort: a reconcile
// failure must not fail the query — stale results beat no results.
func ensureFresh(ctx context.Context, a *app.App) {
	_ = a.Graph.ReconcileIfStale(ctx, a.Git.Worktree, graph.NewSource(a.Git.Worktree), graph.WorktreeToken(a.Git.Worktree))
}

// --- search_graph ---

type SearchGraphInput struct {
	Query            string   `json:"query" jsonschema:"the concept or symbol name to find, in plain natural language"`
	CandidateSymbols []string `json:"candidate_symbols,omitempty" jsonschema:"the identifier names you expect to exist for this concept (3-8 best guesses). Any form is accepted and matched leniently: PascalCase (AddressLookup), interface-prefixed (IAddressLookupService), snake_case (address_lookup), plain phrases (address lookup), or partial/qualified names. Provide these whenever searching by concept rather than an exact known name."`
	Kind             string   `json:"kind,omitempty" jsonschema:"restrict to a node kind (function, method, class, …)"`
	Language         string   `json:"language,omitempty" jsonschema:"restrict to a language (go, csharp, typescript, tsx, javascript)"`
	Limit            int      `json:"limit,omitempty" jsonschema:"max results"`
}
type GraphSymbol struct {
	Name          string  `json:"name"`
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	Signature     string  `json:"signature,omitempty"`
	Line          int     `json:"line"`
	Score         float64 `json:"score,omitempty"`
}
type SearchGraphOutput struct {
	Count   int           `json:"count"`
	Results []GraphSymbol `json:"results"`
	Hint    string        `json:"hint,omitempty"`
}

// weakResultHint nudges the agent to use candidate_symbols when a concept query
// found nothing and supplied none. No server-side model is involved.
const weakResultHint = "No matches. Retry search_graph with candidate_symbols set to the identifier names you'd expect for this concept (e.g. AddressLookup, Geocode) — any form works (PascalCase, interface-prefixed, snake_case, or plain words)."

// roundScore trims a relevance score to 4 decimal places for the wire. Scores are
// an ordinal ranking hint, and the ranking is already fixed at full precision
// before projection, so this never reorders results — it only spares the LLM the
// ~15 trailing digits of float64 noise (0.05245901639344262 → 0.0525) on every hit.
func roundScore(s float64) float64 { return math.Round(s*1e4) / 1e4 }

func searchGraph(ctx context.Context, a *app.App, in SearchGraphInput) (SearchGraphOutput, error) {
	ensureFresh(ctx, a)
	res, err := a.Graph.SearchGuided(ctx, a.Git.Worktree, in.Query, in.CandidateSymbols, in.Kind, in.Language, in.Limit)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	out := SearchGraphOutput{Count: len(res), Results: make([]GraphSymbol, 0, len(res))}
	for _, r := range res {
		out.Results = append(out.Results, GraphSymbol{
			Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
			Path: r.Path, Signature: r.Signature, Line: r.Line, Score: roundScore(r.Score),
		})
	}
	if out.Count == 0 && len(in.CandidateSymbols) == 0 {
		out.Hint = weakResultHint
	}
	return out, nil
}

// --- trace_call_path ---

type TraceCallPathInput struct {
	Symbol        string  `json:"symbol" jsonschema:"the symbol to trace from (name or qualified name)"`
	Direction     string  `json:"direction,omitempty" jsonschema:"callers or callees (default callees)"`
	Depth         int     `json:"depth,omitempty" jsonschema:"max chain depth (default 3)"`
	MinConfidence float64 `json:"min_confidence,omitempty" jsonschema:"confidence floor 0-1. Default 0 = adaptive: confident hops are shown and low-confidence/ambiguous ones surface only when nothing better exists. Raise toward 1 to keep only high-confidence hops."`
}

// TraceHop is one annotated hop in a call chain. Resolution/Confidence/Receiver
// are empty on the root and tell the LLM how each subsequent call was bound.
type TraceHop struct {
	Symbol     string `json:"symbol"`
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Resolution string `json:"resolution,omitempty"` // self|typed|static|iface|name
	Confidence string `json:"confidence,omitempty"` // high|medium|low
	Receiver   string `json:"receiver,omitempty"`   // receiver expr for an ambiguous hop
}
type TraceCallPathOutput struct {
	Chains      [][]TraceHop `json:"chains"`
	Suggestions []string     `json:"suggestions,omitempty"`
}

func traceCallPath(ctx context.Context, a *app.App, in TraceCallPathInput) (TraceCallPathOutput, error) {
	ensureFresh(ctx, a)
	dir := in.Direction
	if dir == "" {
		dir = "callees"
	}
	chains, suggestions, err := a.Graph.TraceCallPathFiltered(ctx, a.Git.Worktree, in.Symbol, dir, in.Depth, in.MinConfidence)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	out := TraceCallPathOutput{Chains: make([][]TraceHop, 0, len(chains)), Suggestions: suggestions}
	for _, ch := range chains {
		hops := make([]TraceHop, len(ch))
		for i, n := range ch {
			hops[i] = TraceHop{
				Symbol: n.Name, Path: n.Path, Line: n.Line,
				Resolution: n.Resolution, Confidence: n.Confidence, Receiver: n.Receiver,
			}
		}
		out.Chains = append(out.Chains, hops)
	}
	return out, nil
}

// --- get_architecture ---

type GetArchitectureInput struct {
	Scope string `json:"scope,omitempty" jsonschema:"restrict to paths under this prefix — the drill-down path for detail a budgeted top-level map omits"`
	Limit int    `json:"limit,omitempty" jsonschema:"max modules and max dependency edges to return (0 = default 80). The map keeps the highest-signal entries (largest modules, heaviest dependencies); module_count/dep_count report the full pre-cap totals."`
}
type ArchModule struct {
	Name    string `json:"name"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
}
type ModuleDep struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
}
type GetArchitectureOutput struct {
	Modules     []ArchModule `json:"modules"`
	Deps        []ModuleDep  `json:"deps,omitempty"`
	ModuleCount int          `json:"module_count"`        // total modules before the limit cap
	DepCount    int          `json:"dep_count"`           // total dependency edges before the limit cap
	Truncated   bool         `json:"truncated,omitempty"` // true when the cap dropped lower-signal entries
}

// budgetArchitecture delegates to graph.BudgetArchitecture so the CLI (`graph
// arch`) and this handler bound their output identically. See that function for
// the rationale behind the cap.
func budgetArchitecture(arch graph.Architecture, limit int) (mods []graph.Module, deps []graph.ModuleEdge, totalMods, totalDeps int, truncated bool) {
	return graph.BudgetArchitecture(arch, limit)
}

func getArchitecture(ctx context.Context, a *app.App, in GetArchitectureInput) (GetArchitectureOutput, error) {
	ensureFresh(ctx, a)
	arch, err := a.Graph.Architecture(ctx, a.Git.Worktree, in.Scope)
	if err != nil {
		return GetArchitectureOutput{}, err
	}
	mods, deps, totalMods, totalDeps, truncated := budgetArchitecture(arch, in.Limit)
	out := GetArchitectureOutput{
		Modules:     make([]ArchModule, 0, len(mods)),
		ModuleCount: totalMods, DepCount: totalDeps, Truncated: truncated,
	}
	for _, m := range mods {
		out.Modules = append(out.Modules, ArchModule{Name: m.Name, Files: m.Files, Symbols: m.Symbols})
	}
	for _, d := range deps {
		out.Deps = append(out.Deps, ModuleDep{From: d.From, To: d.To, Count: d.Count})
	}
	return out, nil
}

// --- query_graph ---

type QueryGraphInput struct {
	Kind          string `json:"kind,omitempty" jsonschema:"node kind filter"`
	NameContains  string `json:"name_contains,omitempty" jsonschema:"substring match on name/qualified_name"`
	Path          string `json:"path,omitempty" jsonschema:"exact file path filter"`
	Language      string `json:"language,omitempty" jsonschema:"language filter"`
	EdgeKind      string `json:"edge_kind,omitempty" jsonschema:"require an edge of this kind (e.g. calls, imports)"`
	EdgeDirection string `json:"edge_direction,omitempty" jsonschema:"out (node is source) or in (node is target)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max results"`
}
type QueryGraphOutput struct {
	Count int           `json:"count"`
	Nodes []GraphSymbol `json:"nodes"`
}

func queryGraph(ctx context.Context, a *app.App, in QueryGraphInput) (QueryGraphOutput, error) {
	ensureFresh(ctx, a)
	spec := graph.QuerySpec{
		Kind: in.Kind, NameLike: in.NameContains, Path: in.Path,
		Language: in.Language, Limit: in.Limit,
	}
	if in.EdgeKind != "" {
		spec.Edge = &graph.EdgeFilter{Kind: in.EdgeKind, Direction: in.EdgeDirection}
	}
	nodes, err := a.Graph.Query(ctx, a.Git.Worktree, spec)
	if err != nil {
		return QueryGraphOutput{}, err
	}
	out := QueryGraphOutput{Count: len(nodes), Nodes: make([]GraphSymbol, 0, len(nodes))}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, GraphSymbol{
			Name: n.Name, QualifiedName: n.QualifiedName, Kind: n.Kind, Path: n.Path, Signature: n.Signature, Line: n.Line,
		})
	}
	return out, nil
}

// --- search_content ---

type SearchContentInput struct {
	Pattern    string `json:"pattern" jsonschema:"the text to find. Literal substring by default; set regex=true to treat it as a regular expression (RE2 syntax)."`
	Regex      bool   `json:"regex,omitempty" jsonschema:"treat pattern as a regular expression instead of a literal substring"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive match"`
	PathGlob   string `json:"path_glob,omitempty" jsonschema:"restrict to files whose path matches this glob (e.g. *.cs, src/**). A glob without a slash matches the filename."`
	Language   string `json:"language,omitempty" jsonschema:"restrict to a language (go, csharp, typescript, tsx, javascript)"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max matches (default 50)"`
	MaxPerFile int    `json:"max_per_file,omitempty" jsonschema:"max matches per file (default 5) so one file cannot dominate the page"`
}
type ContentMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Text   string `json:"text"`
	Symbol string `json:"symbol,omitempty"` // enclosing definition's qualified name
	Kind   string `json:"kind,omitempty"`   // enclosing definition's kind
}
type SearchContentOutput struct {
	Count   int            `json:"count"`
	Matches []ContentMatch `json:"matches"`
}

func searchContent(ctx context.Context, a *app.App, in SearchContentInput) (SearchContentOutput, error) {
	ensureFresh(ctx, a)
	res, err := a.Graph.SearchContent(ctx, a.Git.Worktree, in.Pattern, graph.ContentSearchOpts{
		Regex: in.Regex, IgnoreCase: in.IgnoreCase, PathGlob: in.PathGlob,
		Language: in.Language, Limit: in.Limit, MaxPerFile: in.MaxPerFile,
	})
	if err != nil {
		return SearchContentOutput{}, err
	}
	out := SearchContentOutput{Count: len(res), Matches: make([]ContentMatch, 0, len(res))}
	for _, m := range res {
		out.Matches = append(out.Matches, ContentMatch{
			Path: m.Path, Line: m.Line, Text: m.Text, Symbol: m.Symbol, Kind: m.Kind,
		})
	}
	return out, nil
}

// --- get_code_snippet ---

type GetCodeSnippetInput struct {
	File   string `json:"file" jsonschema:"repo-relative file path"`
	Symbol string `json:"symbol" jsonschema:"symbol name or qualified name"`
}
type GetCodeSnippetOutput struct {
	Found         bool   `json:"found"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Snippet       string `json:"snippet,omitempty"`
}

func getCodeSnippet(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	ensureFresh(ctx, a)
	r, err := a.Graph.Snippet(ctx, a.Git.Worktree, in.File, in.Symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	return GetCodeSnippetOutput{
		Found: r.Found, StartLine: r.StartLine, EndLine: r.EndLine,
		QualifiedName: r.QualifiedName, Snippet: r.Snippet,
	}, nil
}

// --- find_implementations ---

type FindImplementationsInput struct {
	Symbol    string `json:"symbol" jsonschema:"the interface/base type's bare or qualified name"`
	Direction string `json:"direction,omitempty" jsonschema:"down (default) = implementers/subtypes of the type; up = the supertypes the type extends"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results (default 50)"`
}
type ImplSymbol struct {
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Path          string `json:"path,omitempty"`
	Line          int    `json:"line,omitempty"`
	Relation      string `json:"relation"` // implements | extends
}
type FindImplementationsOutput struct {
	Count   int          `json:"count"`
	Results []ImplSymbol `json:"results"`
}

func findImplementations(ctx context.Context, a *app.App, in FindImplementationsInput) (FindImplementationsOutput, error) {
	ensureFresh(ctx, a)
	dir := in.Direction
	if dir == "" {
		dir = "down"
	}
	rels, err := a.Graph.Implementations(ctx, a.Git.Worktree, in.Symbol, dir, in.Limit)
	if err != nil {
		return FindImplementationsOutput{}, err
	}
	out := FindImplementationsOutput{Count: len(rels), Results: make([]ImplSymbol, 0, len(rels))}
	for _, r := range rels {
		out.Results = append(out.Results, ImplSymbol{
			Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
			Path: r.Path, Line: r.Line, Relation: r.Relation,
		})
	}
	return out, nil
}
