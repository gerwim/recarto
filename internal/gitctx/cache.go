package gitctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cacheMaxAge bounds how long a recorded identity is trusted without asking git.
// ponytail: the stamp below misses history rewritten onto a different root commit
// behind an unchanged branch name (RootCommit keys HTTP sync); the age cap bounds
// that staleness — stamp the branch tip too if it ever matters.
const cacheMaxAge = time.Hour

// stamp is the on-disk state an identity depends on: Top is the nearest
// directory holding a .git entry (the worktree root git discovers), Head the
// HEAD file that entry points at, and HeadContent the branch ref or detached
// commit HEAD names. The zero stamp means no .git anywhere above the directory.
type stamp struct {
	Top, Head, HeadContent string
}

// cachedIdentity is one recorded resolution of Dir.
type cachedIdentity struct {
	Dir     string
	Context Context
	Stamp   stamp
	At      int64 // unix ms
}

// ResolveCached is Resolve(r, dir) remembered across processes under cacheDir.
// Resolve costs four git spawns (one walks the whole history for the root
// commit) — the bulk of every short-lived hook's start-up. A recorded identity
// is reused while the checkout still matches it: the same .git entry found
// walking up from dir, pointing at the same HEAD file, with byte-identical
// content (so a checkout, a detached commit, a nested `git init` or a new repo
// around a non-git dir all re-resolve). Unborn repos (no root commit yet),
// reftable HEAD stubs and GIT_DIR-style overrides are never cached. Any cache
// fault falls back to Resolve.
func ResolveCached(cacheDir string, r Runner, dir string) (Context, error) {
	dir = filepath.Clean(dir)
	if cacheDir == "" || os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" || os.Getenv("GIT_COMMON_DIR") != "" {
		return Resolve(r, dir)
	}
	now, ok := stampOf(dir)
	if !ok {
		return Resolve(r, dir)
	}
	sum := sha256.Sum256([]byte(dir))
	path := filepath.Join(cacheDir, "gitctx", hex.EncodeToString(sum[:8])+".json")
	if b, err := os.ReadFile(path); err == nil {
		var e cachedIdentity
		if json.Unmarshal(b, &e) == nil && e.Dir == dir && e.Stamp == now &&
			time.Since(time.UnixMilli(e.At)) < cacheMaxAge && consistent(e.Context, now) {
			return e.Context, nil
		}
	}
	// The stamp was taken before git runs, so a checkout racing this resolve can
	// only leave an entry whose stamp is already stale — re-resolved next time.
	gc, err := Resolve(r, dir)
	if err == nil && consistent(gc, now) {
		writeEntry(path, cachedIdentity{Dir: dir, Context: gc, Stamp: now, At: time.Now().UnixMilli()})
	}
	return gc, err
}

// consistent reports whether a resolved identity agrees with the stamp, i.e.
// whether the stamp captures everything it depends on.
func consistent(gc Context, s stamp) bool {
	if gc.NonGit {
		return s == stamp{}
	}
	return s.Top != "" && sameDir(s.Top, gc.Worktree) && gc.RootCommit != "" &&
		s.HeadContent != "" && !strings.Contains(s.HeadContent, "/.invalid")
}

// stampOf walks up from dir to the first .git entry, as git discovery does.
// ok=false on an unexpected filesystem error (the caller then skips the cache).
func stampOf(dir string) (stamp, bool) {
	for d := dir; ; {
		dotgit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotgit)
		if err == nil {
			s := stamp{Top: d, Head: filepath.Join(dotgit, "HEAD")}
			if !fi.IsDir() { // linked worktree or submodule: "gitdir: <path>"
				b, err := os.ReadFile(dotgit)
				gitdir, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
				if err != nil || !found {
					return stamp{}, false
				}
				if gitdir = strings.TrimSpace(gitdir); !filepath.IsAbs(gitdir) {
					gitdir = filepath.Join(d, gitdir)
				}
				s.Head = filepath.Join(gitdir, "HEAD")
			}
			b, _ := os.ReadFile(s.Head)
			s.HeadContent = string(b)
			return s, true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return stamp{}, false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return stamp{}, true
		}
		d = parent
	}
}

// sameDir reports whether two paths name one directory: git prints its own
// spelling (forward slashes on Windows, symlinks resolved), so fall back to
// comparing the files themselves.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// writeEntry records e atomically (temp file + rename), best-effort: a
// concurrent writer or a read-only cache just means the next run resolves again.
func writeEntry(path string, e cachedIdentity) {
	b, err := json.Marshal(e)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
