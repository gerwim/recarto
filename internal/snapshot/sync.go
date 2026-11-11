package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/gerwim/recarto/internal/memory"
	"github.com/gerwim/recarto/internal/store"
)

const metaSnapshotHash = "snapshot_hash"

// SyncOnSessionStart imports <repoRoot>/.recarto/memory.jsonl when its content
// hash differs from meta.snapshot_hash, then records the new hash. It returns the
// number of newly-inserted memories. A missing file is not an error (returns 0).
// Best-effort: callers ignore the error so a malformed snapshot never blocks a
// session.
func SyncOnSessionStart(ctx context.Context, eng *memory.Engine, st *store.Store, repoRoot string) (int, error) {
	path := filepath.Join(repoRoot, ".recarto", "memory.jsonl")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	var prev string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key=?`, metaSnapshotHash).Scan(&prev); err == nil {
		if prev == hash {
			return 0, nil // unchanged since last import
		}
	}

	ins, _, _, ierr := Import(ctx, eng, bytes.NewReader(data))
	if ierr != nil {
		return 0, ierr
	}
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		_, e := c.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaSnapshotHash, hash)
		return e
	}); err != nil {
		return ins, err
	}
	return ins, nil
}
