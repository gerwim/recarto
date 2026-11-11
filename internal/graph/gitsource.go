package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gerwim/recarto/internal/parse"
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

// WorktreeToken returns a cheap fingerprint of root's git state used to skip
// reconcile when nothing changed. It mixes HEAD, `git status --porcelain`, and
// the mtime+size of every dirty file. Folding in dirty-file mtime/size makes the
// token exactly as precise as Reconcile's own mtime/size pre-check, so
// re-editing an already-dirty file (whose porcelain line stays "M <path>") still
// bumps the token. It returns "" when root is not a git repo (callers then
// always reconcile).
func WorktreeToken(root string) string {
	if _, err := exec.Command("git", "-C", root, "rev-parse", "--git-dir").Output(); err != nil {
		return "" // not a git repo
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		head = []byte("no-head") // repo with no commits yet
	}
	status, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()

	h := sha256.New()
	h.Write(head)
	h.Write(status)
	// Bounded by the number of dirty files. A stat that fails (deleted, or an
	// oddly quoted path) is skipped, degrading gracefully to the porcelain-only
	// token for that file.
	for _, path := range dirtyPaths(string(status)) {
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
			fmt.Fprintf(h, "\x00%s\x00%d\x00%d", path, info.ModTime().UnixNano(), info.Size())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// dirtyPaths extracts file paths from `git status --porcelain` output. Each line
// is "XY <path>" (two status columns + a space, so the path starts at index 3);
// a rename line "R  old -> new" yields the new path. Quoted paths (core.quotePath
// for special characters) are returned verbatim and may fail to stat, which
// WorktreeToken tolerates.
func dirtyPaths(status string) []string {
	var out []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+len(" -> "):]
		}
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}
