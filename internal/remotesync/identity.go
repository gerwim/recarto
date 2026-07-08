package remotesync

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
)

// RepoKey resolves the cross-machine sync identity for gc. gitctx.RepoID
// hashes the machine-local absolute path to .git, so two clones of the same
// repo on different machines (or a fresh ForgeBoard microVM) get DIFFERENT
// RepoIDs — it cannot key a shared server. RootCommit is identical across
// every clone of the same repo, so it is used instead.
//
// Resolution order:
//  1. RECARTO_SYNC_REPO env override, verbatim — for shallow clones (no
//     reachable root commit), repos with multiple roots, or intentional
//     grouping of otherwise-unrelated repos onto one sync key.
//  2. sha256(RootCommit)[:16] when RootCommit is non-empty.
//  3. Neither: ok=false. Sync stays disabled rather than guessing a
//     machine-local identity that would defeat the whole point.
func RepoKey(gc gitctx.Context) (string, bool) {
	if v := os.Getenv("RECARTO_SYNC_REPO"); v != "" {
		return v, true
	}
	if gc.RootCommit == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(gc.RootCommit))
	return hex.EncodeToString(sum[:])[:16], true
}
