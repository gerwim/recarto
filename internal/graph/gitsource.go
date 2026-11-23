package graph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gerwim/recarto/internal/parse"
)

// AttemptEnv is set on every git call an index attempt makes while it holds the
// worktree's index lock, to a value unique to that attempt. Recarto never reads
// it: it lets a git wrapper tell an attempt's calls from file discovery.
const AttemptEnv = "RECARTO_INDEX_ATTEMPT"

// gitCmd is `git -C root args…`, marked as part of attempt when attempt is set.
func gitCmd(ctx context.Context, attempt, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	// A child of a git wrapper can keep git's output open after git is gone; a cancelled call must not wait for it.
	cmd.WaitDelay = 100 * time.Millisecond
	if attempt != "" {
		cmd.Env = append(os.Environ(), AttemptEnv+"="+attempt)
	}
	return cmd
}

// NewSource returns the best file Source for root: a git-aware source that honors
// .gitignore when root is inside a git repo, falling back to a plain filesystem
// walk (FSSource) otherwise.
func NewSource(root string) Source { return gitSource{root: root} }

// gitSource enumerates a worktree's supported files via `git ls-files`
// (tracked + untracked-but-not-ignored), so .gitignore is honored. Outside a
// git repo (or without git) it falls back to FSSource. attempt marks its git
// calls as part of an index attempt.
type gitSource struct{ root, attempt string }

// inAttempt returns src with its git calls marked as part of attempt.
func inAttempt(src Source, attempt string) Source {
	if s, ok := src.(gitSource); ok {
		s.attempt = attempt
		return s
	}
	return src
}

// lsFiles lists root's tracked and untracked-but-not-ignored files, limited to
// paths when given.
func lsFiles(ctx context.Context, root, attempt string, paths ...string) ([]string, error) {
	// --cached: tracked; --others: untracked; --exclude-standard: respect
	// .gitignore/.git/info/exclude; -z: NUL-separated (handles spaces).
	args := []string{"--literal-pathspecs", "ls-files", "--cached", "--others", "--exclude-standard", "-z"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	var stderr bytes.Buffer
	cmd := gitCmd(ctx, attempt, root, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git ls-files in %s: %w: %s", root, err, msg)
		}
		return nil, fmt.Errorf("git ls-files in %s: %w", root, err)
	}
	var rels []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			rels = append(rels, rel)
		}
	}
	return rels, nil
}

// indexable reports whether rel, as git lists it under root, is a file an
// index takes in, with its entry.
func indexable(root, rel string) (FileEntry, bool) {
	if _, ok := parse.LangForExt(rel); !ok {
		return FileEntry{}, false
	}
	info, err := fileAt(root, rel)
	if err != nil || info == nil {
		// Listed but gone (e.g. deleted but staged), or a stat failure: the
		// index could not read it either.
		return FileEntry{}, false
	}
	return FileEntry{Path: rel, Mtime: info.ModTime().UnixMilli(), Size: info.Size()}, true
}

// fileAt returns the file at slash-separated rel under root, or nil when the
// path holds none: it is missing, an ancestor of it is no longer a directory,
// or it is a directory. Any other failure to stat it is returned.
func fileAt(root, rel string) (fs.FileInfo, error) {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return nil, nil
	case err != nil:
		return nil, err
	case info.IsDir():
		return nil, nil
	}
	return info, nil
}

// inGitRepo reports whether root lies in a git repository.
func inGitRepo(ctx context.Context, root, attempt string) bool {
	if gitCmd(ctx, attempt, root, "rev-parse", "--git-dir").Run() == nil {
		return true
	}
	// git itself may be what fails; a .git entry at root or above still shows
	// a repository, whose listing a walk would get wrong.
	for dir := root; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func (s gitSource) List() ([]FileEntry, error) {
	ctx := context.Background()
	rels, err := lsFiles(ctx, s.root, s.attempt)
	if err != nil {
		// Inside git a failed listing proves nothing about the files; walking
		// instead would index what .gitignore hides.
		if inGitRepo(ctx, s.root, s.attempt) {
			return nil, err
		}
		return FSSource{Root: s.root}.List()
	}
	var entries []FileEntry
	for _, rel := range rels {
		if fe, ok := indexable(s.root, rel); ok {
			entries = append(entries, fe)
		}
	}
	return entries, nil
}

func (s gitSource) Read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.root, filepath.FromSlash(path)))
}

// HasCode reports whether root holds a file an index of root would take in, listing in root's
// git scope as the index does — git ls-files inside git, the filesystem walk outside — and stops
// at the first such file. Inside git, candidates (paths the caller believes hold code) are
// listed first in one narrow call; only if none is still listed is the whole worktree listed.
// Outside git the walk decides. Its git calls carry no attempt marker. An error means root
// could not be listed, which proves nothing.
func HasCode(ctx context.Context, root string, nonGit bool, candidates ...string) (bool, error) {
	if nonGit {
		found := false
		err := walkCode(ctx, root, func(string, fs.DirEntry) (bool, error) {
			found = true
			return true, nil
		})
		return found, err
	}
	holdsCode := func(rels []string) bool {
		for _, rel := range rels {
			if _, ok := indexable(root, rel); ok {
				return true
			}
		}
		return false
	}
	if len(candidates) > 0 {
		rels, err := lsFiles(ctx, root, "", candidates...)
		if err != nil {
			return false, err
		}
		if holdsCode(rels) {
			return true, nil
		}
	}
	rels, err := lsFiles(ctx, root, "")
	if err != nil {
		return false, err
	}
	return holdsCode(rels), nil
}

// WorktreeToken returns a cheap fingerprint of root's git state used to skip
// reconcile when nothing changed. It mixes HEAD, `git status --porcelain`, and
// the mtime+size of every dirty file. Folding in dirty-file mtime/size makes the
// token exactly as precise as Reconcile's own mtime/size pre-check, so
// re-editing an already-dirty file (whose porcelain line stays "M <path>") still
// bumps the token. It returns "" when root is not a git repo (callers then
// always reconcile). attempt marks its git calls as part of an index attempt.
func WorktreeToken(root, attempt string) string {
	ctx := context.Background()
	if gitCmd(ctx, attempt, root, "rev-parse", "--git-dir").Run() != nil {
		return "" // not a git repo
	}
	head, err := gitCmd(ctx, attempt, root, "rev-parse", "HEAD").Output()
	if err != nil {
		head = []byte("no-head") // repo with no commits yet
	}
	status, _ := gitCmd(ctx, attempt, root, "status", "--porcelain").Output()

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
