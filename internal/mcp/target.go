package mcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gerwim/recarto/internal/app"
)

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
		if strings.EqualFold(rc.Git.RepoName, repo) ||
			strings.EqualFold(filepath.Base(rc.Git.Worktree), repo) ||
			strings.EqualFold(filepath.Clean(rc.Git.Worktree), filepath.Clean(repo)) {
			return rc, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, rc := range all {
		names = append(names, rc.Git.RepoName)
	}
	return nil, fmt.Errorf("unknown repo %q — serving: %s", repo, strings.Join(names, ", "))
}
