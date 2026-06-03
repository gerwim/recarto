package memory

import (
	"path/filepath"
	"strings"
)

// RelFileRef normalizes a file path to the repo-relative, forward-slash form used
// as a memory_refs file value. An absolute path under worktree becomes relative;
// anything else (already relative, or outside the worktree) is returned slash-
// normalized. This lets a bug recorded as "internal/foo.go" match a hook that
// reports the absolute path "/abs/worktree/internal/foo.go".
func RelFileRef(worktree, path string) string {
	if worktree != "" && filepath.IsAbs(path) {
		if rel, err := filepath.Rel(worktree, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
