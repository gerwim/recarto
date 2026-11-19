package graph

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gerwim/recarto/internal/parse"
)

// FileEntry is a candidate source file discovered by a Source.
type FileEntry struct {
	Path  string // repo-relative, slash-separated
	Mtime int64  // unix ms
	Size  int64
}

// Source enumerates and reads a worktree's supported source files. It is an
// interface so the engine stays testable (and git-agnostic).
type Source interface {
	List() ([]FileEntry, error)
	Read(path string) ([]byte, error)
}

// skipDir is the directory denylist (build output, deps, VCS, IDE metadata,
// Python virtualenvs and caches).
var skipDir = map[string]bool{
	".git": true, "node_modules": true, "bin": true, "obj": true,
	"dist": true, "build": true, "vendor": true, ".vs": true, ".idea": true,
	".venv": true, "venv": true, ".tox": true, "__pycache__": true, "site-packages": true,
}

// FSSource walks the filesystem under Root, selecting files by supported
// extension and skipping the denylist directories. (.gitignore-honoring is a
// later refinement; the denylist covers the common noise.)
type FSSource struct{ Root string }

func (s FSSource) List() ([]FileEntry, error) {
	var out []FileEntry
	err := walkCode(context.Background(), s.Root, func(rel string, d fs.DirEntry) (bool, error) {
		info, err := d.Info()
		if err != nil {
			return false, err
		}
		out = append(out, FileEntry{Path: rel, Mtime: info.ModTime().UnixMilli(), Size: info.Size()})
		return false, nil
	})
	return out, err
}

// walkCode walks root as an index outside git does, calling fn with the
// slash-separated relative path of each file a supported language takes. It
// skips the denylist directories, and ends when fn reports stop or ctx ends.
func walkCode(ctx context.Context, root string, fn func(rel string, d fs.DirEntry) (stop bool, err error)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := parse.LangForExt(rel); !ok {
			return nil
		}
		stop, err := fn(rel, d)
		if err != nil {
			return err
		}
		if stop {
			return filepath.SkipAll
		}
		return nil
	})
}

func (s FSSource) Read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(path)))
}
