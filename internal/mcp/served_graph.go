package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

func searchGraphServed(ctx context.Context, a *app.App, in SearchGraphInput) (SearchGraphOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return SearchGraphOutput{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = graph.DefaultSearchLimit
	}
	parts, _, more, err := gather(rs, limit, func(s repoState, cap int) ([]graph.SearchResult, error) {
		return s.rc.Graph.SearchGuided(ctx, s.rc.Git.Worktree, in.Query, in.CandidateSymbols, in.Kind, in.Language, cap)
	})
	if err != nil {
		return SearchGraphOutput{}, err
	}
	out := SearchGraphOutput{Results: []GraphSymbol{}, Notes: replyNotes(rs, nil, nil)}
	exactNames := make(map[string]struct{}, len(in.CandidateSymbols)+1)
	if in.Query != "" {
		exactNames[in.Query] = struct{}{}
	}
	for _, name := range in.CandidateSymbols {
		if name != "" {
			exactNames[name] = struct{}{}
		}
	}
	exact := make([][]GraphSymbol, len(parts))
	other := make([][]GraphSymbol, len(parts))
	for i, part := range parts {
		for _, r := range part {
			name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
			result := GraphSymbol{Repo: rs[i].label, Name: name, QualifiedName: qual, Kind: r.Kind, Path: r.Path, Signature: r.Signature, Line: r.Line, Score: roundScore(r.Score)}
			_, nameMatches := exactNames[r.Name]
			_, qualifiedNameMatches := exactNames[r.QualifiedName]
			if nameMatches || qualifiedNameMatches {
				exact[i] = append(exact[i], result)
			} else {
				other[i] = append(other[i], result)
			}
		}
	}
	// Scores from different repos use different scales, so merge by each repo's rank.
	out.Results = append(out.Results, takeInTurn(exact)...)
	out.Results = append(out.Results, takeInTurn(other)...)
	out.Count = len(out.Results)
	if out.Count == 0 && len(in.CandidateSymbols) == 0 {
		out.Hint = weakResultHint
	} else if more {
		out.Hint = sharedCapHint(a) + " Narrow with candidate_symbols, a kind, a language, or more specific query terms."
	}
	return out, nil
}

func takeInTurn(parts [][]GraphSymbol) []GraphSymbol {
	var out []GraphSymbol
	for round := 0; ; round++ {
		added := false
		for _, part := range parts {
			if round < len(part) {
				out = append(out, part[round])
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

func queryGraphServed(ctx context.Context, a *app.App, in QueryGraphInput, spec graph.QuerySpec) (QueryGraphOutput, error) {
	if spec.IsEmpty() {
		return QueryGraphOutput{}, graph.ErrEmptyQuery
	}
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return QueryGraphOutput{}, err
	}
	if in.Path != "" {
		owned := app.OwnedFiles(a.Repos, in.Path)
		switch len(owned) {
		case 0:
			return QueryGraphOutput{Nodes: []GraphSymbol{}, Hint: noFileHint(in.Path, servedLabels(rs)), Notes: replyNotes(rs, nil, nil)}, nil
		case 1:
			owner := stateFor(rs, owned[0].Repo)
			if owner.empty {
				out := queryOut(nil, "")
				out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
				return out, nil
			}
			spec.Path = owned[0].Rel
			out, err := queryIn(ctx, owner.rc, spec, owner.hint())
			if err != nil {
				return QueryGraphOutput{}, err
			}
			out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
			return out, nil
		default:
			parts := make([]repoState, len(owned))
			for i, f := range owned {
				parts[i] = *stateForPtr(rs, f.Repo)
			}
			fetched := make([][]graph.QueryNode, len(owned))
			avail := make([]int, len(owned))
			filledFetch := false
			limit := effectiveLimit(in.Limit, graph.DefaultListLimit)
			for i, f := range owned {
				if parts[i].empty {
					continue
				}
				spec.Path, spec.Limit = f.Rel, limit
				fetched[i], err = f.Repo.Graph.Query(ctx, f.Repo.Git.Worktree, spec)
				if err != nil {
					return QueryGraphOutput{}, fmt.Errorf("%s: %w", parts[i].label, err)
				}
				avail[i] = len(fetched[i])
				filledFetch = filledFetch || avail[i] == limit
			}
			shares := shareCap(avail, limit)
			out := QueryGraphOutput{Nodes: []GraphSymbol{}, Notes: replyNotes(rs, nil, nil)}
			for i, nodes := range fetched {
				for _, n := range nodes[:min(len(nodes), shares[i])] {
					name, qual := canonicalIdentifier(n.Name, n.QualifiedName)
					out.Nodes = append(out.Nodes, GraphSymbol{Repo: parts[i].label, Name: name, QualifiedName: qual, Kind: n.Kind, Path: owned[i].Rel, Signature: n.Signature, Line: n.Line})
				}
			}
			out.Count = len(out.Nodes)
			if filledFetch || sumInts(avail) > out.Count {
				out.Hint = sharedCapHint(a)
			}
			return out, nil
		}
	}
	limit := effectiveLimit(in.Limit, graph.DefaultListLimit)
	parts, _, more, err := gather(rs, limit, func(s repoState, cap int) ([]graph.QueryNode, error) {
		spec.Limit = cap
		return s.rc.Graph.Query(ctx, s.rc.Git.Worktree, spec)
	})
	if err != nil {
		return QueryGraphOutput{}, err
	}
	out := QueryGraphOutput{Nodes: []GraphSymbol{}, Notes: replyNotes(rs, nil, nil)}
	for i, nodes := range parts {
		for _, n := range nodes {
			name, qual := canonicalIdentifier(n.Name, n.QualifiedName)
			out.Nodes = append(out.Nodes, GraphSymbol{Repo: rs[i].label, Name: name, QualifiedName: qual, Kind: n.Kind, Path: n.Path, Signature: n.Signature, Line: n.Line})
		}
	}
	out.Count = len(out.Nodes)
	if more {
		out.Hint = sharedCapHint(a)
	}
	return out, nil
}

func findImplementationsServed(ctx context.Context, a *app.App, in FindImplementationsInput) (FindImplementationsOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return FindImplementationsOutput{}, err
	}
	limit := effectiveLimit(in.Limit, graph.DefaultListLimit)
	parts, _, more, err := gather(rs, limit, func(s repoState, cap int) ([]graph.TypeRelation, error) {
		return s.rc.Graph.Implementations(ctx, s.rc.Git.Worktree, in.Symbol, in.Direction, cap)
	})
	if err != nil {
		return FindImplementationsOutput{}, err
	}
	out := FindImplementationsOutput{Results: []ImplSymbol{}, Notes: replyNotes(rs, nil, nil)}
	for i, rels := range parts {
		for _, r := range rels {
			name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
			out.Results = append(out.Results, ImplSymbol{Repo: rs[i].label, Name: name, QualifiedName: qual, Kind: r.Kind, Path: r.Path, Line: r.Line, Relation: r.Relation})
		}
	}
	out.Count = len(out.Results)
	if more {
		out.Hint = sharedCapHint(a)
	}
	return out, nil
}

func findAnnotatedServed(ctx context.Context, a *app.App, in FindAnnotatedInput) (FindAnnotatedOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return FindAnnotatedOutput{}, err
	}
	limit := effectiveLimit(in.Limit, graph.DefaultListLimit)
	parts, _, more, err := gather(rs, limit, func(s repoState, cap int) ([]graph.AnnotatedSymbol, error) {
		return s.rc.Graph.Annotated(ctx, s.rc.Git.Worktree, in.Attribute, cap)
	})
	if err != nil {
		return FindAnnotatedOutput{}, err
	}
	out := FindAnnotatedOutput{Results: []AnnotatedSym{}, Notes: replyNotes(rs, nil, nil)}
	for i, rels := range parts {
		for _, r := range rels {
			name, qual := canonicalIdentifier(r.Name, r.QualifiedName)
			out.Results = append(out.Results, AnnotatedSym{Repo: rs[i].label, Name: name, QualifiedName: qual, Kind: r.Kind, Path: r.Path, Line: r.Line, Attribute: r.Attribute})
		}
	}
	out.Count = len(out.Results)
	if more {
		out.Hint = sharedCapHint(a)
	}
	return out, nil
}

func effectiveLimit(n, fallback int) int {
	if n <= 0 {
		return fallback
	}
	return n
}

func stateFor(rs []repoState, rc *app.RepoContext) repoState { return *stateForPtr(rs, rc) }

func stateForPtr(rs []repoState, rc *app.RepoContext) *repoState {
	for i := range rs {
		if rs[i].rc == rc {
			return &rs[i]
		}
	}
	return nil
}

func sumInts(values []int) int {
	total := 0
	for _, n := range values {
		total += n
	}
	return total
}

func getArchitectureServed(ctx context.Context, a *app.App, in GetArchitectureInput) (GetArchitectureOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return GetArchitectureOutput{}, err
	}
	cap := effectiveLimit(in.Limit, graph.DefaultArchLimit)
	arches := make([]graph.Architecture, len(rs))
	for i, s := range rs {
		if s.empty {
			continue
		}
		arches[i], err = s.rc.Graph.Architecture(ctx, s.rc.Git.Worktree, in.Scope)
		if err != nil {
			return GetArchitectureOutput{}, fmt.Errorf("%s: %w", s.label, err)
		}
	}
	modAvail, depAvail := make([]int, len(rs)), make([]int, len(rs))
	sections := make([]ArchSection, len(rs))
	for i, s := range rs {
		sections[i] = ArchSection{Repo: s.label}
		if s.empty {
			sections[i].Note = emptyRepoNote
			continue
		}
		modAvail[i], depAvail[i] = len(arches[i].Modules), len(arches[i].Deps)
		sections[i].ModuleCount, sections[i].DepCount = modAvail[i], depAvail[i]
	}
	modShares, depShares := shareCap(modAvail, cap), shareCap(depAvail, cap)
	out := GetArchitectureOutput{Modules: []ArchModule{}, Deps: []ModuleDep{}, Sections: sections, Notes: replyNotes(rs, nil, nil)}
	for i, arch := range arches {
		mods, _, _, _, _ := graph.BudgetArchitecture(arch, modShares[i])
		if modShares[i] == 0 {
			mods = nil
		}
		_, deps, _, _, _ := graph.BudgetArchitecture(arch, depShares[i])
		if depShares[i] == 0 {
			deps = nil
		}
		for _, m := range mods {
			out.Modules = append(out.Modules, ArchModule{Repo: rs[i].label, Name: m.Name, Files: m.Files, Symbols: m.Symbols})
		}
		for _, d := range deps {
			out.Deps = append(out.Deps, ModuleDep{Repo: rs[i].label, From: d.From, To: d.To, Count: d.Count})
		}
		out.ModuleCount += modAvail[i]
		out.DepCount += depAvail[i]
	}
	out.Truncated = out.ModuleCount > len(out.Modules) || out.DepCount > len(out.Deps)
	if out.Truncated {
		out.Hint = sharedCapHint(a)
	}
	return out, nil
}

func getGraphDeltaServed(ctx context.Context, a *app.App, in GetGraphDeltaInput) (GetGraphDeltaOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return GetGraphDeltaOutput{}, err
	}
	cap := effectiveLimit(in.Limit, graph.DefaultListLimit)
	now := a.Clock.Now().UnixMilli()
	sections := make([]DeltaSection, len(rs))
	parts := make([][]graph.Delta, len(rs))
	avail := make([]int, len(rs))
	for i, s := range rs {
		sections[i] = DeltaSection{Repo: s.label}
		if s.empty {
			sections[i].Note = emptyRepoNote
			continue
		}
		at, ok, err := s.rc.Graph.DeltaBaseline(ctx, s.rc.Git.Worktree, now)
		if err != nil {
			return GetGraphDeltaOutput{}, fmt.Errorf("%s: %w", s.label, err)
		}
		if !ok {
			sections[i].Note = noBaselineNote
			continue
		}
		sections[i].BaselineAt = time.UnixMilli(at).Format(time.RFC3339)
		parts[i], sections[i].Total, err = s.rc.Graph.DeltasSince(ctx, s.rc.Git.Worktree, at, cap)
		if err != nil {
			return GetGraphDeltaOutput{}, fmt.Errorf("%s: %w", s.label, err)
		}
		avail[i] = sections[i].Total
	}
	shares := shareCap(avail, cap)
	out := GetGraphDeltaOutput{Deltas: []GraphDeltaEntry{}, Sections: sections, Notes: replyNotes(rs, nil, nil)}
	for i, deltas := range parts {
		for _, d := range deltas[:min(len(deltas), shares[i])] {
			out.Deltas = append(out.Deltas, GraphDeltaEntry{Repo: rs[i].label, Path: d.Path, QualifiedName: d.QualifiedName, Kind: d.Kind, Change: d.Change})
		}
		out.Total += sections[i].Total
	}
	out.Count = len(out.Deltas)
	out.Truncated = out.Total > out.Count
	if out.Truncated {
		out.Hint = sharedCapHint(a)
	}
	return out, nil
}

func traceCallPathServed(ctx context.Context, a *app.App, in TraceCallPathInput) (TraceCallPathOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return TraceCallPathOutput{}, err
	}
	var found []repoState
	for _, s := range rs {
		if s.empty {
			continue
		}
		has, err := s.rc.Graph.HasSymbol(ctx, s.rc.Git.Worktree, "", in.Symbol)
		if err != nil {
			return TraceCallPathOutput{}, fmt.Errorf("%s: %w", s.label, err)
		}
		if has {
			found = append(found, s)
		}
	}
	if len(found) > 1 {
		return TraceCallPathOutput{Chains: [][]TraceHop{}, Repos: repoNames(found), Hint: severalReposHint(in.Symbol, found), Notes: replyNotes(rs, nil, nil)}, nil
	}
	switch len(found) {
	case 1:
		owner := found[0]
		out, err := traceIn(ctx, owner.rc, in, owner.hint())
		if err != nil {
			return TraceCallPathOutput{}, err
		}
		out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
		return out, nil
	default:
		parts := make([][]string, len(rs))
		for i, s := range rs {
			if s.empty {
				continue
			}
			_, parts[i], err = s.rc.Graph.TraceCallPathFiltered(ctx, s.rc.Git.Worktree, in.Symbol, in.Direction, in.Depth, in.MinConfidence)
			if err != nil {
				return TraceCallPathOutput{}, fmt.Errorf("%s: %w", s.label, err)
			}
		}
		avail := make([]int, len(parts))
		for i := range parts {
			avail[i] = len(parts[i])
		}
		shares := shareCap(avail, graph.TraceSuggestions)
		out := TraceCallPathOutput{Chains: [][]TraceHop{}, RepoSuggestions: []RepoSuggestion{}, Hint: noSymbolHint(in.Symbol, servedLabels(rs)), Notes: replyNotes(rs, nil, nil)}
		for i, suggestions := range parts {
			for _, name := range suggestions[:min(len(suggestions), shares[i])] {
				out.RepoSuggestions = append(out.RepoSuggestions, RepoSuggestion{Repo: rs[i].label, Symbol: name})
			}
		}
		return out, nil
	}
}

func findReferencesServed(ctx context.Context, a *app.App, in FindReferencesInput) (FindReferencesOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return FindReferencesOutput{}, err
	}
	name := graph.ReferenceName(in.Symbol)
	var found []repoState
	for _, s := range rs {
		if s.empty {
			continue
		}
		has, err := s.rc.Graph.HasSymbol(ctx, s.rc.Git.Worktree, "", name)
		if err != nil {
			return FindReferencesOutput{}, fmt.Errorf("%s: %w", s.label, err)
		}
		if has {
			found = append(found, s)
		}
	}
	if len(found) > 1 {
		return FindReferencesOutput{Resolved: true, References: []ReferenceOut{}, Repos: repoNames(found), Hint: severalReposHint(in.Symbol, found), Notes: replyNotes(rs, nil, nil)}, nil
	}
	switch len(found) {
	case 1:
		owner := found[0]
		out, err := referencesIn(ctx, owner.rc, in, owner.hint())
		if err != nil {
			return FindReferencesOutput{}, err
		}
		out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
		return out, nil
	default:
		limit := effectiveLimit(in.Limit, graph.DefaultListLimit)
		parts := make([][]graph.Reference, len(rs))
		metas := make([]graph.ReferencesMeta, len(rs))
		avail := make([]int, len(rs))
		for i, s := range rs {
			parts[i], metas[i], err = s.rc.Graph.TextReferences(ctx, s.rc.Git.Worktree, in.Symbol, limit, s.empty)
			if err != nil {
				return FindReferencesOutput{}, fmt.Errorf("%s: %w", s.label, err)
			}
			avail[i] = len(parts[i])
		}
		shares := shareCap(avail, limit)
		out := FindReferencesOutput{References: []ReferenceOut{}, Notes: replyNotes(rs, nil, nil)}
		truncated := false
		for i, refs := range parts {
			truncated = truncated || metas[i].Truncated
			for _, r := range refs[:min(len(refs), shares[i])] {
				out.References = append(out.References, ReferenceOut{Repo: rs[i].label, Path: r.Path, Line: r.Line, Kind: r.Kind, From: r.From, Text: r.Text, IsTest: r.IsTest})
			}
		}
		out.Count, out.Resolved = len(out.References), false
		out.Truncated = truncated || sumInts(avail) > out.Count
		if out.Truncated {
			out.Hint = sharedCapHint(a)
		}
		return out, nil
	}
}

func getFileOutlineServed(ctx context.Context, a *app.App, in GetFileOutlineInput) (GetFileOutlineOutput, error) {
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return GetFileOutlineOutput{}, err
	}
	owned := app.OwnedFiles(a.Repos, in.File)
	switch len(owned) {
	case 0:
		return GetFileOutlineOutput{Symbols: []OutlineSym{}, Hint: noFileHint(in.File, servedLabels(rs)), Notes: replyNotes(rs, nil, nil)}, nil
	case 1:
		owner := stateFor(rs, owned[0].Repo)
		var out GetFileOutlineOutput
		if owner.empty {
			out = outlineOut(nil, "")
		} else {
			out, err = outlineIn(ctx, owner.rc, owned[0].Rel, owner.hint())
			if err != nil {
				return GetFileOutlineOutput{}, err
			}
		}
		out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
		return out, nil
	default:
		files := fileCandidates(a, owned)
		return GetFileOutlineOutput{Symbols: []OutlineSym{}, Files: files, Hint: fmt.Sprintf("%s names %d files in the served repos. Call again with a listed file's repo as repo and its path as file.", in.File, len(files)), Notes: replyNotes(rs, nil, nil)}, nil
	}
}

func repoNames(rs []repoState) []string {
	names := make([]string, len(rs))
	for i, s := range rs {
		names[i] = s.label
	}
	return names
}

func fileCandidates(a *app.App, files []app.OwnedFile) []FileCandidate {
	out := make([]FileCandidate, len(files))
	for i, f := range files {
		out[i] = FileCandidate{Repo: repoLabel(a, f.Repo), Path: f.Rel}
	}
	return out
}

type snippetSettle struct {
	owner    *repoState
	file     string
	repos    []repoState
	files    []app.OwnedFile
	searched []repoState
}

func settleSnippet(ctx context.Context, rs []repoState, fileGiven bool, owned []app.OwnedFile, symbol string) (snippetSettle, error) {
	if fileGiven {
		switch len(owned) {
		case 0:
			return snippetSettle{searched: rs}, nil
		case 1:
			s := stateForPtr(rs, owned[0].Repo)
			return snippetSettle{owner: s, file: owned[0].Rel}, nil
		default:
			var candidates []app.OwnedFile
			for _, f := range owned {
				s := stateFor(rs, f.Repo)
				if s.empty {
					continue
				}
				has, err := f.Repo.Graph.HasSymbol(ctx, f.Repo.Git.Worktree, f.Rel, symbol)
				if err != nil {
					return snippetSettle{}, fmt.Errorf("%s: %w", s.label, err)
				}
				if has {
					candidates = append(candidates, f)
				}
			}
			switch len(candidates) {
			case 1:
				return snippetSettle{owner: stateForPtr(rs, candidates[0].Repo), file: candidates[0].Rel}, nil
			case 0:
				var searched []repoState
				seen := map[*app.RepoContext]bool{}
				for _, f := range owned {
					if !seen[f.Repo] {
						searched = append(searched, stateFor(rs, f.Repo))
						seen[f.Repo] = true
					}
				}
				return snippetSettle{searched: searched}, nil
			default:
				return snippetSettle{files: candidates}, nil
			}
		}
	}
	var found []repoState
	for i := range rs {
		if rs[i].empty {
			continue
		}
		has, err := rs[i].rc.Graph.HasSymbol(ctx, rs[i].rc.Git.Worktree, "", symbol)
		if err != nil {
			return snippetSettle{}, fmt.Errorf("%s: %w", rs[i].label, err)
		}
		if has {
			found = append(found, rs[i])
		}
	}
	switch len(found) {
	case 0:
		return snippetSettle{searched: rs}, nil
	case 1:
		return snippetSettle{owner: stateForPtr(rs, found[0].rc)}, nil
	default:
		return snippetSettle{repos: found}, nil
	}
}

func getCodeSnippetServed(ctx context.Context, a *app.App, in GetCodeSnippetInput) (GetCodeSnippetOutput, error) {
	if len(in.Symbols) == 0 && in.Symbol == "" {
		return GetCodeSnippetOutput{}, fmt.Errorf("pass symbol (one) or symbols (batch)")
	}
	rs, err := freshenServed(ctx, a)
	if err != nil {
		return GetCodeSnippetOutput{}, err
	}
	var owned []app.OwnedFile
	if in.File != "" {
		owned = app.OwnedFiles(a.Repos, in.File)
	}
	if len(in.Symbols) == 0 {
		settled, err := settleSnippet(ctx, rs, in.File != "", owned, in.Symbol)
		if err != nil {
			return GetCodeSnippetOutput{}, err
		}
		if settled.owner != nil {
			return settledSnippet(ctx, rs, *settled.owner, settled.file, in.Symbol)
		}
		out := GetCodeSnippetOutput{Found: false, Notes: replyNotes(rs, nil, nil)}
		if len(settled.repos) > 0 {
			out.Repos = repoNames(settled.repos)
			out.Hint = severalReposHint(in.Symbol, settled.repos)
		} else if len(settled.files) > 0 {
			out.Files = fileCandidates(a, settled.files)
			out.Hint = fmt.Sprintf("%s is in several files named %s. Call again with a listed file's repo as repo and its path as file.", in.Symbol, in.File)
		} else {
			searched := settled.searched
			if in.File != "" && len(owned) == 0 {
				out.Hint = noFileHint(in.File, servedLabels(rs))
			} else {
				out.Hint = noSymbolHint(in.Symbol, labelsForStates(searched))
			}
		}
		return out, nil
	}
	return getCodeSnippetBatchServed(ctx, a, rs, in, owned)
}

func settledSnippet(ctx context.Context, rs []repoState, owner repoState, file, symbol string) (GetCodeSnippetOutput, error) {
	out := GetCodeSnippetOutput{Found: false}
	if !owner.empty {
		var err error
		out, err = snippetForSymbol(ctx, owner.rc, file, symbol)
		if err != nil {
			return GetCodeSnippetOutput{}, err
		}
	}
	out.Repo, out.Hint = owner.label, owner.hint()
	out.Notes = replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
	return out, nil
}

func getCodeSnippetBatchServed(ctx context.Context, a *app.App, rs []repoState, in GetCodeSnippetInput, owned []app.OwnedFile) (GetCodeSnippetOutput, error) {
	batch := in.Symbols
	if len(batch) > maxBatchSnippets {
		batch = batch[:maxBatchSnippets]
	}
	if in.File != "" && len(owned) == 1 {
		owner := stateFor(rs, owned[0].Repo)
		out := GetCodeSnippetOutput{Snippets: []BatchSnippet{}}
		if len(in.Symbols) > maxBatchSnippets {
			out.Skipped = in.Symbols[maxBatchSnippets:]
		}
		if owner.empty {
			for _, symbol := range batch {
				out.Snippets = append(out.Snippets, BatchSnippet{Symbol: symbol, Found: false, Repo: owner.label})
			}
		} else {
			var err error
			out, err = getCodeSnippetIn(ctx, owner.rc, GetCodeSnippetInput{File: owned[0].Rel, Symbols: in.Symbols}, owner.hint())
			if err != nil {
				return GetCodeSnippetOutput{}, err
			}
			for i := range out.Snippets {
				out.Snippets[i].Repo = owner.label
			}
		}
		out.Repo, out.Notes = owner.label, replyNotes(rs, map[*app.RepoContext]bool{owner.rc: true}, nil)
		return out, nil
	}
	out := GetCodeSnippetOutput{Snippets: []BatchSnippet{}}
	if len(in.Symbols) > maxBatchSnippets {
		out.Skipped = in.Symbols[maxBatchSnippets:]
	}
	answered := map[*app.RepoContext]bool{}
	answeredHint := map[*app.RepoContext]bool{}
	var (
		severalRepos, severalFiles bool
		notFound                   []string
		searched                   []repoState
	)
	for _, symbol := range batch {
		settled, err := settleSnippet(ctx, rs, in.File != "", owned, symbol)
		if err != nil {
			return GetCodeSnippetOutput{}, err
		}
		entry := BatchSnippet{Symbol: symbol, Found: false}
		switch {
		case settled.owner != nil:
			one := GetCodeSnippetOutput{Found: false}
			if !settled.owner.empty {
				one, err = snippetForSymbol(ctx, settled.owner.rc, settled.file, symbol)
				if err != nil {
					return GetCodeSnippetOutput{}, err
				}
			}
			entry.Found, entry.Path, entry.StartLine, entry.EndLine = one.Found, one.Path, one.StartLine, one.EndLine
			entry.QualifiedName, entry.Snippet, entry.Candidates = one.QualifiedName, one.Snippet, one.Candidates
			entry.Repo = settled.owner.label
			answered[settled.owner.rc] = true
			if h := settled.owner.hint(); h != "" && !answeredHint[settled.owner.rc] {
				entry.Hint = h
				answeredHint[settled.owner.rc] = true
			}
		case len(settled.repos) > 0:
			entry.Repos = repoNames(settled.repos)
			severalRepos = true
		case len(settled.files) > 0:
			entry.Files = fileCandidates(a, settled.files)
			severalFiles = true
		default:
			notFound = append(notFound, symbol)
			searched = settled.searched
		}
		out.Snippets = append(out.Snippets, entry)
	}
	out.Notes = replyNotes(rs, answered, nil)
	var hints []string
	if severalRepos {
		hints = append(hints, "For a symbol listed with repos, call again with one of them as repo and only those symbols.")
	}
	if severalFiles {
		hints = append(hints, "For a symbol listed with files, call again with that file's repo as repo and its path as file.")
	}
	if in.File != "" && len(owned) == 0 {
		hints = append(hints, noFileHint(in.File, servedLabels(rs)))
	} else if len(notFound) > 0 {
		hints = append(hints, noSymbolHint(strings.Join(notFound, " or "), labelsForStates(searched)))
	}
	out.Hint = strings.Join(hints, " ")
	return out, nil
}

func labelsForStates(rs []repoState) string {
	labels := make([]string, len(rs))
	for i, s := range rs {
		labels[i] = s.label
	}
	return strings.Join(labels, ", ")
}
