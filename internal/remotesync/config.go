// Package remotesync is the client half of Recarto's HTTP memory sync: it
// pulls remote changes into the local store (via memory.SyncApply) and pushes
// local changes out (via memory.PendingPush/PendingTombstones), so the same
// git repo worked on from multiple machines converges its memories without
// ever sharing the derived, per-worktree code graph. See internal/syncserver
// for the server half and internal/syncwire for the shared wire format.
//
// The whole feature is dormant unless RECARTO_SYNC_URL and RECARTO_SYNC_KEY
// are both set: Config.Enabled() gates every entry point, so an unconfigured
// install behaves exactly as it did before this package existed.
package remotesync

import (
	"os"
	"strconv"
	"time"
)

// Config is resolved once per process from the environment.
type Config struct {
	URL      string        // RECARTO_SYNC_URL, e.g. "https://recarto-sync.internal"
	Key      string        // RECARTO_SYNC_KEY: static bearer token, shared by every machine
	Interval time.Duration // RECARTO_SYNC_INTERVAL (seconds); default 60s — long-lived `serve` poll period
	Timeout  time.Duration // RECARTO_SYNC_TIMEOUT (seconds); default 5s — per-request HTTP timeout
}

// DefaultInterval is the poll period used when RECARTO_SYNC_INTERVAL is unset.
// Exported so callers that need an interval without a constructed Syncer (a
// serve process whose primary repo has no syncer) share the same default.
const DefaultInterval = 60 * time.Second

const defaultTimeout = 5 * time.Second

// ConfigFromEnv reads Config from the process environment.
func ConfigFromEnv() Config {
	return Config{
		URL:      os.Getenv("RECARTO_SYNC_URL"),
		Key:      os.Getenv("RECARTO_SYNC_KEY"),
		Interval: durationEnvSeconds("RECARTO_SYNC_INTERVAL", DefaultInterval),
		Timeout:  durationEnvSeconds("RECARTO_SYNC_TIMEOUT", defaultTimeout),
	}
}

// Enabled reports whether sync has enough configuration to run at all. The
// repo identity (RepoKey) is checked separately, since it depends on git state
// Config alone does not carry.
func (c Config) Enabled() bool { return c.URL != "" && c.Key != "" }

func durationEnvSeconds(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
