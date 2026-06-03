// Package gitctx resolves the git identity of the working directory:
// a stable per-repo id (shared across worktrees), branch, worktree, and root commit.
package gitctx

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
)

// Context describes the resolved repo/worktree/branch identity.
type Context struct {
	RepoID     string // 16 hex chars; stable across worktrees of the same repo
	RepoName   string // human-readable (basename of worktree root or cwd)
	Branch     string // current branch, "detached@<sha>", or "(none)"
	Worktree   string // absolute worktree root (distinct per worktree)
	RootCommit string // first root commit sha (empty if non-git)
	NonGit     bool   // true when not inside a git repo
}

// Runner runs `git <args...>` and returns trimmed stdout.
type Runner interface {
	Run(args ...string) (string, error)
}

// Real runs git via os/exec inside dir.
type Real struct{ Dir string }

// Run executes git in r.Dir and returns trimmed combined stdout.
func (r Real) Run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func repoIDFromCommonDir(commonDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(commonDir)))
	return hex.EncodeToString(sum[:])[:16]
}

// Resolve derives the identity for cwd using the given Runner.
func Resolve(r Runner, cwd string) (Context, error) {
	commonDir, err := r.Run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || commonDir == "" {
		// Non-git fallback: hash the absolute cwd.
		sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
		return Context{
			RepoID:   hex.EncodeToString(sum[:])[:16],
			RepoName: filepath.Base(cwd),
			Branch:   "(none)",
			Worktree: cwd,
			NonGit:   true,
		}, nil
	}

	ctx := Context{RepoID: repoIDFromCommonDir(commonDir)}

	if top, err := r.Run("rev-parse", "--show-toplevel"); err == nil && top != "" {
		ctx.Worktree = top
		ctx.RepoName = filepath.Base(top)
	} else {
		ctx.Worktree = cwd
		ctx.RepoName = filepath.Base(cwd)
	}

	branch, err := r.Run("symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || branch == "" {
		sha, _ := r.Run("rev-parse", "--short", "HEAD")
		if sha == "" {
			sha = "unknown"
		}
		ctx.Branch = "detached@" + sha
	} else {
		ctx.Branch = branch
	}

	if root, err := r.Run("rev-list", "--max-parents=0", "HEAD"); err == nil {
		// A repo can have multiple root commits; take the first line.
		if i := strings.IndexByte(root, '\n'); i >= 0 {
			root = root[:i]
		}
		ctx.RootCommit = strings.TrimSpace(root)
	}

	return ctx, nil
}
