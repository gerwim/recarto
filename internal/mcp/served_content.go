package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

type contentPart struct {
	graph    *graph.Engine
	root     string
	label    string
	state    *repoState
	folder   bool
	noSearch bool
}

func searchContentServed(ctx context.Context, a *app.App, in SearchContentInput) (SearchContentOutput, error) {
	if err := graph.CheckPattern(in.Pattern, in.Regex, in.IgnoreCase); err != nil {
		return SearchContentOutput{}, err
	}
	limit := effectiveLimit(in.Limit, graph.DefaultContentLimit)
	offset := max(in.Offset, 0)
	var rs []repoState
	answered := map[*app.RepoContext]bool{}
	var err error
	if len(a.Repos) == 1 {
		var s repoState
		s, err = oneRepoState(ctx, a, a.Repos[0])
		if err != nil {
			return SearchContentOutput{}, err
		}
		rs = []repoState{s}
		answered[s.rc] = true
	} else {
		rs, err = freshenServed(ctx, a)
		if err != nil {
			return SearchContentOutput{}, err
		}
	}
	parts := []contentPart{{graph: a.Graph, root: a.Git.Worktree, label: a.Git.Worktree, folder: true, noSearch: in.Language != ""}}
	for i := range rs {
		s := &rs[i]
		parts = append(parts, contentPart{graph: s.rc.Graph, root: s.rc.Git.Worktree, label: s.label, state: s, noSearch: s.empty && len(rs) > 1 && in.Language != ""})
	}
	var all []ContentMatch
	var total int
	var perFileCapped bool
	unsearched := map[*app.RepoContext]string{}
	folderNotes := make([]string, 0, 1)
	var failedParts []string
	var failedLabels []string
	searchedParts := 0
	for _, part := range parts {
		if err := readableDir(part.root); err != nil {
			reason := fmt.Sprintf("its directory cannot be read (%v)", err)
			recordUnsearched(part, unsearched, &folderNotes, &failedParts, &failedLabels, reason)
			continue
		}
		if err := graph.CanList(part.root); err != nil {
			reason := fmt.Sprintf("its files cannot be listed (%v)", err)
			recordUnsearched(part, unsearched, &folderNotes, &failedParts, &failedLabels, reason)
			continue
		}
		searchedParts++
		if part.noSearch {
			continue
		}
		matches, meta, err := part.graph.SearchContentWithMeta(ctx, part.root, in.Pattern, graph.ContentSearchOpts{
			Regex: in.Regex, IgnoreCase: in.IgnoreCase, PathGlob: in.PathGlob,
			Language: in.Language, Limit: offset + limit, MaxPerFile: in.MaxPerFile, NoSymbols: part.folder || part.state != nil && part.state.empty,
		})
		if err != nil {
			var listErr *graph.ListError
			if errors.As(err, &listErr) {
				reason := fmt.Sprintf("its files cannot be listed (%v)", listErr.Err)
				recordUnsearched(part, unsearched, &folderNotes, &failedParts, &failedLabels, reason)
				searchedParts--
				continue
			}
			return SearchContentOutput{}, fmt.Errorf("%s: %w", part.label, err)
		}
		total += meta.Total
		perFileCapped = perFileCapped || meta.PerFileCapped
		for _, m := range matches {
			all = append(all, ContentMatch{Repo: part.label, Path: m.Path, Line: m.Line, Text: m.Text, Symbol: m.Symbol, Kind: m.Kind})
		}
	}
	if searchedParts == 0 {
		//nolint:staticcheck // Keep the approved user-facing error text verbatim.
		return SearchContentOutput{}, fmt.Errorf("Nothing could be searched: %s", strings.Join(failedParts, "; "))
	}
	end := min(offset+limit, len(all))
	page := []ContentMatch{}
	if offset < len(all) {
		page = all[offset:end]
	}
	out := SearchContentOutput{
		Count: len(page), Total: total, Matches: page,
		Truncated: total > offset+len(page), PerFileCapped: perFileCapped,
		Notes: append([]string(nil), folderNotes...),
	}
	if len(rs) == 1 {
		out.Hint = rs[0].hint()
		out.Notes = append(out.Notes, replyNotes(rs, answered, unsearched)...)
	} else {
		out.Notes = append(out.Notes, replyNotes(rs, nil, unsearched)...)
	}
	if len(failedParts) > 0 {
		out.Hint = joinHints(out.Hint, fmt.Sprintf("The search is incomplete: %s could not be searched (see notes).", strings.Join(failedLabels, ", ")))
	}
	return out, nil
}

func readableDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func recordUnsearched(part contentPart, unsearched map[*app.RepoContext]string, folderNotes *[]string, failedParts, failedLabels *[]string, reason string) {
	if part.folder {
		label := part.label + " (this folder)"
		*folderNotes = append(*folderNotes, label+": not searched: "+strings.TrimPrefix(reason, "its ")+".")
		*failedLabels = append(*failedLabels, label)
		*failedParts = append(*failedParts, label+": "+reason)
		return
	}
	unsearched[part.state.rc] = reason
	*failedLabels = append(*failedLabels, part.label)
	*failedParts = append(*failedParts, part.label+": "+reason)
}
