// Package app wires Recarto's per-process runtime in one place: it resolves the
// git/worktree identity, opens the shared per-repo store, and constructs the
// memory engine and ledger. Every surface (the CLI now; the MCP server and hooks
// in Phase 1C-2) builds an App and works through it, so the wiring lives here and
// nowhere else.
package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/features"
	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/remotesync"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
	"git.gerwim.nl/gerwim/recarto/internal/workspace"
)

// Config configures App construction. The zero value is valid: it resolves the
// real working directory, real git, and the real clock.
type Config struct {
	CWD    string        // "" → os.Getwd()
	Runner gitctx.Runner // nil → gitctx.Real{Dir: CWD}
	Clock  clock.Clock   // nil → clock.Real{}
	// QuickCheck enables the store's proactive corruption check at open time
	// (store.WithQuickCheck) for every store this App opens: the primary repo,
	// the RECARTO_REPOS extras and the workspace store. Only long-lived serving
	// processes set it — `recarto serve` — so a session starts on a store that
	// has been proven readable rather than discovering damage on its first write.
	// One-shot commands and hooks leave it off and stay as cheap as before.
	QuickCheck bool
	// SkipWorkspace leaves App.Workspace nil, so its store is never opened —
	// for the per-tool hooks (post-read, post-edit, augment), which never touch
	// it and run on nearly every tool call.
	SkipWorkspace bool
}

// RepoContext is one served repo root with its own store, engine and graph.
// The primary (cwd-resolved) repo is exposed as one via AllRepos; the extras
// come from RECARTO_REPOS and open the SAME store a session cd'd into that
// repo would (the store key is the git common dir, not the serving process's
// cwd) — so a repo-less session reads and writes exactly the memories the
// cd'd sessions see.
type RepoContext struct {
	Git    gitctx.Context
	Store  *store.Store
	Ledger *ledger.Ledger
	Engine *memory.Engine
	Graph  *graph.Engine
	Sync   *remotesync.Syncer // nil when unconfigured
}

// App is the resolved per-process runtime shared by every surface.
type App struct {
	Git    gitctx.Context
	Runner gitctx.Runner // the git runner identity was resolved with (worktree GC reuses it)
	Store  *store.Store
	Ledger *ledger.Ledger
	Engine *memory.Engine
	Graph  *graph.Engine
	Clock  clock.Clock
	// Repos are the extra repo roots served by this process (RECARTO_REPOS),
	// beyond the cwd-resolved primary. Empty unless explicitly configured —
	// the feature exists for sessions that are not cd'd into (all of) the
	// repos they work with, e.g. a multi-repo verifier running from a
	// scratch directory.
	Repos []*RepoContext
	// Sync is the HTTP memory-sync client, or nil when unconfigured (no
	// RECARTO_SYNC_URL/RECARTO_SYNC_KEY, or the repo has no resolvable
	// cross-machine identity). Every caller must nil-check before use — sync is
	// entirely optional, and its absence must never be an error.
	Sync *remotesync.Syncer
	// Workspace is the optional cross-repo workspace store (RECARTO_WORKSPACE),
	// or nil when unconfigured. Like Sync, every caller must nil-check: the
	// feature is opt-in and its absence must never be an error.
	Workspace *workspace.Bundle
	// Features are the user's feature switches (features.Load), read once at
	// Open: hooks and MCP handlers consult them, and tests set them directly.
	Features features.Set
	// SyncTrigger is signaled (non-blocking, via SignalSync) by write paths that
	// want a long-lived `recarto serve` process to sync sooner than its next
	// periodic tick — e.g. an MCP record_memory/complete_todo/forget_memory call,
	// so an interactive session converges within seconds rather than waiting up
	// to RECARTO_SYNC_INTERVAL. Always allocated (even when Sync is nil) so
	// callers never need a nil check before signaling; nothing consumes it when
	// sync is unconfigured.
	SyncTrigger chan struct{}
}

// SignalSync requests an out-of-band sync cycle from a long-lived process's
// background sync loop (see mcp.Serve) as soon as possible, instead of
// waiting for the next periodic tick. Non-blocking: the channel is buffered
// to depth 1, and a pending signal already covers any additional calls before
// it is consumed, so this never piles up or blocks a write-tool handler.
func (a *App) SignalSync() {
	select {
	case a.SyncTrigger <- struct{}{}:
	default:
	}
}

// Open resolves identity, opens the per-repo store at the global cache path (which
// also runs migrations), builds the engine + ledger, and upserts repo metadata.
// The git identity is resolved exactly once here and cached on App, so a
// long-lived `serve` process resolves git a single time (spec §6).
func Open(cfg Config) (*App, error) {
	runner, gc, err := resolvePrimary(cfg)
	if err != nil {
		return nil, err
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	path, err := cache.StorePath(gc.RepoID)
	if err != nil {
		return nil, err
	}
	storeOpts := storeOptions(cfg)
	st, err := store.Open(path, storeOpts...)
	if err != nil {
		return nil, err
	}
	led := ledger.New(st)
	eng := memory.New(st, led, clk)
	gr, err := graph.New(st, led, clk)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	eng.SetRefResolver(graphRefResolver{st: st})
	syncer, _ := remotesync.New(remotesync.ConfigFromEnv(), gc, eng, st)
	var ws *workspace.Bundle
	if !cfg.SkipWorkspace {
		if ws, err = workspace.Open(clk, storeOpts...); err != nil {
			gr.Close()
			_ = st.Close()
			return nil, err
		}
	}

	a := &App{
		Git: gc, Runner: runner, Store: st, Ledger: led, Engine: eng, Graph: gr, Clock: clk,
		Sync: syncer, Workspace: ws, Repos: openRepos(gc.RepoID, clk, storeOpts...), SyncTrigger: make(chan struct{}, 1),
		Features: features.Load(),
	}
	if err := a.writeMeta(context.Background()); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

// resolvePrimary resolves the identity of cfg's working directory, returning the
// git runner it used.
func resolvePrimary(cfg Config) (gitctx.Runner, gitctx.Context, error) {
	cwd := cfg.CWD
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, gitctx.Context{}, err
		}
		cwd = wd
	}
	if cfg.Runner != nil {
		gc, err := gitctx.Resolve(cfg.Runner, cwd)
		return cfg.Runner, gc, err
	}
	gc, err := resolveReal(cwd)
	return gitctx.Real{Dir: cwd}, gc, err
}

// Worktrees returns the worktree roots an App opened with cfg would serve —
// the primary's, then the RECARTO_REPOS repos' — resolving identity as Open
// does but opening no store, so a hook can tell a file outside every served
// repo before paying for one. It may list a repo Open then fails to open,
// never miss one.
func Worktrees(cfg Config) ([]string, error) {
	_, gc, err := resolvePrimary(cfg)
	if err != nil {
		return nil, err
	}
	out := []string{gc.Worktree}
	for _, p := range filepath.SplitList(os.Getenv("RECARTO_REPOS")) {
		if p == "" {
			continue
		}
		if gc, err := resolveReal(p); err == nil && !gc.NonGit {
			out = append(out, gc.Worktree)
		}
	}
	return out, nil
}

// resolveReal resolves dir's identity with real git, through the cross-process
// identity cache, so a hook skips the git spawns while the checkout is
// unchanged. An injected test Runner bypasses it (Open calls Resolve).
func resolveReal(dir string) (gitctx.Context, error) {
	cacheDir, _ := cache.Dir() // "" disables the cache
	return gitctx.ResolveCached(cacheDir, gitctx.Real{Dir: dir}, dir)
}

// storeOptions maps the App config onto the store-level options every store
// this process opens should share.
func storeOptions(cfg Config) []store.Option {
	if cfg.QuickCheck {
		return []store.Option{store.WithQuickCheck()}
	}
	return nil
}

// Close releases the graph engine's parser, the workspace store, the extra
// repo stores, then the primary store.
func (a *App) Close() error {
	if a.Graph != nil {
		a.Graph.Close()
	}
	if a.Workspace != nil {
		_ = a.Workspace.Close()
	}
	for _, rc := range a.Repos {
		if rc.Graph != nil {
			rc.Graph.Close()
		}
		_ = rc.Store.Close()
	}
	return a.Store.Close()
}

// AllRepos returns the primary (cwd-resolved) repo context followed by the
// RECARTO_REPOS extras — the iteration order every merged surface uses, so
// the primary always leads.
func (a *App) AllRepos() []*RepoContext {
	prim := &RepoContext{Git: a.Git, Store: a.Store, Ledger: a.Ledger, Engine: a.Engine, Graph: a.Graph, Sync: a.Sync}
	return append([]*RepoContext{prim}, a.Repos...)
}

// HasSync reports whether any syncer — the repo stores' or the workspace
// store's — is configured, i.e. whether a background sync loop has work.
func (a *App) HasSync() bool {
	if a.Sync != nil || (a.Workspace != nil && a.Workspace.Sync != nil) {
		return true
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			return true
		}
	}
	return false
}

// SyncInterval returns the poll period for a long-lived process's background
// sync loop (mcp.Serve). Every syncer is built from the same
// RECARTO_SYNC_INTERVAL, so the first configured one speaks for all; the
// point of asking App rather than a.Sync directly is that the PRIMARY syncer
// is nil whenever the cwd is not a git repo (no cross-machine identity) even
// while the workspace or RECARTO_REPOS syncers exist — exactly the state
// HasSync reports true for.
func (a *App) SyncInterval() time.Duration {
	if a.Sync != nil {
		return a.Sync.Interval()
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			return rc.Sync.Interval()
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		return a.Workspace.Sync.Interval()
	}
	return remotesync.DefaultInterval
}

// SyncAll runs one best-effort pull+push cycle on every configured syncer.
// Each store's cycle is independent: a failure of one never blocks the others,
// and any may be absent.
func (a *App) SyncAll(ctx context.Context) {
	if a.Sync != nil {
		_ = a.Sync.Once(ctx)
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			_ = rc.Sync.Once(ctx)
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		_ = a.Workspace.Sync.Once(ctx)
	}
}

// openRepos resolves RECARTO_REPOS (a filepath.SplitList of repo roots) into
// served repo contexts. Best-effort by design: a path that is missing (an
// unmounted host's config), not a git repo, unresolvable, or a duplicate of
// the primary or an earlier entry is skipped — an incomplete list must never
// fail the session.
func openRepos(primaryID string, clk clock.Clock, storeOpts ...store.Option) []*RepoContext {
	seen := map[string]bool{primaryID: true}
	var out []*RepoContext
	for _, p := range filepath.SplitList(os.Getenv("RECARTO_REPOS")) {
		if p == "" {
			continue
		}
		gc, err := resolveReal(p)
		if err != nil || gc.NonGit || seen[gc.RepoID] {
			continue
		}
		path, err := cache.StorePath(gc.RepoID)
		if err != nil {
			continue
		}
		st, err := store.Open(path, storeOpts...)
		if err != nil {
			continue
		}
		led := ledger.New(st)
		eng := memory.New(st, led, clk)
		gr, err := graph.New(st, led, clk)
		if err != nil {
			_ = st.Close()
			continue
		}
		eng.SetRefResolver(graphRefResolver{st: st})
		syncer, _ := remotesync.New(remotesync.ConfigFromEnv(), gc, eng, st)
		seen[gc.RepoID] = true
		out = append(out, &RepoContext{Git: gc, Store: st, Ledger: led, Engine: eng, Graph: gr, Sync: syncer})
	}
	return out
}

// writeMeta upserts repo identity metadata. It is idempotent and safe under
// concurrency (the store serializes writers; the UPSERT is atomic). Every open
// runs it, so it first reads the rows and writes only on drift: an unchanged
// store is then opened without taking the write lock — and a hook that only
// reads never makes SQLite fsync and checkpoint the WAL on close.
func (a *App) writeMeta(ctx context.Context) error {
	want := map[string]string{
		"repo_name":       a.Git.RepoName,
		"repo_id":         a.Git.RepoID,
		"root_commit":     a.Git.RootCommit,
		"recarto_version": version.Version,
	}
	if rows, err := a.Store.DB.QueryContext(ctx,
		`SELECT key, value FROM meta WHERE key IN ('repo_name', 'repo_id', 'root_commit', 'recarto_version')`); err == nil {
		same := 0
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil && want[k] == v {
				same++
			}
		}
		if rows.Close() == nil && rows.Err() == nil && same == len(want) {
			return nil
		}
	}
	return a.Store.WriteTx(ctx, func(c store.Conn) error {
		for k, v := range want {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				k, v); err != nil {
				return err
			}
		}
		return nil
	})
}
