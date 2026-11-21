package app

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Unselectable marks a served repo no repo argument reaches (see Selector).
const Unselectable = "the repo argument cannot select it"

// Within returns abs relative to worktree wt (slash-separated), ok=false when
// abs is not absolute or lies outside wt.
func Within(wt, abs string) (string, bool) {
	if wt == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	r, err := filepath.Rel(wt, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
}

// Innermost returns the index of the worktree in wts that abs lies in, -1 when
// none does. The longest wins: a nested checkout belongs to the inner repo.
func Innermost(wts []string, abs string) int {
	best := -1
	for i, wt := range wts {
		if _, in := Within(wt, abs); in && (best < 0 || len(wt) > len(wts[best])) {
			best = i
		}
	}
	return best
}

// OwnedFile is a real file a served repo owns.
type OwnedFile struct {
	Repo *RepoContext
	Rel  string // slash-separated, relative to Repo's worktree
}

// OwnedFiles returns the real files path names among repos, each with the repo
// that owns it: an absolute path is taken as given, a relative one is tried
// under each repo's worktree; symlinks are followed in path order, so
// "link/.." is the parent of the link's target; the real file belongs to the
// innermost repo whose real worktree contains it. Paths reaching one real file
// count once. A path whose real file does not exist, is a directory, or lies
// outside every repo names nothing. Ordered by the owner's position in repos,
// then by Rel.
func OwnedFiles(repos []*RepoContext, path string) []OwnedFile {
	wts := make([]string, len(repos))
	for i, rc := range repos {
		wts[i], _ = filepath.EvalSymlinks(rc.Git.Worktree) // A worktree that does not resolve holds no file to own.
	}
	candidates := make([]string, 0, len(repos))
	if filepath.IsAbs(path) {
		candidates = append(candidates, path)
	} else {
		for _, rc := range repos {
			// Joining would clean "link/.." away before the link is followed.
			candidates = append(candidates, rc.Git.Worktree+string(filepath.Separator)+path)
		}
	}
	seen := make(map[string]bool)
	out := make([]OwnedFile, 0, len(candidates))
	for _, candidate := range candidates {
		real, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue // A path that does not resolve names no file.
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		info, err := os.Stat(real)
		if err != nil || info.IsDir() {
			continue
		}
		i := Innermost(wts, real)
		if i < 0 {
			continue
		}
		rel, _ := Within(wts[i], real)
		out = append(out, OwnedFile{Repo: repos[i], Rel: rel})
	}
	order := make(map[*RepoContext]int, len(repos))
	for i, rc := range repos {
		order[rc] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if order[out[i].Repo] != order[out[j].Repo] {
			return order[out[i].Repo] < order[out[j].Repo]
		}
		return out[i].Rel < out[j].Rel
	})
	return out
}
