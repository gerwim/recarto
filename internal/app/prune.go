package app

import (
	"context"
	"os"

	"github.com/gerwim/recarto/internal/gitctx"
)

// DeadWorktrees returns the stored graph worktrees that no longer exist. A
// worktree is condemned only when BOTH hold: git's authoritative list
// (`git worktree list --porcelain`) does not name it, AND its path is gone
// from disk — so a path-form mismatch (symlinked roots) or a temporarily
// unavailable list can never condemn a live worktree. With no authoritative
// list (non-git, or the command fails) it reports none: pruning is an
// optimization, guessing is not.
func (a *App) DeadWorktrees(ctx context.Context) ([]string, error) {
	if a.Git.NonGit {
		return nil, nil
	}
	livePaths, err := gitctx.LiveWorktrees(a.Runner)
	if err != nil || len(livePaths) == 0 {
		return nil, nil
	}
	live := make(map[string]bool, len(livePaths))
	for _, p := range livePaths {
		live[p] = true
	}
	stored, err := a.Graph.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	var dead []string
	for _, wt := range stored {
		if live[wt] || wt == a.Git.Worktree {
			continue
		}
		if _, statErr := os.Stat(wt); os.IsNotExist(statErr) {
			dead = append(dead, wt)
		}
	}
	return dead, nil
}
