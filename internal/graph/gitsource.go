package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/parse"
)

// NewSource returns the best file Source for root: a git-aware source that honors
// .gitignore when root is inside a git repo, falling back to a plain filesystem
// walk (FSSource) otherwise.
func NewSource(root string) Source { return gitSource{root: root} }

// gitSource enumerates a worktree's supported files via `git ls-files`
// (tracked + untracked-but-not-ignored), so .gitignore is honored. If git is
// unavailable or root is not a repo, it falls back to FSSource.
type gitSource struct{ root string }

func (s gitSource) List() ([]FileEntry, error) {
	// --cached: tracked; --others: untracked; --exclude-standard: respect
	// .gitignore/.git/info/exclude; -z: NUL-separated (handles spaces).
	out, err := exec.Command("git", "-C", s.root,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return FSSource{Root: s.root}.List() // not a git repo (or git missing)
	}
	var entries []FileEntry
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		if _, ok := parse.LangForExt(rel); !ok {
			continue
		}
		info, statErr := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel)))
		if statErr != nil {
			continue // listed but gone (e.g. deleted but staged); skip
		}
		entries = append(entries, FileEntry{
			Path: rel, Mtime: info.ModTime().UnixMilli(), Size: info.Size(),
		})
	}
	return entries, nil
}

func (s gitSource) Read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.root, filepath.FromSlash(path)))
}

// WorktreeToken returns a cheap fingerprint of root's git state (HEAD + working-
// tree dirtiness) used to skip reconcile when nothing changed. It returns "" when
// root is not a git repo (callers then always reconcile).
func WorktreeToken(root string) string {
	if _, err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Output(); err != nil {
		return "" // not a git repo
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		head = []byte("no-head") // repo with no commits yet
	}
	status, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	sum := sha256.Sum256(append(head, status...))
	return hex.EncodeToString(sum[:])
}
