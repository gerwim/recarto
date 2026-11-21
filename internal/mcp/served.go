package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
)

//nolint:staticcheck // Keep the approved user-facing error text verbatim.
var errNoCode = errors.New("No code is indexed for this directory: it is outside git and held no code files when the Recarto server started. search_content still searches its files.")

func graphTarget(a *app.App, repo string) (*app.RepoContext, bool, error) {
	if repo != "" || !a.Codeless {
		t, err := repoTarget(a, repo)
		return t, false, err
	}
	switch len(a.Repos) {
	case 0:
		return nil, false, errNoCode
	case 1:
		return a.Repos[0], false, nil
	default:
		return nil, true, nil
	}
}

type repoState struct {
	rc    *app.RepoContext
	label string
	empty bool
	fresh graph.Freshness
	built bool
}

func (s repoState) hint() string { return freshnessHint(s.fresh) }
func (s repoState) unfinished() bool {
	return !s.empty && s.fresh.State != graph.Fresh && !s.built
}
func (s repoState) stale() bool { return !s.empty && s.fresh.State != graph.Fresh && s.built }

func stateOf(ctx context.Context, a *app.App, rc *app.RepoContext, checkEmpty bool, ensure func() graph.Freshness) (repoState, error) {
	s := repoState{rc: rc, label: repoLabel(a, rc)}
	if checkEmpty && nothingToIndex(ctx, rc) {
		s.empty = true
		return s, nil
	}
	s.fresh = ensure()
	if s.fresh.State == graph.Fresh {
		s.built = true
		return s, nil
	}
	var err error
	s.built, _, err = rc.Graph.IndexedSymbols(ctx, rc.Git.Worktree, rc.Git.NonGit, nil)
	if err != nil {
		return repoState{}, fmt.Errorf("read the index of %s: %w", s.label, err)
	}
	return s, nil
}

func freshenServed(ctx context.Context, a *app.App) ([]repoState, error) {
	deadline := time.Now().Add(freshWait)
	states := make([]repoState, len(a.Repos))
	errs := make([]error, len(a.Repos))
	var wg sync.WaitGroup
	for i, rc := range a.Repos {
		wg.Go(func() {
			states[i], errs[i] = stateOf(ctx, a, rc, true, func() graph.Freshness {
				wt := rc.Git.Worktree
				return rc.Graph.EnsureFreshBy(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt, ""), deadline)
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return states, nil
}

func oneRepoState(ctx context.Context, a *app.App, rc *app.RepoContext) (repoState, error) {
	return stateOf(ctx, a, rc, false, func() graph.Freshness {
		wt := rc.Git.Worktree
		return rc.Graph.EnsureFresh(ctx, wt, graph.NewSource(wt), graph.WorktreeToken(wt, ""), freshWait)
	})
}

func replyNotes(rs []repoState, answered map[*app.RepoContext]bool, unsearched map[*app.RepoContext]string) []string {
	var notes []string
	for _, s := range rs {
		var parts []string
		if reason := unsearched[s.rc]; reason != "" {
			parts = append(parts, "not searched: "+reason)
		}
		if !answered[s.rc] {
			switch {
			case s.unfinished():
				parts = append(parts, fmt.Sprintf("its first index has not finished (%s), so results from it may be missing", s.fresh.Detail))
			case s.stale():
				parts = append(parts, fmt.Sprintf("results from it reflect its last completed index and may be stale (%s)", s.fresh.Detail))
			}
		}
		if len(parts) > 0 {
			notes = append(notes, s.label+": "+strings.Join(parts, "; ")+".")
		}
	}
	return notes
}

func shareCap(avail []int, limit int) []int {
	out := make([]int, len(avail))
	if limit <= 0 {
		return out
	}
	total := 0
	for _, n := range avail {
		total += max(n, 0)
	}
	if total <= limit {
		for i, n := range avail {
			out[i] = max(n, 0)
		}
		return out
	}
	lo, hi := 0, limit
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		used := 0
		for _, n := range avail {
			used += min(max(n, 0), mid)
		}
		if used <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	used := 0
	for i, n := range avail {
		out[i] = min(max(n, 0), lo)
		used += out[i]
	}
	for i, n := range avail {
		if used == limit {
			break
		}
		if n > lo {
			out[i]++
			used++
		}
	}
	return out
}

func gather[T any](rs []repoState, limit int, fetch func(repoState, int) ([]T, error)) ([][]T, []int, bool, error) {
	parts := make([][]T, len(rs))
	avail := make([]int, len(rs))
	for i, s := range rs {
		if s.empty {
			continue
		}
		part, err := fetch(s, limit)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%s: %w", s.label, err)
		}
		parts[i], avail[i] = part, len(part)
	}
	shares := shareCap(avail, limit)
	kept := 0
	more := false
	for i := range parts {
		if shares[i] < len(parts[i]) {
			parts[i] = parts[i][:shares[i]]
		}
		kept += len(parts[i])
		if avail[i] == limit && limit > 0 {
			more = true
		}
	}
	total := 0
	for _, n := range avail {
		total += n
	}
	return parts, shares, more || total > kept, nil
}

func servedArgs(a *app.App) string { return strings.Join(repoArgs(a, a.Repos), ", ") }

func servedLabels(rs []repoState) string {
	labels := make([]string, len(rs))
	for i, s := range rs {
		labels[i] = s.label
	}
	return strings.Join(labels, ", ")
}

func sharedCapHint(a *app.App) string {
	return "More entries may match: the limit is shared between the served repos. Pass repo as one of: " + servedArgs(a) + " to narrow the call to one repo."
}

func severalReposHint(name string, rs []repoState) string {
	args := make([]string, len(rs))
	for i, s := range rs {
		args[i] = fmt.Sprintf("%q", s.label)
	}
	return fmt.Sprintf("%s is in several served repos. Pass repo as one of: %s.", name, strings.Join(args, ", "))
}

func noSymbolHint(names, searched string) string {
	return fmt.Sprintf("No served repo has a symbol named %s (searched: %s).", names, searched)
}

func noFileHint(file, searched string) string {
	return fmt.Sprintf("No served repo has the file %s (searched: %s).", file, searched)
}

const noBaselineNote = "no prior working session has changed the code graph here — nothing to compare against."

const emptyRepoNote = "it holds no files Recarto indexes."
