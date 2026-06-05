package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckpointTruncatesWAL: after a run of committed writes grows the WAL,
// Checkpoint flushes it into the main db and truncates the WAL file, bounding
// on-disk growth during a long index.
func TestCheckpointTruncatesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	for i := range 300 {
		if err := st.WriteTx(ctx, func(c Conn) error {
			_, err := c.ExecContext(ctx,
				`INSERT INTO meta(key,value) VALUES (?,?)`, fmt.Sprintf("k%d", i), "v")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	walPath := path + "-wal"
	before := walSize(t, walPath)
	if before == 0 {
		t.Fatalf("expected WAL to have grown before checkpoint, got 0")
	}
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if after := walSize(t, walPath); after != 0 {
		t.Errorf("WAL not truncated: before=%d after=%d", before, after)
	}
}

func walSize(t *testing.T, p string) int64 {
	t.Helper()
	info, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
