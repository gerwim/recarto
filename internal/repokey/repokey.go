// Package repokey computes the sha256-derived HTTP-sync bucket key from a
// git root commit. It is split out from internal/remotesync (whose RepoKey
// uses it) purely to avoid an import cycle: internal/syncserver depends on
// internal/fullexport (for the bulk-import archive reader), and
// internal/remotesync's own tests depend on internal/syncserver (to run a
// real server in-process) — so internal/fullexport, which must derive the
// identical key remotesync does, cannot import internal/remotesync itself.
package repokey

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash derives the sha256(rootCommit)[:16] server bucket key — the same
// formula for every caller (remotesync.RepoKey, fullexport's per-store key
// resolution), so a client push and a bulk import of the same repo always
// land in the same bucket.
func Hash(rootCommit string) string {
	sum := sha256.Sum256([]byte(rootCommit))
	return hex.EncodeToString(sum[:])[:16]
}
