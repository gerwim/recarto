package graph

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// Module is a project-level grouping (the nearest ancestor directory holding a
// project marker — .csproj / package.json / go.mod — else the first path segment)
// with rollup counts.
type Module struct {
	Name    string
	Files   int
	Symbols int
}

// ModuleEdge is a dependency from one module to another (resolved references
// crossing a top-level-directory boundary), with the number of such references.
type ModuleEdge struct {
	From  string
	To    string
	Count int
}

// Architecture is a bird's-eye structural map of a worktree.
type Architecture struct {
	Modules []Module
	Deps    []ModuleEdge
}

// DefaultArchLimit caps both the module list and the dependency-edge list of an
// unbudgeted architecture map. On a large monorepo the dependency edges grow
// near-quadratically with module count (a 331-module repo yielded 4,404 edges,
// ~90k tokens) — almost all of it an incidental long tail of single-reference
// edges. Bounding to the highest-signal entries keeps orientation intact at a
// fraction of the tokens.
const DefaultArchLimit = 80

// BudgetArchitecture bounds an Architecture to at most `limit` modules and `limit`
// dependency edges, selecting the highest-signal entries — modules by symbol count,
// edges by reference count — then restoring name order within the kept subset so
// output ordering is stable. It returns the kept slices, the pre-cap totals, and
// whether anything was dropped. limit <= 0 uses DefaultArchLimit.
func BudgetArchitecture(arch Architecture, limit int) (mods []Module, deps []ModuleEdge, totalMods, totalDeps int, truncated bool) {
	if limit <= 0 {
		limit = DefaultArchLimit
	}
	totalMods, totalDeps = len(arch.Modules), len(arch.Deps)

	mods = append(mods, arch.Modules...)
	if len(mods) > limit {
		sort.SliceStable(mods, func(i, j int) bool { return mods[i].Symbols > mods[j].Symbols })
		mods = mods[:limit]
		truncated = true
	}
	sort.SliceStable(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })

	deps = append(deps, arch.Deps...)
	if len(deps) > limit {
		sort.SliceStable(deps, func(i, j int) bool { return deps[i].Count > deps[j].Count })
		deps = deps[:limit]
		truncated = true
	}
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].From != deps[j].From {
			return deps[i].From < deps[j].From
		}
		return deps[i].To < deps[j].To
	})
	return mods, deps, totalMods, totalDeps, truncated
}

// MapArchitecture selects the highest-signal slice of an Architecture for the
// compact session-start orientation map. It differs from BudgetArchitecture (which
// ranks modules by symbol count for the full get_architecture tool) in two ways
// that matter at a tight budget of a handful of modules:
//
//   - it ranks modules by FILE count, not symbols. Symbol count is inflated by
//     generated clients and symbol-dense languages — a 43-file codegen module can
//     carry 23k symbols — which crowds out the genuinely large hand-written modules
//     an agent needs to orient. File count is the more robust size signal.
//   - it demotes test modules below product modules, so the map leads with the
//     code under development rather than its test projects (mirroring how search
//     demotes test results).
//
// Within the kept set, modules are restored to name order and deps (ranked by
// reference count) likewise, so the displayed output is stable. limit <= 0 keeps
// everything. It never mutates arch.
func MapArchitecture(arch Architecture, limit int) (mods []Module, deps []ModuleEdge) {
	mods = append(mods, arch.Modules...)
	sort.SliceStable(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	sort.SliceStable(mods, func(i, j int) bool {
		if ti, tj := isTestModuleName(mods[i].Name), isTestModuleName(mods[j].Name); ti != tj {
			return !ti // product modules before test modules
		}
		return mods[i].Files > mods[j].Files
	})
	if limit > 0 && len(mods) > limit {
		mods = mods[:limit]
	}
	sort.SliceStable(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })

	deps = append(deps, arch.Deps...)
	sort.SliceStable(deps, func(i, j int) bool {
		// Demote edges touching a test module: "tests reference the code under test"
		// is the heaviest edge in many repos yet says nothing about the product's
		// architecture. Product→product coupling is the signal worth the budget.
		ti := isTestModuleName(deps[i].From) || isTestModuleName(deps[i].To)
		tj := isTestModuleName(deps[j].From) || isTestModuleName(deps[j].To)
		if ti != tj {
			return !ti
		}
		return deps[i].Count > deps[j].Count
	})
	if limit > 0 && len(deps) > limit {
		deps = deps[:limit]
	}
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].From != deps[j].From {
			return deps[i].From < deps[j].From
		}
		return deps[i].To < deps[j].To
	})
	return mods, deps
}

// projectRoots returns directories (relative to worktree, slash-separated,
// longest-first) that hold a project marker (.csproj / package.json / go.mod).
// They let a monorepo be grouped at project granularity instead of collapsing
// every nested project under one top-level directory. A non-existent worktree
// (e.g. a synthetic test path) yields no roots, so moduleOf falls back to the
// first path segment.
func projectRoots(worktree string) []string {
	var roots []string
	_ = filepath.WalkDir(worktree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "bin", "obj", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == "package.json" || name == "go.mod" || strings.HasSuffix(name, ".csproj") {
			if rel, relErr := filepath.Rel(worktree, filepath.Dir(p)); relErr == nil {
				roots = append(roots, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return roots
}

// moduleNames assigns each project root a display name: its basename when that
// basename is unique among the roots, else its full relative path so that two
// distinct projects sharing a basename (e.g. backend/App and frontend/App) stay
// separate modules instead of silently merging.
func moduleNames(roots []string) map[string]string {
	count := map[string]int{}
	for _, r := range roots {
		if r != "." {
			count[filepath.Base(r)]++
		}
	}
	names := map[string]string{}
	for _, r := range roots {
		if r == "." {
			continue
		}
		if b := filepath.Base(r); count[b] == 1 {
			names[r] = b
		} else {
			names[r] = r
		}
	}
	return names
}

// moduleOf returns the module a path belongs to: the (collision-disambiguated)
// name of the nearest ancestor project root, or the first path segment ("." for a
// top-level file) when no deeper project marker applies. A repo-root marker (".")
// is skipped so it never flattens everything into one bucket.
func moduleOf(roots []string, names map[string]string, path string) string {
	for _, r := range roots {
		if r == "." {
			continue
		}
		if path == r || strings.HasPrefix(path, r+"/") {
			return names[r]
		}
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// Architecture summarizes the worktree's modules: file and symbol counts per
// top-level directory. scope, if non-empty, restricts to paths under that prefix.
func (e *Engine) Architecture(ctx context.Context, worktree, scope string) (Architecture, error) {
	roots := projectRoots(worktree)
	modNames := moduleNames(roots)
	q := `SELECT f.path, (SELECT COUNT(*) FROM nodes n WHERE n.file_id = f.id)
	        FROM files f WHERE f.worktree = ?`
	args := []any{worktree}
	if scope != "" {
		q += " AND f.path LIKE ?"
		args = append(args, scope+"%")
	}
	rows, err := e.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return Architecture{}, err
	}
	defer rows.Close()
	type agg struct{ files, syms int }
	byMod := map[string]*agg{}
	for rows.Next() {
		var path string
		var syms int
		if err := rows.Scan(&path, &syms); err != nil {
			return Architecture{}, err
		}
		m := moduleOf(roots, modNames, path)
		if byMod[m] == nil {
			byMod[m] = &agg{}
		}
		byMod[m].files++
		byMod[m].syms += syms
	}
	if err := rows.Err(); err != nil {
		return Architecture{}, err
	}
	var arch Architecture
	for name, a := range byMod {
		arch.Modules = append(arch.Modules, Module{Name: name, Files: a.files, Symbols: a.syms})
	}
	sort.Slice(arch.Modules, func(i, j int) bool { return arch.Modules[i].Name < arch.Modules[j].Name })

	// Deps: inter-module dependency edges derived from resolved cross-module
	// references. Deps are computed worktree-wide (scope only applies to Modules
	// above). This is intentional: module boundaries are top-level directories
	// and scope filtering of Deps would silently drop edges that cross the scope
	// boundary — easier to reason about with the full picture.
	depRows, err := e.st.DB.QueryContext(ctx, `
SELECT srcf.path, dstf.path, COUNT(*)
  FROM edges ed
  JOIN nodes sn   ON sn.id = ed.src_node_id
  JOIN files srcf ON srcf.id = sn.file_id
  JOIN nodes dn   ON dn.id = ed.dst_node_id
  JOIN files dstf ON dstf.id = dn.file_id
 WHERE srcf.worktree = ? AND dstf.worktree = ?
 GROUP BY srcf.path, dstf.path`, worktree, worktree)
	if err != nil {
		return Architecture{}, err
	}
	depCount := map[[2]string]int{}
	for depRows.Next() {
		var sp, dp string
		var c int
		if err := depRows.Scan(&sp, &dp, &c); err != nil {
			depRows.Close()
			return Architecture{}, err
		}
		sm, dm := moduleOf(roots, modNames, sp), moduleOf(roots, modNames, dp)
		if sm != dm {
			depCount[[2]string{sm, dm}] += c
		}
	}
	if err := depRows.Err(); err != nil {
		depRows.Close()
		return Architecture{}, err
	}
	depRows.Close()
	for k, c := range depCount {
		arch.Deps = append(arch.Deps, ModuleEdge{From: k[0], To: k[1], Count: c})
	}
	sort.Slice(arch.Deps, func(i, j int) bool {
		if arch.Deps[i].From != arch.Deps[j].From {
			return arch.Deps[i].From < arch.Deps[j].From
		}
		return arch.Deps[i].To < arch.Deps[j].To
	})

	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return arch, nil
}
