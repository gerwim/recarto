package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

// anchors are what routes a memory write made without repo from a code-less folder.
type anchors struct {
	files      []string
	symbols    []string
	supersedes string
	bug        bool
}

// route is the served repo a write goes to, with the file anchors to store
// (resolved against its worktree by memory.RelFileRef).
type route struct {
	rc    *app.RepoContext
	files []string
	held  []bool
	other *app.RepoContext
}

type repoIndex int

const (
	indexEmpty repoIndex = iota
	indexUnfinished
	indexFinished
)

// presentCandidates lists stored files first, sparing a listing of the whole worktree.
const presentCandidates = 8

func readIndex(ctx context.Context, a *app.App, rc *app.RepoContext, symbols []string) (repoIndex, []bool, error) {
	wt := rc.Git.Worktree
	built, has, err := rc.Graph.IndexedSymbols(ctx, wt, rc.Git.NonGit, symbols)
	if err != nil {
		return indexEmpty, nil, fmt.Errorf("read the index of %s: %w", repoLabel(a, rc), err)
	}
	present, _, err := rc.Graph.PresentFiles(ctx, wt, presentCandidates)
	if err != nil {
		present = nil // Without the stored files HasCode lists the whole worktree.
	}
	code, err := graph.HasCode(ctx, wt, rc.Git.NonGit, present...)
	if err == nil && !code {
		return indexEmpty, nil, nil
	}
	if !built {
		return indexUnfinished, nil, nil
	}
	// A listing that fails proves nothing, so the index state decides.
	return indexFinished, has, nil
}

type resolvedAnchor struct {
	kind  string
	value string
	set   []bool
	files []app.OwnedFile
}

type choice struct {
	common   []int
	blocked  int
	multiple []resolvedAnchor
}

func routeWrite(ctx context.Context, a *app.App, w anchors) (route, error) {
	repos := a.Repos
	held, err := repoHolders(ctx, repos, w.supersedes)
	if err != nil {
		return route{}, err
	}
	if len(repos) == 1 {
		return routeSingleRepo(ctx, a, w, repos[0], held)
	}
	if len(w.files)+len(w.symbols) == 0 && w.supersedes == "" {
		return route{}, refuse(append([]string{noAnchorLine(w)}, nextSteps(a, w, nil, nil, false, false)...)...)
	}

	states, has, err := readIndexes(ctx, a, repos, w.symbols)
	if err != nil {
		return route{}, err
	}
	fileAnchors, symbolAnchors := resolveAnchors(repos, w.files, w.symbols, states, has)
	otherAnchors := append(append([]resolvedAnchor(nil), fileAnchors...), symbolAnchors...)
	other := settledRepo(repos, otherAnchors, states)
	if w.supersedes != "" && !anyHeld(held) {
		return route{}, refuseNoHolder(ctx, a, w, held, other)
	}
	return selectRoute(a, w, repos, held, states, fileAnchors, symbolAnchors, other)
}

func repoHolders(ctx context.Context, repos []*app.RepoContext, id string) ([]bool, error) {
	held := make([]bool, len(repos))
	if id == "" {
		return held, nil
	}
	for i, rc := range repos {
		rec, err := rc.Engine.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		held[i] = rec != nil
	}
	return held, nil
}

func routeSingleRepo(ctx context.Context, a *app.App, w anchors, rc *app.RepoContext, held []bool) (route, error) {
	if w.supersedes != "" && !held[0] {
		return route{}, refuseNoHolder(ctx, a, w, held, rc)
	}
	var other *app.RepoContext
	if w.supersedes != "" {
		other = rc
	}
	return route{rc: rc, files: w.files, held: heldOrNil(w, held), other: other}, nil
}

func readIndexes(ctx context.Context, a *app.App, repos []*app.RepoContext, symbols []string) ([]repoIndex, [][]bool, error) {
	states := make([]repoIndex, len(repos))
	has := make([][]bool, len(repos))
	if len(symbols) == 0 {
		return states, has, nil
	}
	for i, rc := range repos {
		state, found, err := readIndex(ctx, a, rc, symbols)
		if err != nil {
			return nil, nil, err
		}
		states[i], has[i] = state, found
	}
	return states, has, nil
}

func resolveAnchors(repos []*app.RepoContext, files, symbols []string, states []repoIndex, has [][]bool) ([]resolvedAnchor, []resolvedAnchor) {
	fileAnchors := make([]resolvedAnchor, len(files))
	for i, f := range files {
		owned := app.OwnedFiles(repos, f)
		set := make([]bool, len(repos))
		for _, candidate := range owned {
			for ri, rc := range repos {
				if rc == candidate.Repo {
					set[ri] = true
					break
				}
			}
		}
		fileAnchors[i] = resolvedAnchor{kind: "file", value: f, set: set, files: owned}
	}
	symbolAnchors := make([]resolvedAnchor, len(symbols))
	for si, symbol := range symbols {
		set := make([]bool, len(repos))
		for ri := range repos {
			set[ri] = states[ri] == indexUnfinished || states[ri] == indexFinished && has[ri][si]
		}
		symbolAnchors[si] = resolvedAnchor{kind: "symbol", value: symbol, set: set}
	}
	return fileAnchors, symbolAnchors
}

func settledRepo(repos []*app.RepoContext, anchors []resolvedAnchor, states []repoIndex) *app.RepoContext {
	if picked, ok := settled(repos, anchors, states); ok {
		return repos[picked]
	}
	return nil
}

func selectRoute(a *app.App, w anchors, repos []*app.RepoContext, held []bool, states []repoIndex, fileAnchors, symbolAnchors []resolvedAnchor, other *app.RepoContext) (route, error) {
	anchors := append(append([]resolvedAnchor(nil), fileAnchors...), symbolAnchors...)
	if w.supersedes != "" {
		anchors = append(anchors, resolvedAnchor{kind: "supersedes", value: w.supersedes, set: held})
	}
	selected := assess(repos, anchors, states)
	if len(selected.common) != 1 {
		lines := []string{"Not stored: the anchors do not lead to exactly one served repo."}
		lines = append(lines, anchorLines(a, anchors, states)...)
		lines = append(lines, nextSteps(a, w, heldOrNil(w, held), other, false, false)...)
		return route{}, refuse(lines...)
	}
	r := selected.common[0]
	if selected.blocked >= 0 {
		lines := []string{fmt.Sprintf("Not stored: the anchors lead only to %s, whose index is unfinished, so a symbol cannot choose it yet.", repoLabel(a, repos[r]))}
		lines = append(lines, anchorLines(a, anchors, states)...)
		lines = append(lines, nextSteps(a, w, heldOrNil(w, held), other, false, false)...)
		return route{}, refuse(lines...)
	}
	if len(selected.multiple) > 0 {
		lines := multipleFileLines(a, repos[r], selected.multiple)
		lines = append(lines, nextSteps(a, w, heldOrNil(w, held), other, false, false)...)
		return route{}, refuse(lines...)
	}

	rels := make([]string, 0, len(fileAnchors))
	seen := make(map[string]bool)
	for _, anchor := range fileAnchors {
		for _, candidate := range anchor.files {
			if candidate.Repo == repos[r] && !seen[candidate.Rel] {
				seen[candidate.Rel] = true
				rels = append(rels, candidate.Rel)
				break
			}
		}
	}
	var routedOther *app.RepoContext
	if w.supersedes != "" {
		routedOther = other
	}
	return route{rc: repos[r], files: rels, held: heldOrNil(w, held), other: routedOther}, nil
}

func heldOrNil(w anchors, held []bool) []bool {
	if w.supersedes == "" {
		return nil
	}
	return held
}

func assess(repos []*app.RepoContext, anchors []resolvedAnchor, states []repoIndex) choice {
	if len(anchors) == 0 {
		return choice{blocked: -1}
	}
	common := make([]bool, len(repos))
	for i := range common {
		common[i] = true
	}
	for _, anchor := range anchors {
		for i := range common {
			common[i] = common[i] && anchor.set[i]
		}
	}
	result := choice{blocked: -1}
	for i, yes := range common {
		if yes {
			result.common = append(result.common, i)
		}
	}
	if len(result.common) != 1 {
		return result
	}
	r := result.common[0]
	for _, anchor := range anchors {
		if anchor.kind == "symbol" && states[r] == indexUnfinished {
			result.blocked = r
		}
		if anchor.kind == "file" {
			owned := 0
			for _, f := range anchor.files {
				if f.Repo == repos[r] {
					owned++
				}
			}
			if owned > 1 {
				result.multiple = append(result.multiple, anchor)
			}
		}
	}
	return result
}

func settled(repos []*app.RepoContext, anchors []resolvedAnchor, states []repoIndex) (int, bool) {
	choice := assess(repos, anchors, states)
	if len(choice.common) != 1 || choice.blocked >= 0 || len(choice.multiple) > 0 {
		return -1, false
	}
	return choice.common[0], true
}

func anchorLines(a *app.App, anchors []resolvedAnchor, states []repoIndex) []string {
	lines := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		var labels []string
		for i, included := range anchor.set {
			if !included {
				continue
			}
			label := repoLabel(a, a.Repos[i])
			if anchor.kind == "symbol" && states[i] == indexUnfinished {
				label += " (index unfinished)"
			}
			labels = append(labels, label)
		}
		if len(labels) == 0 {
			labels = []string{"no served repo"}
		}
		if anchor.kind == "supersedes" {
			lines = append(lines, fmt.Sprintf("- supersedes_id %s: %s", anchor.value, strings.Join(labels, ", ")))
		} else {
			lines = append(lines, fmt.Sprintf("- %s: %s", anchor.value, strings.Join(labels, ", ")))
		}
	}
	return lines
}

func multipleFileLines(a *app.App, rc *app.RepoContext, anchors []resolvedAnchor) []string {
	label := repoLabel(a, rc)
	var lines []string
	for _, anchor := range anchors {
		var files []app.OwnedFile
		for _, f := range anchor.files {
			if f.Repo == rc {
				files = append(files, f)
			}
		}
		lines = append(lines, fmt.Sprintf("Not stored: %s names %d files in %s:", anchor.value, len(files), label))
		for _, f := range files {
			lines = append(lines, fmt.Sprintf("- %s: %s", label, f.Rel))
		}
	}
	return append(lines, "Pass the absolute path of the one you mean.")
}

func noAnchorLine(w anchors) string {
	if w.bug {
		return "Not stored: several repos are served and the bug has no file or symbols to choose one by."
	}
	return "Not stored: several repos are served and the memory has no files or symbols to choose one by."
}

func anyHeld(values []bool) bool {
	for _, v := range values {
		if v {
			return true
		}
	}
	return false
}

func refuseNoHolder(ctx context.Context, a *app.App, w anchors, held []bool, other *app.RepoContext) error {
	inFolder, inWorkspace := false, false
	rec, err := a.Engine.GetByID(ctx, w.supersedes)
	if err != nil {
		return err
	}
	inFolder = rec != nil
	if a.Workspace != nil {
		rec, err = a.Workspace.Engine.GetByID(ctx, w.supersedes)
		if err != nil {
			return err
		}
		inWorkspace = rec != nil
	}
	line := "Not stored: no served repo's store holds memory " + w.supersedes
	if inFolder {
		line += "; it is in this folder's own store"
	}
	if inWorkspace {
		line += "; it is in the workspace store"
	}
	lines := []string{line + "."}
	lines = append(lines, nextSteps(a, w, held, other, inFolder, inWorkspace)...)
	return refuse(lines...)
}

// nextSteps returns a refusal's next-step lines from the same routing facts.
func nextSteps(a *app.App, w anchors, held []bool, other *app.RepoContext, inFolder, inWorkspace bool) []string {
	var lines []string
	if w.supersedes == "" {
		if args := repoArgs(a, a.Repos); len(args) > 0 {
			lines = append(lines, "Pass repo as one of: "+strings.Join(args, ", ")+".")
		}
	} else {
		var holders []*app.RepoContext
		for i, yes := range held {
			if yes {
				holders = append(holders, a.Repos[i])
			}
		}
		if args := repoArgs(a, holders); len(args) > 0 {
			lines = append(lines, "To replace it, pass repo as one of: "+strings.Join(args, ", ")+".")
		}
		if inFolder {
			if args := repoArgs(a, []*app.RepoContext{a.AllRepos()[0]}); len(args) > 0 {
				lines = append(lines, "To replace it in this folder's store, pass repo: "+strings.Join(args, ", ")+".")
			}
		}
		if inWorkspace {
			lines = append(lines, "To replace it in the workspace store, use scope: \"workspace\".")
		}
		if other == nil {
			lines = append(lines, "To store the memory without replacing anything, record again without supersedes_id.")
		} else {
			lines = append(lines, fmt.Sprintf("To store the memory in %q without replacing anything, record again without supersedes_id.", repoLabel(a, other)))
		}
	}
	if !w.bug && a.Workspace != nil && !inWorkspace {
		lines = append(lines, "For knowledge that spans the repos, use scope: \"workspace\".")
	}
	return lines
}

// refuse is a not-stored reply: full sentences for the agent, one per line.
func refuse(lines ...string) error { return errors.New(strings.Join(lines, "\n")) }

// replaceMissed replies after a replacement's target left the chosen store.
func replaceMissed(ctx context.Context, a *app.App, w anchors, from *app.RepoContext) error {
	now, err := routeWrite(ctx, a, w)
	if err != nil {
		return err
	}
	var holders []string
	for i, held := range now.held {
		if held {
			holders = append(holders, repoLabel(a, a.Repos[i]))
		}
	}
	line := fmt.Sprintf("Not stored: memory %s was not in %s's store when the write ran; it is now in %s.", w.supersedes, repoLabel(a, from), strings.Join(holders, ", "))
	lines := []string{line}
	lines = append(lines, nextSteps(a, w, now.held, now.other, false, false)...)
	return refuse(lines...)
}

// beforeRoutedWrite is a test seam between routing and storage; it is nil in production.
var beforeRoutedWrite func()
