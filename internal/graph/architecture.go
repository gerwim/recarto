package graph

import (
	"context"
	"sort"
	"strings"
)

// Module is a top-level grouping (first path segment) with rollup counts.
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

// moduleOf returns the first path segment ("." for a top-level file).
func moduleOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// Architecture summarizes the worktree's modules: file and symbol counts per
// top-level directory. scope, if non-empty, restricts to paths under that prefix.
func (e *Engine) Architecture(ctx context.Context, worktree, scope string) (Architecture, error) {
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
		m := moduleOf(path)
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
		sm, dm := moduleOf(sp), moduleOf(dp)
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
