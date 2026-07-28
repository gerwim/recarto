// Package workspace opens Recarto's optional cross-repo workspace store: a
// second memory store shared by every repo worked on under the same
// RECARTO_WORKSPACE name. Like the global feedback store it is keyed by a
// reserved store id and is independent of the current git repo or working
// directory — so it works identically from a repo worktree, a scratch dir, or
// no checkout at all. Repo stores keep the code graph and repo-anchored
// memories; the workspace store holds what spans repos: preferences, org-wide
// decisions, cross-repo knowledge.
//
// Workspace memories are branch-less by construction (branches are a repo
// concept), and they are surfaced at session start and via recall/list — never
// by the file-anchored just-in-time hooks, whose repo-relative paths are
// ambiguous across repos.
package workspace

import (
	"os"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/remotesync"
	"git.gerwim.nl/gerwim/recarto/internal/repokey"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// EnvVar names the environment variable that enables the workspace store. Its
// value is an operator-chosen workspace name; every process sharing the same
// name (and cache dir / sync server) shares the same store.
const EnvVar = "RECARTO_WORKSPACE"

// StorePrefix is the reserved-store-id prefix for workspace stores. Real repo
// ids are git-hash derived hex, so a leading-underscore sentinel cannot collide
// with one (same convention as the feedback store).
const StorePrefix = "_ws_"

// Name returns the configured workspace name, or "" when the feature is off.
func Name() string { return strings.TrimSpace(os.Getenv(EnvVar)) }

// StoreID derives the store id — used both as the local cache directory name
// and as the HTTP-sync bucket key — for a workspace name. Hashing keeps an
// arbitrary operator-chosen name filesystem- and URL-safe, and using the SAME
// id for both means every machine configured with the same name converges on
// the same sync bucket with no extra configuration.
func StoreID(name string) string { return StorePrefix + repokey.Hash(name) }

// Bundle is an open workspace store with its engine and optional syncer.
type Bundle struct {
	Name   string
	Store  *store.Store
	Engine *memory.Engine
	// Sync is nil when HTTP sync is unconfigured — same optionality contract
	// as app.App.Sync: every caller must nil-check.
	Sync *remotesync.Syncer
}

// Open opens (creating and migrating on first use) the workspace store named
// by RECARTO_WORKSPACE, or (nil, nil) when the env var is unset — the feature
// is entirely opt-in and its absence is never an error. storeOpts are passed
// through to store.Open, so a long-lived serving process can request the
// proactive corruption check (store.WithQuickCheck) for this store too.
func Open(clk clock.Clock, storeOpts ...store.Option) (*Bundle, error) {
	name := Name()
	if name == "" {
		return nil, nil
	}
	if clk == nil {
		clk = clock.Real{}
	}
	id := StoreID(name)
	path, err := cache.StorePath(id)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path, storeOpts...)
	if err != nil {
		return nil, err
	}
	eng := memory.New(st, ledger.New(st), clk)
	b := &Bundle{Name: name, Store: st, Engine: eng}
	b.Sync, _ = remotesync.NewWithKey(remotesync.ConfigFromEnv(), id, eng, st)
	return b, nil
}

// Close releases the underlying store.
func (b *Bundle) Close() error { return b.Store.Close() }
