// Package app wires Recarto's per-process runtime in one place: it resolves the
// git/worktree identity, opens the shared per-repo store, and constructs the
// memory engine and ledger. Every surface (the CLI now; the MCP server and hooks
// in Phase 1C-2) builds an App and works through it, so the wiring lives here and
// nowhere else.
package app

import (
	"context"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/graph"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// Config configures App construction. The zero value is valid: it resolves the
// real working directory, real git, and the real clock.
type Config struct {
	CWD    string        // "" → os.Getwd()
	Runner gitctx.Runner // nil → gitctx.Real{Dir: CWD}
	Clock  clock.Clock   // nil → clock.Real{}
}

// App is the resolved per-process runtime shared by every surface.
type App struct {
	Git    gitctx.Context
	Store  *store.Store
	Ledger *ledger.Ledger
	Engine *memory.Engine
	Graph  *graph.Engine
	Clock  clock.Clock
}

// Open resolves identity, opens the per-repo store at the global cache path (which
// also runs migrations), builds the engine + ledger, and upserts repo metadata.
// The git identity is resolved exactly once here and cached on App, so a
// long-lived `serve` process resolves git a single time (spec §6).
func Open(cfg Config) (*App, error) {
	cwd := cfg.CWD
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = wd
	}
	runner := cfg.Runner
	if runner == nil {
		runner = gitctx.Real{Dir: cwd}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	gc, err := gitctx.Resolve(runner, cwd)
	if err != nil {
		return nil, err
	}
	path, err := cache.StorePath(gc.RepoID)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path)
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

	a := &App{Git: gc, Store: st, Ledger: led, Engine: eng, Graph: gr, Clock: clk}
	if err := a.writeMeta(context.Background()); err != nil {
		_ = st.Close()
		return nil, err
	}
	return a, nil
}

// Close releases the graph engine's parser then the store.
func (a *App) Close() error {
	if a.Graph != nil {
		a.Graph.Close()
	}
	return a.Store.Close()
}

// writeMeta upserts repo identity metadata. It is idempotent and safe under
// concurrency (the store serializes writers; the UPSERT is atomic).
func (a *App) writeMeta(ctx context.Context) error {
	return a.Store.WriteTx(ctx, func(c store.Conn) error {
		for _, kv := range [][2]string{
			{"repo_name", a.Git.RepoName},
			{"repo_id", a.Git.RepoID},
			{"root_commit", a.Git.RootCommit},
			{"recarto_version", version.Version},
		} {
			if _, err := c.ExecContext(ctx,
				`INSERT INTO meta(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
}
