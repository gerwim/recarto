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

// Architecture is a bird's-eye structural map of a worktree.
type Architecture struct {
	Modules []Module
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
	_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
	return arch, nil
}
