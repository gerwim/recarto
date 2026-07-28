// Package feedback opens Recarto's global, cross-repo "feedback about Recarto
// itself" store. It is keyed by a reserved repo-id and is independent of the
// current git repo or working directory, so reports written from any repo collect
// in one queue and `recarto feedback` reads them from anywhere.
package feedback

import (
	"git.gerwim.nl/gerwim/recarto/internal/cache"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// StoreID is the reserved repo-id for the global feedback store. Real repo-ids are
// git-hash derived, so a leading-underscore sentinel cannot collide with one.
const StoreID = "_recarto_feedback"

// RepoRefPrefix prefixes the source-repo tag ref carried on a feedback report, so
// reinforcement unions the set of repos a report was seen in. The report handler
// writes RepoRefPrefix+<repo>; the `recarto feedback` CLI strips it for display.
const RepoRefPrefix = "repo:"

// Bundle is an open feedback store and its memory engine. Close it when done.
type Bundle struct {
	Store  *store.Store
	Engine *memory.Engine
}

// Open opens — creating and migrating on first use — the global feedback store.
// It ignores the working directory and git: the path is fixed under the cache dir.
// storeOpts reach store.Open unchanged; the MCP report handler passes
// store.WithQuickCheck because it runs inside a long-lived serving process, and
// this store is opened nowhere else in that process (so it would otherwise never
// get a proactive check).
func Open(clk clock.Clock, storeOpts ...store.Option) (*Bundle, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	path, err := cache.StorePath(StoreID)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path, storeOpts...)
	if err != nil {
		return nil, err
	}
	led := ledger.New(st)
	eng := memory.New(st, led, clk)
	return &Bundle{Store: st, Engine: eng}, nil
}

// Close releases the underlying store.
func (b *Bundle) Close() error { return b.Store.Close() }
