package mcp

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

// leafName returns the last dotted segment of a (possibly qualified) symbol — the
// identifier to feed a word-boundary reference search. "A.B.Create" → "Create".
func leafName(sym string) string {
	for i := len(sym) - 1; i >= 0; i-- {
		if sym[i] == '.' {
			return sym[i+1:]
		}
	}
	return sym
}

// canonicalIdentifier removes the redundant short name from a list result. Across
// every indexed language (go, csharp, typescript, tsx, javascript, rust, python) a symbol's name
// is exactly the trailing segment of its qualified_name — verified over 124k nodes,
// 100% — so emitting both duplicates the leaf on every row. qualified_name is the
// precise, overload-safe form that get_code_snippet/trace_call_path/find_* already
// accept, so we keep it and drop name whenever name is its leaf (leafName splits on
// '.', the sole qualifier separator those languages use). When qualified_name is
// empty (or, defensively, name is not its leaf) name is kept, so no row ever loses
// its only identifier. This makes the JSON output match the CLI, which already shows
// only the qualified name.
func canonicalIdentifier(name, qualified string) (outName, outQualified string) {
	// Drop name when it is fully derivable from qualified_name: either an exact copy
	// (top-level funcs, and namespace nodes whose multi-segment name equals the
	// qualified_name) or its trailing identifier segment.
	if qualified != "" && (qualified == name || leafName(qualified) == name) {
		return "", qualified
	}
	return name, qualified
}

// freshWait bounds how long a query waits for an in-flight graph refresh before
// answering from the data it has. Small repos finish their refresh inside the
// wait, keeping the old synchronous feel; a large first index keeps running in
// the background while every tool call returns promptly — a hard requirement,
// since MCP clients time out (and cancel) slow tool calls. A var so tests can
// shrink it.
var freshWait = 3 * time.Second

// ensureFresh lazily refreshes the worktree's graph so query results reflect the
// current checkout (spec §11 query-time freshness) — without ever blocking the
// query on an unbounded reconcile. The refresh runs detached and singleflighted
// (see graph.EnsureFresh); when results may be stale or absent, the returned
// hint says so (empty when fresh). Best-effort: a refresh failure must not fail
// the query — stale results beat no results.
func ensureFresh(ctx context.Context, t *app.RepoContext) string {
	f := t.Graph.EnsureFresh(ctx, t.Git.Worktree,
		graph.NewSource(t.Git.Worktree), graph.WorktreeToken(t.Git.Worktree), freshWait)
	return freshnessHint(f)
}

// freshnessHint renders an EnsureFresh status for the agent: what state the
// results are in, what is happening, and what to do about it.
func freshnessHint(f graph.Freshness) string {
	switch f.State {
	case graph.FirstIndex:
		return fmt.Sprintf("The code graph for this worktree is still being built in the background (%s); results are empty or incomplete until it finishes — retry this tool shortly.", f.Detail)
	case graph.Refreshing:
		return fmt.Sprintf("A graph refresh is running in the background (%s); results reflect the last completed index and may be slightly stale.", f.Detail)
	}
	return ""
}

// joinHints concatenates two optional hint sentences.
func joinHints(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

// --- search_graph ---

type SearchGraphInput struct {
	Repo             string   `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Query            string   `json:"query" jsonschema:"the concept or symbol name, in plain words"`
	CandidateSymbols []string `json:"candidate_symbols,omitempty" jsonschema:"3-8 identifier names you expect for the concept, in any form: PascalCase, snake_case, plain words or partial names"`
	Kind             string   `json:"kind,omitempty" jsonschema:"restrict to a node kind (function, method, class, ...)"`
	Language         string   `json:"language,omitempty" jsonschema:"restrict to a language (go, csharp, typescript, tsx, javascript, rust, python)"`
	Limit            int      `json:"limit,omitempty" jsonschema:"max results"`
}
type GraphSymbol struct {
	Name          string  `json:"name,omitempty"`
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
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	res, err := t.Graph.SearchGuided(ctx, t.Git.Worktree, in.Query, in.CandidateSymbols, in.Kind, in.Language, in.Limit)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	out := SearchGraphOutput{Count: len(res), Results: make([]GraphSymbol, 0, len(res))}
	for _, r := range res {
		name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
		out.Results = append(out.Results, GraphSymbol{
			Name: name, QualifiedName: qual, Kind: r.Kind,
			Path: r.Path, Signature: r.Signature, Line: r.Line, Score: roundScore(r.Score),
		})
	}
	if out.Count == 0 && len(in.CandidateSymbols) == 0 {
		out.Hint = weakResultHint
	}
	// A full page of ranked hits likely means more match beyond the cap. Results
	// are relevance-ordered so the top page usually suffices, but flag it rather
	// than imply exhaustiveness and name the levers that sharpen the ranking.
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if out.Count >= limit {
		out.Hint = fmt.Sprintf("Showing the top %d by relevance; more may match. Narrow with candidate_symbols, a kind, a language, or more specific query terms.", limit)
	}
	out.Hint = joinHints(fresh, out.Hint)
	return out, nil
}

// --- trace_call_path ---

type TraceCallPathInput struct {
	Repo          string  `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Symbol        string  `json:"symbol" jsonschema:"the symbol to trace from (name or qualified name)"`
	Direction     string  `json:"direction,omitempty" jsonschema:"callers or callees (default callees)"`
	Depth         int     `json:"depth,omitempty" jsonschema:"max chain depth (default 3)"`
	MinConfidence float64 `json:"min_confidence,omitempty" jsonschema:"confidence floor 0-1; default 0 shows confident hops, and low-confidence ones only when nothing better exists"`
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
	IsTest     bool   `json:"is_test,omitempty"`    // hop lives in test code
}
type TraceCallPathOutput struct {
	Chains      [][]TraceHop `json:"chains"`
	Suggestions []string     `json:"suggestions,omitempty"`
	// Legend states how to read a chain. The engine roots every chain at the queried
	// symbol, so callers chains are target→caller order — the same shape as callees
	// (target→callee) but the opposite call direction. Without a legend an LLM reads
	// callers backwards. We reverse callers chains so every chain reads in the
	// "calls" direction (entry n calls entry n+1) and describe that here.
	Legend string `json:"legend,omitempty"`
	// Hint steers the agent when a trace dead-ends on a type: call edges target a
	// type's methods, not its declaration, so "who uses this type" is answered by
	// find_implementations / find_references, not trace_call_path.
	Hint string `json:"hint,omitempty"`
}

func traceCallPath(ctx context.Context, a *app.App, in TraceCallPathInput) (TraceCallPathOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	dir := in.Direction
	if dir == "" {
		dir = "callees"
	}
	chains, suggestions, err := t.Graph.TraceCallPathFiltered(ctx, t.Git.Worktree, in.Symbol, dir, in.Depth, in.MinConfidence)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	out := TraceCallPathOutput{Chains: make([][]TraceHop, 0, len(chains)), Suggestions: suggestions}
	// A legend only helps when there are chains to read; an unknown-symbol miss
	// returns empty chains + suggestions, where a legend is just noise.
	if len(chains) > 0 {
		if dir == "callers" {
			out.Legend = "each chain reads in the calls direction: entry n calls entry n+1, so the outer caller is first and " + leafName(in.Symbol) + " is last."
		} else {
			out.Legend = "each chain reads in the calls direction: entry n calls entry n+1, so " + leafName(in.Symbol) + " is first and the deepest callee is last."
		}
	}
	for _, ch := range chains {
		hops := make([]TraceHop, len(ch))
		for i, n := range ch {
			hops[i] = TraceHop{
				Symbol: n.Name, Path: n.Path, Line: n.Line,
				Resolution: n.Resolution, Confidence: n.Confidence, Receiver: n.Receiver,
				IsTest: graph.IsTestPath(n.Path),
			}
		}
		// Reverse callers chains so the structured order matches the legend's "calls"
		// direction (outer caller → … → queried symbol). The engine roots chains at
		// the queried symbol; callees already reads queried→callee, so leave it.
		if dir == "callers" {
			for l, r := 0, len(hops)-1; l < r; l, r = l+1, r-1 {
				hops[l], hops[r] = hops[r], hops[l]
			}
		}
		out.Chains = append(out.Chains, hops)
	}
	// Dead-end: a single root-only chain means the symbol resolved but no call edge
	// was traversed. Steer the agent to the query that answers its real question so
	// it stays in the graph instead of grepping.
	if len(chains) == 1 && len(chains[0]) == 1 {
		root := chains[0][0]
		switch {
		case graph.IsTypeKind(root.Kind):
			// Calls target a type's methods, not its declaration.
			out.Hint = fmt.Sprintf("%s is a type (%s); call traces follow methods, not the declaration. Use find_implementations(%q) for implementers, find_references(%q) for references, or trace_call_path on a specific %s.<Method>.",
				root.Name, root.Kind, root.Name, root.Name, root.Name)
		case dir == "callers":
			// Resolved method with no callers: an entrypoint, or call sites bind
			// dynamically/through an interface and weren't statically resolved.
			out.Hint = fmt.Sprintf("%s resolved but has no callers in the graph — it may be an entrypoint, or callers bind dynamically and weren't statically resolved. Use find_references(%q) to find the references the call edges miss.",
				root.Name, leafName(root.Name))
		}
	}
	// All callers are tests: without a marker this reads as "dead production
	// code", when the truth is usually a wrapper or sibling API the trace can't
	// see (e.g. SearchContent vs SearchContentWithMeta). Say so and steer to
	// find_references, which sees edges and text.
	if dir == "callers" && out.Hint == "" {
		anyCaller, allTest := false, true
		for _, hops := range out.Chains {
			if len(hops) < 2 {
				continue
			}
			anyCaller = true
			for _, h := range hops[:len(hops)-1] {
				if !h.IsTest {
					allTest = false
				}
			}
		}
		if anyCaller && allTest {
			leaf := leafName(in.Symbol)
			out.Hint = fmt.Sprintf("every caller found is test code — production may reach %s through a wrapper or sibling API the trace can't see. Use find_references(%q) for non-test references.", leaf, leaf)
		}
	}
	out.Hint = joinHints(fresh, out.Hint)
	return out, nil
}

// --- get_architecture ---

type GetArchitectureInput struct {
	Repo  string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Scope string `json:"scope,omitempty" jsonschema:"only paths under this prefix"`
	Limit int    `json:"limit,omitempty" jsonschema:"max modules and max dependencies (default 80)"`
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
	Hint        string       `json:"hint,omitempty"`      // freshness status while a refresh is in flight
}

// budgetArchitecture delegates to graph.BudgetArchitecture so the CLI (`graph
// arch`) and this handler bound their output identically. See that function for
// the rationale behind the cap.
func budgetArchitecture(arch graph.Architecture, limit int) (mods []graph.Module, deps []graph.ModuleEdge, totalMods, totalDeps int, truncated bool) {
	return graph.BudgetArchitecture(arch, limit)
}

func getArchitecture(ctx context.Context, a *app.App, in GetArchitectureInput) (GetArchitectureOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return GetArchitectureOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	arch, err := t.Graph.Architecture(ctx, t.Git.Worktree, in.Scope)
	if err != nil {
		return GetArchitectureOutput{}, err
	}
	mods, deps, totalMods, totalDeps, truncated := budgetArchitecture(arch, in.Limit)
	out := GetArchitectureOutput{
		Modules:     make([]ArchModule, 0, len(mods)),
		ModuleCount: totalMods, DepCount: totalDeps, Truncated: truncated,
		Hint: fresh,
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
	Repo          string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
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
	Hint  string        `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func queryGraph(ctx context.Context, a *app.App, in QueryGraphInput) (QueryGraphOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return QueryGraphOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	spec := graph.QuerySpec{
		Kind: in.Kind, NameLike: in.NameContains, Path: in.Path,
		Language: in.Language, Limit: in.Limit,
	}
	if in.EdgeKind != "" {
		spec.Edge = &graph.EdgeFilter{Kind: in.EdgeKind, Direction: in.EdgeDirection}
	}
	nodes, err := t.Graph.Query(ctx, t.Git.Worktree, spec)
	if err != nil {
		return QueryGraphOutput{}, err
	}
	out := QueryGraphOutput{Count: len(nodes), Nodes: make([]GraphSymbol, 0, len(nodes)), Hint: fresh}
	for _, n := range nodes {
		name, qual := canonicalIdentifier(n.Name, n.QualifiedName)
		out.Nodes = append(out.Nodes, GraphSymbol{
			Name: name, QualifiedName: qual, Kind: n.Kind, Path: n.Path, Signature: n.Signature, Line: n.Line,
		})
	}
	return out, nil
}

// --- search_content ---

type SearchContentInput struct {
	Repo       string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Pattern    string `json:"pattern" jsonschema:"the text to find: a literal substring, or an RE2 regex with regex=true"`
	Regex      bool   `json:"regex,omitempty" jsonschema:"treat pattern as a regular expression"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"case-insensitive match"`
	PathGlob   string `json:"path_glob,omitempty" jsonschema:"only files matching this glob (e.g. src/**); a glob without a slash matches the file name"`
	Language   string `json:"language,omitempty" jsonschema:"only indexed code in this language (go, csharp, typescript, tsx, javascript, rust, python); omit it to include config, docs and data files"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max matches (default 50)"`
	MaxPerFile int    `json:"max_per_file,omitempty" jsonschema:"max matches per file (default 5)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"skip the first N matches, to page with offset+limit"`
}
type ContentMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Text   string `json:"text"`
	Symbol string `json:"symbol,omitempty"` // enclosing definition's qualified name
	Kind   string `json:"kind,omitempty"`   // enclosing definition's kind
}
type SearchContentOutput struct {
	Count   int            `json:"count"` // matches returned in this page
	Total   int            `json:"total"` // every matching line, both caps ignored
	Matches []ContentMatch `json:"matches"`
	// Truncated reports that the page is partial (Total > Count): the agent must
	// not treat these matches as the complete set. PerFileCapped flags that the
	// per-file cap (max_per_file) withheld matches in at least one file.
	Truncated     bool   `json:"truncated,omitempty"`
	PerFileCapped bool   `json:"per_file_capped,omitempty"`
	Hint          string `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func searchContent(ctx context.Context, a *app.App, in SearchContentInput) (SearchContentOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return SearchContentOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	res, meta, err := t.Graph.SearchContentWithMeta(ctx, t.Git.Worktree, in.Pattern, graph.ContentSearchOpts{
		Regex: in.Regex, IgnoreCase: in.IgnoreCase, PathGlob: in.PathGlob,
		Language: in.Language, Limit: in.Limit, MaxPerFile: in.MaxPerFile, Offset: in.Offset,
	})
	if err != nil {
		return SearchContentOutput{}, err
	}
	out := SearchContentOutput{
		Count: len(res), Total: meta.Total, Truncated: meta.Truncated, PerFileCapped: meta.PerFileCapped,
		Matches: make([]ContentMatch, 0, len(res)), Hint: fresh,
	}
	for _, m := range res {
		out.Matches = append(out.Matches, ContentMatch{
			Path: m.Path, Line: m.Line, Text: m.Text, Symbol: m.Symbol, Kind: m.Kind,
		})
	}
	return out, nil
}

// --- get_code_snippet ---

type GetCodeSnippetInput struct {
	Repo    string   `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	File    string   `json:"file,omitempty" jsonschema:"optional repo-relative path; without it the symbol is resolved across the repo"`
	Symbol  string   `json:"symbol,omitempty" jsonschema:"a bare name, a qualified name or a Type.Method suffix, as search and trace results print it"`
	Symbols []string `json:"symbols,omitempty" jsonschema:"up to 10 symbols, returned in order under snippets; extras are listed in skipped"`
}
type SnippetCandidate struct {
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	Kind          string `json:"kind,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
}
type GetCodeSnippetOutput struct {
	Found         bool   `json:"found"`
	Path          string `json:"path,omitempty"` // repo-relative file of the returned definition
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Snippet       string `json:"snippet,omitempty"`
	// Candidates is populated only when file was omitted and the bare name matched
	// several definitions: Found is false, and the agent should re-query with a
	// qualified_name (usually unique) or by supplying the file.
	Candidates []SnippetCandidate `json:"candidates,omitempty"`
	// Snippets holds the per-symbol results of a batch (`symbols`) call, in input
	// order; the top-level fields above stay zero. Skipped lists symbols beyond
	// the batch cap that were NOT fetched — re-call with them rather than
	// assuming coverage.
	Snippets []BatchSnippet `json:"snippets,omitempty"`
	Skipped  []string       `json:"skipped,omitempty"`
	Hint     string         `json:"hint,omitempty"` // freshness status while a refresh is in flight
}
type BatchSnippet struct {
	Symbol        string             `json:"symbol"` // as requested
	Found         bool               `json:"found"`
	Path          string             `json:"path,omitempty"`
	StartLine     int                `json:"start_line,omitempty"`
	EndLine       int                `json:"end_line,omitempty"`
	QualifiedName string             `json:"qualified_name,omitempty"`
	Snippet       string             `json:"snippet,omitempty"`
	Candidates    []SnippetCandidate `json:"candidates,omitempty"`
}

// maxBatchSnippets caps one batch call so a pathological symbol list cannot blow
// the token budget; overflow is reported in Skipped, never silently dropped.
const maxBatchSnippets = 10

// snippetForSymbol is the single-symbol body shared by the scalar and batch
// forms of getCodeSnippet. The freshness hint is the caller's concern: it is
// per-call, not per-symbol, so getCodeSnippet sets it once on the envelope.
func snippetForSymbol(ctx context.Context, t *app.RepoContext, file, symbol string) (GetCodeSnippetOutput, error) {
	if file == "" {
		r, cands, err := t.Graph.SnippetByName(ctx, t.Git.Worktree, symbol)
		if err != nil {
			return GetCodeSnippetOutput{}, err
		}
		out := GetCodeSnippetOutput{
			Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
			QualifiedName: r.QualifiedName, Snippet: r.Snippet,
		}
		for _, c := range cands {
			out.Candidates = append(out.Candidates, SnippetCandidate{
				Path: c.Path, StartLine: c.StartLine, Kind: c.Kind, QualifiedName: c.QualifiedName,
			})
		}
		return out, nil
	}
	r, err := t.Graph.Snippet(ctx, t.Git.Worktree, file, symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	return GetCodeSnippetOutput{
		Found: r.Found, Path: r.Path, StartLine: r.StartLine, EndLine: r.EndLine,
		QualifiedName: r.QualifiedName, Snippet: r.Snippet,
	}, nil
}

func getCodeSnippet(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	if len(in.Symbols) > 0 {
		var out GetCodeSnippetOutput
		batch := in.Symbols
		if len(batch) > maxBatchSnippets {
			out.Skipped = batch[maxBatchSnippets:]
			batch = batch[:maxBatchSnippets]
		}
		for _, sym := range batch {
			one, err := snippetForSymbol(ctx, t, in.File, sym)
			if err != nil {
				return GetCodeSnippetOutput{}, err
			}
			out.Snippets = append(out.Snippets, BatchSnippet{
				Symbol: sym, Found: one.Found, Path: one.Path,
				StartLine: one.StartLine, EndLine: one.EndLine,
				QualifiedName: one.QualifiedName, Snippet: one.Snippet,
				Candidates: one.Candidates,
			})
		}
		out.Hint = fresh
		return out, nil
	}
	if in.Symbol == "" {
		return GetCodeSnippetOutput{}, fmt.Errorf("pass symbol (one) or symbols (batch)")
	}
	out, err := snippetForSymbol(ctx, t, in.File, in.Symbol)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	out.Hint = fresh
	return out, nil
}

// --- get_file_outline ---

type GetFileOutlineInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	File string `json:"file" jsonschema:"repo-relative file path to outline"`
}
type OutlineSym struct {
	Name          string `json:"name,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind"`
	Parent        string `json:"parent,omitempty"`    // enclosing symbol's name, when nested
	Signature     string `json:"signature,omitempty"` // one-line declaration, when known
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line,omitempty"`
}
type GetFileOutlineOutput struct {
	Count   int          `json:"count"`
	Symbols []OutlineSym `json:"symbols"`
	Hint    string       `json:"hint,omitempty"`
}

// outlineMissHint distinguishes "file genuinely has no symbols" from the common
// mistake of passing an absolute or mismatched path: the graph keys files by their
// repo-relative path, so an absolute path or a wrong prefix silently returns none.
const outlineMissHint = "No symbols. The file may be unindexed, or `file` may not match the indexed repo-relative path (pass a path relative to the repo root, e.g. src/Foo/Bar.cs — not an absolute path)."

func getFileOutline(ctx context.Context, a *app.App, in GetFileOutlineInput) (GetFileOutlineOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return GetFileOutlineOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	syms, err := t.Graph.Outline(ctx, t.Git.Worktree, in.File)
	if err != nil {
		return GetFileOutlineOutput{}, err
	}
	out := GetFileOutlineOutput{Count: len(syms), Symbols: make([]OutlineSym, 0, len(syms))}
	for _, s := range syms {
		name, qual := canonicalIdentifier(s.Name, s.QualifiedName)
		out.Symbols = append(out.Symbols, OutlineSym{
			Name: name, QualifiedName: qual, Kind: s.Kind,
			Parent: s.ParentName, Signature: s.Signature,
			StartLine: s.StartLine, EndLine: s.EndLine,
		})
	}
	if out.Count == 0 {
		out.Hint = outlineMissHint
	}
	out.Hint = joinHints(fresh, out.Hint)
	return out, nil
}

// --- find_implementations ---

type FindImplementationsInput struct {
	Repo      string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Symbol    string `json:"symbol" jsonschema:"the type's bare or qualified name"`
	Direction string `json:"direction,omitempty" jsonschema:"down (default) or up (supertypes)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results (default 50)"`
}
type ImplSymbol struct {
	Name          string `json:"name,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Path          string `json:"path,omitempty"`
	Line          int    `json:"line,omitempty"`
	Relation      string `json:"relation"` // implements | extends
}
type FindImplementationsOutput struct {
	Count   int          `json:"count"`
	Results []ImplSymbol `json:"results"`
	Hint    string       `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func findImplementations(ctx context.Context, a *app.App, in FindImplementationsInput) (FindImplementationsOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return FindImplementationsOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	dir := in.Direction
	if dir == "" {
		dir = "down"
	}
	rels, err := t.Graph.Implementations(ctx, t.Git.Worktree, in.Symbol, dir, in.Limit)
	if err != nil {
		return FindImplementationsOutput{}, err
	}
	out := FindImplementationsOutput{Count: len(rels), Results: make([]ImplSymbol, 0, len(rels)), Hint: fresh}
	for _, r := range rels {
		name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
		out.Results = append(out.Results, ImplSymbol{
			Name: name, QualifiedName: qual, Kind: r.Kind,
			Path: r.Path, Line: r.Line, Relation: r.Relation,
		})
	}
	return out, nil
}

// --- find_annotated ---

type FindAnnotatedInput struct {
	Repo      string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Attribute string `json:"attribute" jsonschema:"the name as written at the use site; the C# Attribute suffix is optional"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results (default 50)"`
}
type AnnotatedSym struct {
	Name          string `json:"name,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Path          string `json:"path,omitempty"`
	Line          int    `json:"line,omitempty"`
	Attribute     string `json:"attribute"` // the attribute name as written at this use site
}
type FindAnnotatedOutput struct {
	Count   int            `json:"count"`
	Results []AnnotatedSym `json:"results"`
	Hint    string         `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func findAnnotated(ctx context.Context, a *app.App, in FindAnnotatedInput) (FindAnnotatedOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return FindAnnotatedOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	rels, err := t.Graph.Annotated(ctx, t.Git.Worktree, in.Attribute, in.Limit)
	if err != nil {
		return FindAnnotatedOutput{}, err
	}
	out := FindAnnotatedOutput{Count: len(rels), Results: make([]AnnotatedSym, 0, len(rels)), Hint: fresh}
	for _, r := range rels {
		name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
		out.Results = append(out.Results, AnnotatedSym{
			Name: name, QualifiedName: qual, Kind: r.Kind,
			Path: r.Path, Line: r.Line, Attribute: r.Attribute,
		})
	}
	return out, nil
}

// --- find_references ---

type FindReferencesInput struct {
	Repo   string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Symbol string `json:"symbol" jsonschema:"bare, qualified or Type.Method form"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max references (default 50)"`
}
type ReferenceOut struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Kind   string `json:"kind"`              // calls|imports|extends|implements|annotates|references|text
	From   string `json:"from,omitempty"`    // enclosing/referencing symbol, "" at file scope
	Text   string `json:"text,omitempty"`    // matching line (text refs only)
	IsTest bool   `json:"is_test,omitempty"` // reference lives in test code
}
type FindReferencesOutput struct {
	// Resolved reports whether the symbol matched a graph node; false means every
	// hit below is text-only (the symbol may be misspelled, or live outside the
	// indexed languages).
	Resolved   bool           `json:"resolved"`
	Count      int            `json:"count"`
	Truncated  bool           `json:"truncated,omitempty"`
	References []ReferenceOut `json:"references"`
	Hint       string         `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func findReferences(ctx context.Context, a *app.App, in FindReferencesInput) (FindReferencesOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return FindReferencesOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	refs, meta, err := t.Graph.References(ctx, t.Git.Worktree, in.Symbol, in.Limit)
	if err != nil {
		return FindReferencesOutput{}, err
	}
	out := FindReferencesOutput{
		Resolved: meta.Resolved, Count: len(refs), Truncated: meta.Truncated,
		References: make([]ReferenceOut, 0, len(refs)), Hint: fresh,
	}
	for _, r := range refs {
		out.References = append(out.References, ReferenceOut{
			Path: r.Path, Line: r.Line, Kind: r.Kind,
			From: r.From, Text: r.Text, IsTest: r.IsTest,
		})
	}
	return out, nil
}

// --- get_graph_delta ---

type GetGraphDeltaInput struct {
	Repo  string `json:"repo,omitempty" jsonschema:"a served repo's name or path; omit for the current one"`
	Limit int    `json:"limit,omitempty" jsonschema:"max symbol changes to return (default 50)"`
}
type GraphDeltaEntry struct {
	Path          string `json:"path"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Change        string `json:"change"` // added | removed | changed
}
type GetGraphDeltaOutput struct {
	// BaselineAt is the start of the last session that changed the graph — the
	// same baseline the session-start tier uses. Empty when no prior working
	// session exists (see Note).
	BaselineAt string            `json:"baseline_at,omitempty"`
	Count      int               `json:"count"`
	Total      int               `json:"total"`
	Truncated  bool              `json:"truncated,omitempty"`
	Deltas     []GraphDeltaEntry `json:"deltas"`
	Note       string            `json:"note,omitempty"`
	Hint       string            `json:"hint,omitempty"` // freshness status while a refresh is in flight
}

func getGraphDelta(ctx context.Context, a *app.App, in GetGraphDeltaInput) (GetGraphDeltaOutput, error) {
	t, err := repoTarget(a, in.Repo)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	fresh := ensureFresh(ctx, t)
	now := a.Clock.Now().UnixMilli()
	at, ok, err := t.Graph.DeltaBaseline(ctx, t.Git.Worktree, now)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	if !ok {
		return GetGraphDeltaOutput{
			Note: "no prior working session has changed the code graph here — nothing to compare against.",
			Hint: fresh, Deltas: []GraphDeltaEntry{},
		}, nil
	}
	deltas, total, err := t.Graph.DeltasSince(ctx, t.Git.Worktree, at, in.Limit)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	out := GetGraphDeltaOutput{
		BaselineAt: time.UnixMilli(at).Format(time.RFC3339),
		Count:      len(deltas), Total: total, Truncated: total > len(deltas),
		Deltas: make([]GraphDeltaEntry, 0, len(deltas)), Hint: fresh,
	}
	for _, d := range deltas {
		out.Deltas = append(out.Deltas, GraphDeltaEntry{
			Path: d.Path, QualifiedName: d.QualifiedName, Kind: d.Kind, Change: d.Change,
		})
	}
	return out, nil
}
