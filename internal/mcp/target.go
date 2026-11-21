package mcp

import (
	"fmt"
	"strings"

	"github.com/gerwim/recarto/internal/app"
)

// servedRepos returns the repos replies list as served: the RECARTO_REPOS
// repos, and the session's own folder too unless it is a code-less one.
func servedRepos(a *app.App) []*app.RepoContext {
	if a.CodelessWithRepos() {
		return a.Repos
	}
	return a.AllRepos()
}

// repoLabel names rc in a reply. From a code-less folder it is rc's selector,
// the repo value that reaches rc alone; elsewhere it is its name, as before.
func repoLabel(a *app.App, rc *app.RepoContext) string {
	if !a.CodelessWithRepos() {
		return rc.Git.RepoName
	}
	if sel, ok := a.Selector(rc); ok {
		return sel
	}
	return rc.Git.Worktree + " (" + app.Unselectable + ")"
}

// repoArgs returns the quoted repo values that reach each repo alone, in order.
func repoArgs(a *app.App, repos []*app.RepoContext) []string {
	var out []string
	for _, rc := range repos {
		if sel, ok := a.Selector(rc); ok {
			out = append(out, fmt.Sprintf("%q", sel))
		}
	}
	return out
}

// repoTarget resolves which served repo a tool call operates on: the primary
// (cwd-resolved) repo when `repo` is empty, else the served repo — primary or
// RECARTO_REPOS extra — whose name, worktree basename, or worktree path
// matches. An unknown name errors with the list of served repos, so the agent
// can self-correct instead of silently querying the wrong graph.
func repoTarget(a *app.App, repo string) (*app.RepoContext, error) {
	all := a.AllRepos()
	if repo == "" {
		return all[0], nil
	}
	for _, rc := range all {
		if rc.AnswersTo(repo) {
			return rc, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, rc := range servedRepos(a) {
		names = append(names, repoLabel(a, rc))
	}
	return nil, fmt.Errorf("unknown repo %q — serving: %s", repo, strings.Join(names, ", "))
}
