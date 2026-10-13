package graph

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Module is a project-level grouping (the nearest ancestor directory holding a
// project marker — .csproj / package.json / go.mod / Cargo.toml — else the first
// path segment) with rollup counts.
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
	// GeneratedFrom is the subset of Count whose source file is machine-generated
	// (isGeneratedPath). An edge dominated by generated-source references is
	// codegen noise rather than product architecture, so the session-start map
	// demotes it (see MapArchitecture).
	GeneratedFrom int
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
		// Then demote edges dominated by generated-source references: a generated
		// client's thousands of refs into a shared abstractions module is the
		// heaviest edge in many monorepos yet is codegen noise, not architecture.
		gi, gj := genDominatedEdge(deps[i]), genDominatedEdge(deps[j])
		if gi != gj {
			return !gi
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

	// Backfill: ensure every endpoint of a displayed edge appears in the module
	// list, so the map never points at a module it never introduced. Module
	// selection ranks by file count while edges rank by reference count, so on
	// large monorepos a coupling hub (many refs, few files) can anchor the
	// heaviest edge yet fall out of the top-N list — leaving a dangling endpoint
	// the agent has no context for. Coherent maps (the common case) add nothing.
	// Endpoints with no Module entry are left as-is (rare; the edge still shows).
	present := make(map[string]bool, len(mods))
	for _, m := range mods {
		present[m.Name] = true
	}
	byName := make(map[string]Module, len(arch.Modules))
	for _, m := range arch.Modules {
		byName[m.Name] = m
	}
	for _, d := range deps {
		for _, end := range [2]string{d.From, d.To} {
			if present[end] {
				continue
			}
			if m, ok := byName[end]; ok {
				mods = append(mods, m)
				present[end] = true
			}
		}
	}
	sort.SliceStable(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })

	return mods, deps
}

// genDominatedEdge reports whether a majority of an edge's references originate
// in generated source files, marking it codegen noise rather than product
// coupling. A minority of generated refs leaves a genuine edge unpenalized.
func genDominatedEdge(e ModuleEdge) bool {
	return e.GeneratedFrom*2 > e.Count
}

// projectRoots returns directories (relative to worktree, slash-separated,
// longest-first) that hold a project marker (.csproj / package.json / go.mod /
// Cargo.toml).
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
			case "node_modules", "bin", "obj", ".git", "target":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == "package.json" || name == "go.mod" || name == "Cargo.toml" || strings.HasSuffix(name, ".csproj") {
			if rel, relErr := filepath.Rel(worktree, filepath.Dir(p)); relErr == nil {
				roots = append(roots, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return roots
}

// genericNameSegments are path segments that carry no disambiguating signal and
// only inflate a module's display name — the conventional "src" wrapper in a
// .NET/monorepo layout (".../<area>/src/Types"). They are dropped when computing a
// disambiguated name, unless dropping them would merge two distinct roots.
var genericNameSegments = map[string]bool{"src": true}

// moduleNames assigns each project root a compact, unambiguous display name. A root
// whose basename is unique among the roots keeps just that basename. Roots sharing a
// basename (e.g. several ".../<area>/src/Types") are disambiguated by the shortest
// trailing path suffix that is unique within the colliding group, after dropping
// generic wrapper segments ("src") — so "src/HotChocolate/Core/src/Types" renders as
// "Core/Types", not its full path. If dropping "src" would make two distinct roots
// collide, the group falls back to full relative paths rather than silently merge two
// modules into one. The repo-root marker (".") is skipped.
func moduleNames(roots []string) map[string]string {
	byBase := map[string][]string{}
	for _, r := range roots {
		if r == "." {
			continue
		}
		b := filepath.Base(r)
		byBase[b] = append(byBase[b], r)
	}
	names := map[string]string{}
	for _, group := range byBase {
		if len(group) == 1 {
			names[group[0]] = filepath.Base(group[0])
			continue
		}
		// Colliding basename: name each by the shortest distinguishing suffix of its
		// cleaned segments. Detect first whether cleaning collapses two distinct roots
		// to the same segment list; if so the suffix scheme cannot tell them apart, so
		// the whole group falls back to full paths.
		cleaned := make(map[string][]string, len(group))
		seen := make(map[string]string, len(group))
		ambiguous := false
		for _, r := range group {
			segs := cleanNameSegments(strings.Split(r, "/"))
			cleaned[r] = segs
			key := strings.Join(segs, "/")
			if other, ok := seen[key]; ok && other != r {
				ambiguous = true
			}
			seen[key] = r
		}
		if ambiguous {
			for _, r := range group {
				names[r] = r
			}
			continue
		}
		for _, r := range group {
			names[r] = shortestUniqueSuffix(r, cleaned, group)
		}
	}
	return names
}

// cleanNameSegments drops generic wrapper segments (genericNameSegments) from all but
// the final position, so the basename is always preserved even if it is itself generic
// (a project directory literally named "src").
func cleanNameSegments(segs []string) []string {
	out := make([]string, 0, len(segs))
	for i, s := range segs {
		if i < len(segs)-1 && genericNameSegments[s] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// shortestUniqueSuffix returns the shortest trailing-segment suffix of r's cleaned
// segments that is unique among every other root in the group, joined by "/". Because
// the group shares a basename the result is always at least two segments. The caller's
// ambiguity guard guarantees a unique suffix exists within the full segment list.
func shortestUniqueSuffix(r string, cleaned map[string][]string, group []string) string {
	segs := cleaned[r]
	for k := 1; k <= len(segs); k++ {
		suffix := strings.Join(segs[len(segs)-k:], "/")
		unique := true
		for _, other := range group {
			if other == r {
				continue
			}
			os := cleaned[other]
			if len(os) >= k && strings.Join(os[len(os)-k:], "/") == suffix {
				unique = false
				break
			}
		}
		if unique {
			return suffix
		}
	}
	return strings.Join(segs, "/")
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

// archCacheVersion stamps the cached architecture payload so a binary with a
// different computation never serves a predecessor's cache.
const archCacheVersion = 2

// archCachePayload is the meta-table cache entry: the full architecture, valid
// exactly while the worktree's graph token equals Token.
type archCachePayload struct {
	Version int          `json:"version"`
	Token   string       `json:"token"`
	Arch    Architecture `json:"arch"`
}

// Architecture summarizes the worktree's modules: file and symbol counts per
// top-level directory. scope, if non-empty, restricts to paths under that prefix.
//
// The unscoped form is cached in the meta table keyed by the worktree's graph
// token — the same freshness signal ReconcileIfStale maintains — so repeat calls
// (every session start renders the orientation map; agents re-orient often) skip
// the filesystem walk and full edge aggregation while the graph is unchanged. A
// non-git worktree never stores a token and so never caches.
func (e *Engine) Architecture(ctx context.Context, worktree, scope string) (Architecture, error) {
	if scope != "" {
		return e.architectureUncached(ctx, worktree, scope)
	}
	var token string
	_ = e.st.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key=?`, "graph_token:"+worktree).Scan(&token)
	cacheKey := "arch_cache:" + worktree
	if token != "" {
		var raw string
		if err := e.st.DB.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key=?`, cacheKey).Scan(&raw); err == nil {
			var p archCachePayload
			if json.Unmarshal([]byte(raw), &p) == nil && p.Version == archCacheVersion && p.Token == token {
				_ = e.led.Incr(ctx, "lifetime", "graph_queries", 1)
				return p.Arch, nil
			}
		}
	}
	arch, err := e.architectureUncached(ctx, worktree, scope)
	if err != nil {
		return arch, err
	}
	if token != "" {
		// Best-effort: a busy store just means the next call recomputes. The payload
		// is keyed by the token read BEFORE computing, so a reconcile racing this
		// write leaves a cache entry whose token no longer matches — never served.
		if raw, jerr := json.Marshal(archCachePayload{Version: archCacheVersion, Token: token, Arch: arch}); jerr == nil {
			_ = e.st.WriteTx(ctx, func(c store.Conn) error {
				_, werr := c.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?)
				 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, cacheKey, string(raw))
				return werr
			})
		}
	}
	return arch, nil
}

// architectureUncached computes the architecture from the store. The dependency
// aggregation streams the edges table once and joins via in-memory maps —
// the SQL join form (edges×nodes×files×nodes×files with a path-pair GROUP BY)
// did an index seek per edge and dominated the 2s arch latency on large repos.
func (e *Engine) architectureUncached(ctx context.Context, worktree, scope string) (Architecture, error) {
	roots := projectRoots(worktree)
	modNames := moduleNames(roots)
	// moduleOf is O(roots) prefix matching and the same paths recur across the
	// per-file and per-edge loops; memoize it (and the generated-path test, which
	// runs per edge below).
	modMemo := map[string]string{}
	modOf := func(p string) string {
		if m, ok := modMemo[p]; ok {
			return m
		}
		m := moduleOf(roots, modNames, p)
		modMemo[p] = m
		return m
	}
	genMemo := map[string]bool{}
	genOf := func(p string) bool {
		if g, ok := genMemo[p]; ok {
			return g
		}
		g := isGeneratedPath(p)
		genMemo[p] = g
		return g
	}

	// Worktree files with their per-file symbol counts; filePath additionally
	// feeds the dep aggregation below (which is worktree-wide regardless of scope).
	filePath := map[string]string{}
	type agg struct{ files, syms int }
	byMod := map[string]*agg{}
	rows, err := e.st.DB.QueryContext(ctx,
		`SELECT f.id, f.path, (SELECT COUNT(*) FROM nodes n WHERE n.file_id = f.id)
		   FROM files f WHERE f.worktree = ?`, worktree)
	if err != nil {
		return Architecture{}, err
	}
	for rows.Next() {
		var id, path string
		var syms int
		if err := rows.Scan(&id, &path, &syms); err != nil {
			rows.Close()
			return Architecture{}, err
		}
		filePath[id] = path
		if scope != "" && !strings.HasPrefix(path, scope) {
			continue
		}
		m := modOf(path)
		if byMod[m] == nil {
			byMod[m] = &agg{}
		}
		byMod[m].files++
		byMod[m].syms += syms
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Architecture{}, err
	}
	rows.Close()
	var arch Architecture
	for name, a := range byMod {
		arch.Modules = append(arch.Modules, Module{Name: name, Files: a.files, Symbols: a.syms})
	}
	sort.Slice(arch.Modules, func(i, j int) bool { return arch.Modules[i].Name < arch.Modules[j].Name })

	// node id -> file id, for resolving an edge's target file without per-edge seeks.
	nodeFile := make(map[string]string, 1<<17)
	rows, err = e.st.DB.QueryContext(ctx, `SELECT id, file_id FROM nodes`)
	if err != nil {
		return Architecture{}, err
	}
	for rows.Next() {
		var id, fid string
		if err := rows.Scan(&id, &fid); err != nil {
			rows.Close()
			return Architecture{}, err
		}
		nodeFile[id] = fid
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Architecture{}, err
	}
	rows.Close()

	// Deps: inter-module dependency edges derived from resolved cross-module
	// references. Deps are computed worktree-wide (scope only applies to Modules
	// above). This is intentional: module boundaries are top-level directories
	// and scope filtering of Deps would silently drop edges that cross the scope
	// boundary — easier to reason about with the full picture. The worktree
	// membership test for both endpoints is the filePath map (built unscoped
	// above), mirroring the old SQL's srcf.worktree/dstf.worktree filters.
	// src_node_id IS NOT NULL mirrors the old form's inner join on the source
	// node: a file-scope reference (an import, or a call outside any symbol) has
	// no source symbol and never counted as a module dependency.
	depCount := map[[2]string]int{}
	depGen := map[[2]string]int{} // refs whose source file is generated, per edge
	rows, err = e.st.DB.QueryContext(ctx,
		`SELECT file_id, dst_node_id FROM edges
		  WHERE dst_node_id IS NOT NULL AND src_node_id IS NOT NULL`)
	if err != nil {
		return Architecture{}, err
	}
	for rows.Next() {
		var srcFile, dstNode string
		if err := rows.Scan(&srcFile, &dstNode); err != nil {
			rows.Close()
			return Architecture{}, err
		}
		sp, ok := filePath[srcFile]
		if !ok {
			continue // edge from another worktree
		}
		dp, ok := filePath[nodeFile[dstNode]]
		if !ok {
			continue // target outside this worktree
		}
		sm, dm := modOf(sp), modOf(dp)
		if sm != dm {
			key := [2]string{sm, dm}
			depCount[key]++
			if genOf(sp) {
				depGen[key]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Architecture{}, err
	}
	rows.Close()
	for k, c := range depCount {
		arch.Deps = append(arch.Deps, ModuleEdge{From: k[0], To: k[1], Count: c, GeneratedFrom: depGen[k]})
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
